package tasmota

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseStatus0S31(t *testing.T) {
	st, err := ParseStatus0(fixture(t, "status0_s31.json"))
	if err != nil {
		t.Fatal(err)
	}
	if st.DeviceName != "desk-plug" || st.IP != "10.0.4.21" || st.MAC != "DE:AD:BE:EF:00:01" {
		t.Errorf("identity: %+v", st)
	}
	if st.Version.Short() != "15.6.0" || st.Version.Variant != "release-tasmota" || st.Version.IsESP32() {
		t.Errorf("version: %+v", st.Version)
	}
	if len(st.Relays) != 1 || st.Relays[0].Index != 1 || !st.Relays[0].On || st.Relays[0].Name != "Desk Plug" {
		t.Errorf("relays: %+v", st.Relays)
	}
	if st.Energy == nil || st.Energy.Power != 62 || st.Energy.Voltage != 121 {
		t.Errorf("energy: %+v", st.Energy)
	}
	if st.Wifi.RSSI != 100 || st.Wifi.Signal >= 0 || st.Wifi.Channel != 11 {
		t.Errorf("wifi: %+v", st.Wifi)
	}
	if st.OtaURL != "http://ota.tasmota.com/tasmota/release/tasmota.bin.gz" {
		t.Errorf("ota: %q", st.OtaURL)
	}
	if st.Dimmer != -1 {
		t.Errorf("dimmer on a plug: %d", st.Dimmer)
	}
}

func TestParseStatus0FourRelay(t *testing.T) {
	st, err := ParseStatus0(fixture(t, "status0_esp32_4relay.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !st.Version.IsESP32() || st.Hardware == "" {
		t.Errorf("esp32 detection: %+v", st.Version)
	}
	if len(st.Relays) != 4 {
		t.Fatalf("relays: %+v", st.Relays)
	}
	for i, r := range st.Relays {
		if r.Index != i+1 || r.On {
			t.Errorf("relay %d: %+v", i, r)
		}
	}
	if st.Relays[3].Name != "Pump - Quick Clean" {
		t.Errorf("relay names: %+v", st.Relays)
	}
	if st.Energy != nil {
		t.Errorf("no energy expected: %+v", st.Energy)
	}
}

func TestParseStatus0Bulb(t *testing.T) {
	st, err := ParseStatus0(fixture(t, "status0_bulb.json"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Dimmer != 100 || len(st.Relays) != 1 || st.Relays[0].On {
		t.Errorf("bulb: dimmer=%d relays=%+v", st.Dimmer, st.Relays)
	}
}

func TestParseStatus0Rejects(t *testing.T) {
	if _, err := ParseStatus0([]byte(`{"hello":"world"}`)); err == nil {
		t.Error("non-Tasmota JSON accepted")
	}
	if _, err := ParseStatus0([]byte(`<html>`)); err == nil {
		t.Error("HTML accepted")
	}
}

func TestVersion(t *testing.T) {
	cases := []struct {
		in       string
		short    string
		variant  string
		safeboot bool
	}{
		{"15.6.0(release-tasmota)", "15.6.0", "release-tasmota", false},
		{"12.3.1(safeboot)", "12.3.1", "safeboot", true},
		{"v16.0.0", "16.0.0", "", false},
	}
	for _, c := range cases {
		v, err := ParseVersion(c.in)
		if err != nil {
			t.Fatalf("%s: %v", c.in, err)
		}
		if v.Short() != c.short || v.Variant != c.variant || v.Safeboot() != c.safeboot {
			t.Errorf("%s: %+v", c.in, v)
		}
	}
	a, _ := ParseVersion("15.6.0(release-tasmota)")
	b, _ := ParseVersion("16.0.0")
	if a.Compare(b) >= 0 || b.Compare(a) <= 0 || a.Compare(a) != 0 {
		t.Error("compare")
	}
	if _, err := ParseVersion("latest"); err == nil {
		t.Error("garbage accepted")
	}
}

func TestParsePowerReply(t *testing.T) {
	idx, on, err := ParsePowerReply([]byte(`{"POWER":"ON"}`))
	if err != nil || idx != 1 || !on {
		t.Errorf("single: %d %v %v", idx, on, err)
	}
	idx, on, err = ParsePowerReply([]byte(`{"POWER3":"OFF"}`))
	if err != nil || idx != 3 || on {
		t.Errorf("multi: %d %v %v", idx, on, err)
	}
	if _, _, err := ParsePowerReply([]byte(`{"Command":"Unknown"}`)); err == nil {
		t.Error("unknown accepted")
	}
}

// fakeDevice is an httptest server speaking enough of the Tasmota HTTP API
// for the client tests: /cm with an optional password, /dl, /u2.
func fakeDevice(t *testing.T, password string) *httptest.Server {
	t.Helper()
	status0 := fixture(t, "status0_s31.json")
	mux := http.NewServeMux()
	authorized := func(r *http.Request) bool {
		if password == "" {
			return true
		}
		if u, p, ok := r.BasicAuth(); ok && u == "admin" && p == password {
			return true
		}
		return r.URL.Query().Get("user") == "admin" && r.URL.Query().Get("password") == password
	}
	mux.HandleFunc("/cm", func(w http.ResponseWriter, r *http.Request) {
		if !authorized(r) {
			_, _ = w.Write([]byte(`{"WARNING":"Need user=<username>&password=<password>"}`))
			return
		}
		switch cmd := r.URL.Query().Get("cmnd"); {
		case cmd == "Status 0":
			_, _ = w.Write(status0)
		case strings.HasPrefix(cmd, "Power"):
			_, _ = w.Write([]byte(`{"POWER":"OFF"}`))
		default:
			_, _ = w.Write([]byte(`{"Command":"Unknown"}`))
		}
	})
	mux.HandleFunc("/dl", func(w http.ResponseWriter, r *http.Request) {
		if !authorized(r) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", "attachment; filename=Config_desk_plug_15.6.0.dmp")
		_, _ = w.Write(make([]byte, 4096))
	})
	mux.HandleFunc("/u2", func(w http.ResponseWriter, r *http.Request) {
		if !authorized(r) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		f, hdr, err := r.FormFile("u2")
		if err != nil {
			http.Error(w, "no file", 400)
			return
		}
		defer f.Close()
		if hdr.Size != 4096 {
			_, _ = w.Write([]byte("<html>Upload failed: invalid file</html>"))
			return
		}
		_, _ = w.Write([]byte("<html>Upload successful. Restarting</html>"))
	})
	return httptest.NewServer(mux)
}

func TestClientRoundTrip(t *testing.T) {
	srv := fakeDevice(t, "")
	defer srv.Close()
	c := New(2 * time.Second)
	ctx := context.Background()
	st, raw, err := c.Status0(ctx, srv.URL, Auth{})
	if err != nil {
		t.Fatal(err)
	}
	if st.DeviceName != "desk-plug" || !json.Valid(raw) {
		t.Errorf("status: %+v", st)
	}
	name, data, err := c.Backup(ctx, srv.URL, Auth{})
	if err != nil || name != "Config_desk_plug_15.6.0.dmp" || len(data) != 4096 {
		t.Errorf("backup: %q %d %v", name, len(data), err)
	}
	if err := c.Restore(ctx, srv.URL, Auth{}, name, data); err != nil {
		t.Errorf("restore: %v", err)
	}
	if err := c.Restore(ctx, srv.URL, Auth{}, name, data[:10]); err == nil {
		t.Error("short restore accepted")
	}
}

func TestClientPassword(t *testing.T) {
	srv := fakeDevice(t, "s3cret")
	defer srv.Close()
	c := New(2 * time.Second)
	ctx := context.Background()
	if _, _, err := c.Status0(ctx, srv.URL, Auth{}); err != ErrUnauthorized {
		t.Errorf("no password: %v", err)
	}
	if _, _, err := c.Status0(ctx, srv.URL, Auth{Username: "admin", Password: "s3cret"}); err != nil {
		t.Errorf("with password: %v", err)
	}
	if _, _, err := c.Backup(ctx, srv.URL, Auth{}); err != ErrUnauthorized {
		t.Errorf("backup no password: %v", err)
	}
	if _, _, err := c.Backup(ctx, srv.URL, Auth{Username: "admin", Password: "s3cret"}); err != nil {
		t.Errorf("backup with password: %v", err)
	}
}

func TestClientNotTasmota(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<html>nginx</html>"))
	}))
	defer srv.Close()
	c := New(2 * time.Second)
	if _, err := c.Command(context.Background(), srv.URL, Auth{}, "Status 0"); err != ErrNotTasmota {
		t.Errorf("want ErrNotTasmota, got %v", err)
	}
}
