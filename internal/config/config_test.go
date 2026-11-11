package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadFileAndEnv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "c.yaml")
	if err := os.WriteFile(path, []byte(`
listen: ":9000"
identityHeader: X-Forwarded-User
devices:
  - host: 10.0.4.5
    name: Plug
discovery:
  cidrs: ["10.0.4.0/24"]
poll:
  interval: 30s
firmware:
  latest: "16.0.0"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvPrefix+"LISTEN", ":9001")
	t.Setenv(EnvPrefix+"DISCOVERY_CIDRS", "10.0.4.0/25, 10.0.5.0/26")
	t.Setenv(EnvPrefix+"POLL_PARALLEL", "4")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != ":9001" || cfg.IdentityHeader != "X-Forwarded-User" {
		t.Errorf("env should win, file should apply: %q %q", cfg.Listen, cfg.IdentityHeader)
	}
	if len(cfg.Discovery.CIDRs) != 2 || cfg.Discovery.CIDRs[1] != "10.0.5.0/26" {
		t.Errorf("cidrs: %v", cfg.Discovery.CIDRs)
	}
	if cfg.Poll.Interval != 30*time.Second || cfg.Poll.Parallel != 4 || cfg.Poll.Timeout != 5*time.Second {
		t.Errorf("poll: %+v", cfg.Poll)
	}
	if len(cfg.Devices) != 1 || cfg.Devices[0].Name != "Plug" {
		t.Errorf("devices: %+v", cfg.Devices)
	}
}

func TestValidate(t *testing.T) {
	cfg := Defaults()
	if err := cfg.Validate(); err != nil {
		t.Errorf("defaults must validate: %v", err)
	}
	cfg.Discovery.CIDRs = []string{"10.0.0.0/8", "nope", "fd00::/64"}
	err := cfg.Validate()
	for _, want := range []string{"spans", "not a CIDR", "IPv4"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("cidr validation missing %q: %v", want, err)
		}
	}
	cfg.Discovery.CIDRs = []string{"10.0.4.0/22"}
	cfg.Firmware.Latest = "sixteen"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "firmware.latest") {
		t.Errorf("bad pinned version: %v", err)
	}
}

func TestValidateMirror(t *testing.T) {
	ok := Mirror{Listen: ":8081", URL: "http://10.0.2.10:8081"}
	for name, tc := range map[string]struct {
		mirror Mirror
		otaURL string
		want   string
	}{
		"off":            {Mirror{}, "", ""},
		"on":             {ok, "", ""},
		"trailing slash": {Mirror{Listen: ":8081", URL: "http://fw.example.lan/"}, "", ""},
		"listen only":    {Mirror{Listen: ":8081"}, "", "set together"},
		"url only":       {Mirror{URL: ok.URL}, "", "set together"},
		"main listener":  {Mirror{Listen: ":8080", URL: ok.URL}, "", "must differ"},
		"https":          {Mirror{Listen: ":8081", URL: "https://10.0.2.10:8081"}, "", "bare http://"},
		"path":           {Mirror{Listen: ":8081", URL: "http://10.0.2.10:8081/fw"}, "", "bare http://"},
		"no scheme":      {Mirror{Listen: ":8081", URL: "10.0.2.10:8081"}, "", "bare http://"},
		"empty query":    {Mirror{Listen: ":8081", URL: "http://10.0.2.10:8081/?"}, "", "bare http://"},
		"fragment":       {Mirror{Listen: ":8081", URL: "http://10.0.2.10:8081#"}, "", "bare http://"},
		"escaped path":   {Mirror{Listen: ":8081", URL: "http://10.0.2.10:8081/%2F"}, "", "bare http://"},
		"userinfo":       {Mirror{Listen: ":8081", URL: "http://u@10.0.2.10:8081"}, "", "bare http://"},
		"with otaUrl":    {ok, "http://ota.example/tasmota.bin.gz", "set one"},
	} {
		cfg := Defaults()
		cfg.Firmware.Mirror, cfg.Firmware.OtaURL = tc.mirror, tc.otaURL
		err := cfg.Validate()
		if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("%s: want %q, got %v", name, tc.want, err)
		}
	}
	t.Setenv("TASMONAUT_FIRMWARE_MIRROR_LISTEN", ":8081")
	t.Setenv("TASMONAUT_FIRMWARE_MIRROR_URL", ok.URL)
	cfg, err := Load("")
	if err != nil || cfg.Firmware.Mirror != ok {
		t.Errorf("env: %+v %v", cfg.Firmware.Mirror, err)
	}
}

func TestBadEnvValues(t *testing.T) {
	t.Setenv(EnvPrefix+"DISCOVERY_AT_STARTUP", "yes please")
	t.Setenv(EnvPrefix+"POLL_INTERVAL", "soon")
	_, err := Load("")
	if err == nil || !strings.Contains(err.Error(), "not a boolean") || !strings.Contains(err.Error(), "not a duration") {
		t.Errorf("bad env values: %v", err)
	}
}

func TestHosts(t *testing.T) {
	hs, err := Hosts("10.0.4.0/30")
	if err != nil || len(hs) != 2 || hs[0] != "10.0.4.1" || hs[1] != "10.0.4.2" {
		t.Errorf("/30: %v %v", hs, err)
	}
	hs, _ = Hosts("10.0.4.0/24")
	if len(hs) != 254 || hs[253] != "10.0.4.254" {
		t.Errorf("/24: %d %s", len(hs), hs[len(hs)-1])
	}
	hs, _ = Hosts("10.0.4.7/32")
	if len(hs) != 1 || hs[0] != "10.0.4.7" {
		t.Errorf("/32: %v", hs)
	}
	hs, _ = Hosts("10.0.4.77/24")
	if hs[0] != "10.0.4.1" {
		t.Errorf("unmasked input: %v", hs[0])
	}
}
