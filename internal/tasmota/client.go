// Package tasmota talks to a single Tasmota device over its HTTP API: the /cm
// command endpoint, the /dl configuration download and the /u2 configuration
// upload. It knows nothing about inventories or scheduling; see internal/fleet.
package tasmota

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Auth is a device's web credentials (Tasmota WebPassword). An empty Password
// means none is sent.
type Auth struct {
	Username string
	Password string
}

// ErrUnauthorized is returned when the device demands a web password that was
// not supplied or did not match.
var ErrUnauthorized = errors.New("device requires a web password (WebPassword) and the supplied one was refused")

// ErrNotTasmota is returned when a host answers HTTP but not like Tasmota.
var ErrNotTasmota = errors.New("host answered, but not with the Tasmota HTTP API")

// Client issues requests to devices. The zero value is not usable; use New.
type Client struct {
	http *http.Client
}

// New returns a client whose requests time out after timeout. Redirects are
// not followed: a captive portal or a non-Tasmota web server must not be
// mistaken for a device.
func New(timeout time.Duration) *Client {
	transport := &http.Transport{
		Proxy:               nil, // devices are on the LAN; never go through a proxy
		DialContext:         (&net.Dialer{Timeout: timeout}).DialContext,
		MaxIdleConnsPerHost: 2,
		IdleConnTimeout:     30 * time.Second,
		// Tasmota's web server is tiny; keep the body ceiling low so a bad
		// peer cannot make the poller buffer megabytes.
		DisableCompression: true,
	}
	return &Client{http: &http.Client{
		Timeout:   timeout,
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}}
}

// maxBody bounds any response body read from a device. Status 0 on a sensor
// build is a few KB; a configuration dump is 4 KB (ESP8266) or 8 KB (ESP32).
const maxBody = 256 << 10

func baseURL(host string) string {
	if strings.Contains(host, "://") {
		return strings.TrimRight(host, "/")
	}
	return "http://" + host
}

// Command sends one Tasmota command and returns the JSON reply verbatim.
// Backlog commands return {} immediately; their effects show on the next poll.
func (c *Client) Command(ctx context.Context, host string, auth Auth, cmd string) (json.RawMessage, error) {
	q := url.Values{}
	q.Set("cmnd", cmd)
	if auth.Password != "" {
		q.Set("user", auth.Username)
		q.Set("password", auth.Password)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL(host)+"/cm?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, ErrUnauthorized
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: HTTP %d", host, resp.StatusCode)
	}
	body = bytes.TrimSpace(body)
	if !json.Valid(body) {
		return nil, ErrNotTasmota
	}
	// A protected device answers 200 with this warning instead of a 401.
	var warn struct {
		Warning string `json:"WARNING"`
	}
	if json.Unmarshal(body, &warn) == nil && strings.HasPrefix(warn.Warning, "Need user=") {
		return nil, ErrUnauthorized
	}
	return json.RawMessage(body), nil
}

// Status0 runs `Status 0` and parses it.
func (c *Client) Status0(ctx context.Context, host string, auth Auth) (Status, json.RawMessage, error) {
	raw, err := c.Command(ctx, host, auth, "Status 0")
	if err != nil {
		return Status{}, nil, err
	}
	st, err := ParseStatus0(raw)
	if err != nil {
		return Status{}, raw, fmt.Errorf("%w: %v", ErrNotTasmota, err)
	}
	return st, raw, nil
}

// Backup downloads the device's configuration (/dl). It returns the filename
// the device suggests (Config_<name>_<version>.dmp) and the bytes.
func (c *Client) Backup(ctx context.Context, host string, auth Auth) (string, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL(host)+"/dl", nil)
	if err != nil {
		return "", nil, err
	}
	c.setPageAuth(req, auth)
	resp, err := c.http.Do(req)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return "", nil, ErrUnauthorized
	}
	if resp.StatusCode != http.StatusOK {
		return "", nil, fmt.Errorf("%s: /dl HTTP %d", host, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return "", nil, err
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/octet-stream") {
		return "", nil, fmt.Errorf("%w: /dl returned %q", ErrNotTasmota, ct)
	}
	name := "Config.dmp"
	if _, params, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition")); err == nil && params["filename"] != "" {
		name = params["filename"]
	}
	return name, data, nil
}

// Restore uploads a configuration dump (/u2). The device validates the dump,
// applies it and restarts; the call returns once the upload page answers.
func (c *Client) Restore(ctx context.Context, host string, auth Auth, filename string, data []byte) error {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("u2", filename)
	if err != nil {
		return err
	}
	if _, err := part.Write(data); err != nil {
		return err
	}
	if err := mw.Close(); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL(host)+"/u2", &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	c.setPageAuth(req, auth)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if resp.StatusCode == http.StatusUnauthorized {
		return ErrUnauthorized
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: /u2 HTTP %d", host, resp.StatusCode)
	}
	// Tasmota renders the result as a page; a failed restore says so in it.
	if b := strings.ToLower(string(body)); strings.Contains(b, "failed") || strings.Contains(b, "invalid") {
		return fmt.Errorf("%s: device refused the configuration file", host)
	}
	return nil
}

// setPageAuth applies the web password the way the HTML pages expect it:
// HTTP basic auth, plus the query form the /cm endpoint uses, so both code
// paths in the firmware are satisfied.
func (c *Client) setPageAuth(req *http.Request, auth Auth) {
	if auth.Password == "" {
		return
	}
	req.SetBasicAuth(auth.Username, auth.Password)
	q := req.URL.Query()
	q.Set("user", auth.Username)
	q.Set("password", auth.Password)
	req.URL.RawQuery = q.Encode()
}
