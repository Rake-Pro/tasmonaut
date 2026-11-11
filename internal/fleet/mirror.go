package fleet

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
)

// mirrorUpstream is where release images come from. ESP8266 builds of Tasmota
// can only download an OTA image over plain HTTP, so the mirror does the
// internet leg itself over verified HTTPS and the devices only ever speak
// HTTP to tasmonaut on the local network.
const mirrorUpstream = "https://github.com/arendst/Tasmota/releases/download"

const (
	// maxImageBytes bounds one image; the largest OTA image in 16.0.0 is 2.7 MB.
	maxImageBytes = 8 << 20
	// maxMirrored bounds the cache. The mirror listener is unauthenticated, so
	// a client asking for every asset of a release must not grow it further.
	maxMirrored = 8
	// maxMissing bounds the remembered "not in this release" answers, which
	// keep repeated requests for a missing name from each costing a round trip
	// upstream.
	maxMissing = 256
)

var (
	// ErrNoImage means the name is not an OTA image of the reference release.
	ErrNoImage = errors.New("not a firmware image of this release")
	// ErrNoRelease means there is no reference release to mirror yet.
	ErrNoRelease = errors.New("no reference release known (firmware.checkLatest failed and firmware.latest is not set)")
)

// imageName is the shape of a release OTA image: tasmota.bin.gz,
// tasmota-minimal.bin.gz, tasmota32.bin, tasmota32-safeboot.bin and so on.
// It is one path segment by construction, which is what keeps a request to
// the mirror from reaching any other upstream path.
var imageName = regexp.MustCompile(`^tasmota[0-9A-Za-z-]{0,40}\.bin(\.gz)?$`)

// mirror fetches release images over HTTPS and keeps the last few in memory.
type mirror struct {
	base string
	http *http.Client

	// mu guards the maps only; it is never held across a download.
	mu      sync.Mutex
	images  map[string][]byte
	order   []string
	missing map[string]struct{}
	// pending holds the downloads in flight: four devices asking for the same
	// image share one, and a slow download never delays another image.
	pending map[string]*download
}

type download struct {
	done chan struct{}
	b    []byte
	err  error
}

func newMirror() *mirror {
	return &mirror{
		base:    mirrorUpstream,
		http:    &http.Client{Timeout: 60 * time.Second},
		images:  make(map[string][]byte),
		missing: make(map[string]struct{}),
		pending: make(map[string]*download),
	}
}

// httpsOnly refuses a redirect that would leave HTTPS. GitHub redirects a
// release download to its asset host; that hop must stay encrypted too.
func httpsOnly(req *http.Request, via []*http.Request) error {
	if req.URL.Scheme != "https" {
		return fmt.Errorf("refusing redirect to %s://%s: not HTTPS", req.URL.Scheme, req.URL.Host)
	}
	if len(via) >= 5 {
		return errors.New("stopped after 5 redirects")
	}
	return nil
}

// image returns one image of the release tagged tag, from memory or upstream.
func (m *mirror) image(ctx context.Context, tag, name string) ([]byte, error) {
	if !imageName.MatchString(name) {
		return nil, fmt.Errorf("%q: %w", name, ErrNoImage)
	}
	key := tag + "/" + name
	m.mu.Lock()
	if b, ok := m.images[key]; ok {
		m.mu.Unlock()
		return b, nil
	}
	if _, ok := m.missing[key]; ok {
		m.mu.Unlock()
		return nil, fmt.Errorf("%s is not in release %s: %w", name, tag, ErrNoImage)
	}
	d, ok := m.pending[key]
	if !ok {
		d = &download{done: make(chan struct{})}
		m.pending[key] = d
		go func() {
			// Not the caller's context: the other waiters must not lose the
			// download because the first requester went away. The client
			// timeout bounds it.
			d.b, d.err = m.fetch(context.WithoutCancel(ctx), tag, name)
			m.mu.Lock()
			delete(m.pending, key)
			switch {
			case d.err == nil:
				m.images[key] = d.b
				m.order = append(m.order, key)
				if len(m.order) > maxMirrored {
					delete(m.images, m.order[0])
					m.order = m.order[1:]
				}
			case errors.Is(d.err, ErrNoImage):
				if len(m.missing) >= maxMissing {
					clear(m.missing)
				}
				m.missing[key] = struct{}{}
			}
			m.mu.Unlock()
			close(d.done)
		}()
	}
	m.mu.Unlock()
	select {
	case <-d.done:
		return d.b, d.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// fetch downloads one image from upstream.
func (m *mirror) fetch(ctx context.Context, tag, name string) ([]byte, error) {
	src := m.base + "/" + url.PathEscape(tag) + "/" + name
	if !strings.HasPrefix(src, "https://") {
		return nil, fmt.Errorf("mirror upstream %s is not HTTPS", m.base)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "tasmonaut")
	client := *m.http
	client.CheckRedirect = httpsOnly
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%s is not in release %s: %w", name, tag, ErrNoImage)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %s: HTTP %d", name, resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxImageBytes+1))
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", name, err)
	}
	if len(b) == 0 || len(b) > maxImageBytes {
		return nil, fmt.Errorf("download %s: %d bytes is not a plausible image", name, len(b))
	}
	log.Info().Str("image", name).Str("tag", tag).Int("bytes", len(b)).Msg("firmware image mirrored")
	return b, nil
}

// FirmwareImage returns a release image of the reference version and its tag.
// It is what the mirror listener serves to the devices.
func (f *Fleet) FirmwareImage(ctx context.Context, name string) ([]byte, string, error) {
	fw := f.fw.get()
	if !fw.known() {
		return nil, "", ErrNoRelease
	}
	// Built from the parsed version, not the raw upstream tag string.
	tag := "v" + fw.Version.Short()
	b, err := f.mirror.image(ctx, tag, name)
	return b, tag, err
}

// mirrorURL makes sure the mirror holds the image d's own OtaUrl names and
// returns the mirror URL for it. Keeping the file name is what lets one
// mirror serve ESP8266 and ESP32 devices, and any build variant.
func (f *Fleet) mirrorURL(ctx context.Context, d *Device) (string, error) {
	f.mu.RLock()
	ota := ""
	if d.Status != nil {
		ota = d.Status.OtaURL
	}
	f.mu.RUnlock()
	if ota == "" {
		return "", errors.New("firmware mirror: the device's OtaUrl is not known yet; refresh it first")
	}
	u, err := url.Parse(ota)
	if err != nil {
		return "", fmt.Errorf("firmware mirror: device OtaUrl %q: %w", ota, err)
	}
	name := path.Base(u.Path)
	if _, _, err := f.FirmwareImage(ctx, name); err != nil {
		return "", fmt.Errorf("firmware mirror: %w", err)
	}
	return strings.TrimSuffix(f.cfg.Firmware.Mirror.URL, "/") + "/firmware/" + name, nil
}
