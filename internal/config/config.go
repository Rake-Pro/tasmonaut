// Package config loads tasmonaut's configuration from an optional YAML file
// overlaid by TASMONAUT_* environment variables. The environment wins so an
// orchestrator's injected values stay authoritative over a checked-in file.
//
// tasmonaut is deliberately stateless: everything here is the whole
// configuration, there is no database and nothing is written to disk. The
// device inventory is the configured list plus whatever discovery finds, held
// in memory for the life of the process.
package config

import (
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// EnvPrefix is the prefix of every environment override.
const EnvPrefix = "TASMONAUT_"

// maxHostsPerCIDR bounds a discovery sweep: a /22 is 1022 hosts and already
// takes a while at the default concurrency; anything wider is a typo.
const maxHostsPerCIDR = 1024

// Config is the resolved configuration.
type Config struct {
	Listen   string `yaml:"listen"`
	LogLevel string `yaml:"logLevel"`

	// IdentityHeader names the request header the authenticating reverse proxy
	// sets with the signed-in user (Authentik's forward auth sends
	// X-Authentik-Username). tasmonaut has no login of its own: it must run
	// behind such a proxy, and this header is only used for display and logs.
	IdentityHeader string `yaml:"identityHeader"`

	Devices   []Device  `yaml:"devices"`
	Discovery Discovery `yaml:"discovery"`
	Poll      Poll      `yaml:"poll"`
	Tasmota   Tasmota   `yaml:"tasmota"`
	Firmware  Firmware  `yaml:"firmware"`
}

// Device is a statically configured device. Host is an IP or a resolvable
// name; Name overrides the device's own DeviceName in the UI; Username and
// Password override the fleet-wide web credentials for this device only.
type Device struct {
	Host     string `yaml:"host"`
	Name     string `yaml:"name"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

// Discovery sweeps the listed CIDRs for devices that answer the Tasmota HTTP
// API. Found devices join the in-memory inventory next to the configured ones.
type Discovery struct {
	CIDRs     []string      `yaml:"cidrs"`
	AtStartup bool          `yaml:"atStartup"`
	Interval  time.Duration `yaml:"interval"`
	Timeout   time.Duration `yaml:"timeout"`
	Parallel  int           `yaml:"parallel"`
}

// Poll controls the background status refresh of known devices.
type Poll struct {
	Interval time.Duration `yaml:"interval"`
	Timeout  time.Duration `yaml:"timeout"`
	Parallel int           `yaml:"parallel"`
}

// Tasmota holds the fleet-wide web credentials (Tasmota's WebPassword). Empty
// password means the devices are unprotected, which is the firmware default.
type Tasmota struct {
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

// Firmware controls the "update available" comparison and the OTA default.
type Firmware struct {
	// CheckLatest fetches the newest Tasmota release tag from GitHub hourly.
	// Off, the Latest value below (if any) is the only reference.
	CheckLatest bool `yaml:"checkLatest"`
	// Latest pins the reference version ("16.0.0") instead of asking GitHub.
	Latest string `yaml:"latest"`
	// OtaURL, when set, is sent as the device's OtaUrl before every upgrade.
	// Empty keeps each device's own OtaUrl, which is what the stock release
	// channel needs (ESP8266 and ESP32 devices use different URLs).
	OtaURL string `yaml:"otaUrl"`
}

// Defaults is the configuration before any file or environment is applied.
func Defaults() Config {
	return Config{
		Listen:         ":8080",
		LogLevel:       "info",
		IdentityHeader: "X-Authentik-Username",
		Discovery:      Discovery{AtStartup: true, Interval: 15 * time.Minute, Timeout: 3 * time.Second, Parallel: 64},
		Poll:           Poll{Interval: 60 * time.Second, Timeout: 5 * time.Second, Parallel: 16},
		Tasmota:        Tasmota{Username: "admin"},
		Firmware:       Firmware{CheckLatest: true},
	}
}

// Load reads the YAML file at path (or $TASMONAUT_CONFIG when path is empty;
// no file at all is fine), overlays the environment and validates the result.
func Load(path string) (Config, error) {
	cfg := Defaults()
	if path == "" {
		path = os.Getenv(EnvPrefix + "CONFIG")
	}
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return cfg, fmt.Errorf("read config: %w", err)
		}
		dec := yaml.NewDecoder(strings.NewReader(string(b)))
		dec.KnownFields(true)
		if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
			return cfg, fmt.Errorf("parse config %s: %w", path, err)
		}
	}
	if err := applyEnv(&cfg); err != nil {
		return cfg, err
	}
	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func applyEnv(cfg *Config) error {
	str := func(key string, dst *string) {
		if v, ok := os.LookupEnv(EnvPrefix + key); ok {
			*dst = v
		}
	}
	list := func(key string, dst *[]string) {
		if v, ok := os.LookupEnv(EnvPrefix + key); ok {
			*dst = splitList(v)
		}
	}
	var err error
	boolean := func(key string, dst *bool) {
		v, ok := os.LookupEnv(EnvPrefix + key)
		if !ok {
			return
		}
		b, perr := strconv.ParseBool(v)
		if perr != nil {
			err = errors.Join(err, fmt.Errorf("%s%s: %q is not a boolean", EnvPrefix, key, v))
			return
		}
		*dst = b
	}
	dur := func(key string, dst *time.Duration) {
		v, ok := os.LookupEnv(EnvPrefix + key)
		if !ok {
			return
		}
		d, perr := time.ParseDuration(v)
		if perr != nil {
			err = errors.Join(err, fmt.Errorf("%s%s: %q is not a duration", EnvPrefix, key, v))
			return
		}
		*dst = d
	}
	integer := func(key string, dst *int) {
		v, ok := os.LookupEnv(EnvPrefix + key)
		if !ok {
			return
		}
		n, perr := strconv.Atoi(v)
		if perr != nil {
			err = errors.Join(err, fmt.Errorf("%s%s: %q is not an integer", EnvPrefix, key, v))
			return
		}
		*dst = n
	}

	str("LISTEN", &cfg.Listen)
	str("LOG_LEVEL", &cfg.LogLevel)
	str("IDENTITY_HEADER", &cfg.IdentityHeader)

	// TASMONAUT_DEVICES is a comma-separated host list; it replaces the file's
	// list (with no per-device names or credentials) rather than merging.
	if v, ok := os.LookupEnv(EnvPrefix + "DEVICES"); ok {
		cfg.Devices = nil
		for _, h := range splitList(v) {
			cfg.Devices = append(cfg.Devices, Device{Host: h})
		}
	}
	list("DISCOVERY_CIDRS", &cfg.Discovery.CIDRs)
	boolean("DISCOVERY_AT_STARTUP", &cfg.Discovery.AtStartup)
	dur("DISCOVERY_INTERVAL", &cfg.Discovery.Interval)
	dur("DISCOVERY_TIMEOUT", &cfg.Discovery.Timeout)
	integer("DISCOVERY_PARALLEL", &cfg.Discovery.Parallel)
	dur("POLL_INTERVAL", &cfg.Poll.Interval)
	dur("POLL_TIMEOUT", &cfg.Poll.Timeout)
	integer("POLL_PARALLEL", &cfg.Poll.Parallel)
	str("DEVICE_USERNAME", &cfg.Tasmota.Username)
	str("DEVICE_PASSWORD", &cfg.Tasmota.Password)
	boolean("FIRMWARE_CHECK_LATEST", &cfg.Firmware.CheckLatest)
	str("FIRMWARE_LATEST", &cfg.Firmware.Latest)
	str("FIRMWARE_OTA_URL", &cfg.Firmware.OtaURL)
	return err
}

func splitList(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Validate checks the cross-field rules. Errors lead with the field path so
// the operator can find the line.
func (c Config) Validate() error {
	var errs []error
	if c.Listen == "" {
		errs = append(errs, errors.New("listen: must not be empty"))
	}
	for i, d := range c.Devices {
		if strings.TrimSpace(d.Host) == "" {
			errs = append(errs, fmt.Errorf("devices[%d].host: must not be empty", i))
		}
	}
	for i, cidr := range c.Discovery.CIDRs {
		p, err := netip.ParsePrefix(cidr)
		if err != nil {
			errs = append(errs, fmt.Errorf("discovery.cidrs[%d]: %q is not a CIDR (like 10.0.4.0/24)", i, cidr))
			continue
		}
		if !p.Addr().Is4() {
			errs = append(errs, fmt.Errorf("discovery.cidrs[%d]: %q: only IPv4 ranges are swept", i, cidr))
			continue
		}
		if n := hostCount(p); n > maxHostsPerCIDR {
			errs = append(errs, fmt.Errorf("discovery.cidrs[%d]: %q spans %d hosts; the limit is %d (use a /22 or smaller)", i, cidr, n, maxHostsPerCIDR))
		}
	}
	if c.Discovery.Interval < 0 || c.Poll.Interval < 0 {
		errs = append(errs, errors.New("discovery.interval and poll.interval: must be zero (off) or positive"))
	}
	if c.Discovery.Timeout <= 0 || c.Poll.Timeout <= 0 {
		errs = append(errs, errors.New("discovery.timeout and poll.timeout: must be positive"))
	}
	if c.Discovery.Parallel <= 0 || c.Poll.Parallel <= 0 {
		errs = append(errs, errors.New("discovery.parallel and poll.parallel: must be positive"))
	}
	if c.Firmware.Latest != "" {
		if _, err := ParseVersion(c.Firmware.Latest); err != nil {
			errs = append(errs, fmt.Errorf("firmware.latest: %w", err))
		}
	}
	return errors.Join(errs...)
}

// hostCount returns the number of usable host addresses in an IPv4 prefix.
func hostCount(p netip.Prefix) int {
	bits := 32 - p.Bits()
	if bits >= 31 {
		return 1 << 31
	}
	n := 1 << bits
	if bits <= 1 {
		return n
	}
	return n - 2
}

// Hosts expands an IPv4 prefix into its usable host addresses (network and
// broadcast skipped for /30 and wider).
func Hosts(cidr string) ([]string, error) {
	p, err := netip.ParsePrefix(cidr)
	if err != nil {
		return nil, err
	}
	p = p.Masked()
	n := hostCount(p)
	addr := p.Addr()
	if p.Bits() <= 30 {
		addr = addr.Next()
	}
	out := make([]string, 0, n)
	for i := 0; i < n && p.Contains(addr); i++ {
		out = append(out, addr.String())
		addr = addr.Next()
	}
	return out, nil
}

// ParseVersion parses "16.0.0" (a leading "v" is accepted) into three ints.
func ParseVersion(s string) ([3]int, error) {
	var v [3]int
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return v, fmt.Errorf("%q is not a version like 16.0.0", s)
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return v, fmt.Errorf("%q is not a version like 16.0.0", s)
		}
		v[i] = n
	}
	return v, nil
}
