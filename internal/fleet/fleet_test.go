package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rake-pro/tasmonaut/internal/config"
)

// fakeDevice serves the Tasmota HTTP API from a fixture, with a mutable
// relay state and firmware version so upgrade and power flows can be driven.
type fakeDevice struct {
	mu      sync.Mutex
	status  map[string]any
	power   string
	version string
	ota     string
	cmds    []string
	srv     *httptest.Server
}

func newFakeDevice(t *testing.T, fixture string) *fakeDevice {
	t.Helper()
	b, err := os.ReadFile("../tasmota/testdata/" + fixture)
	if err != nil {
		t.Fatal(err)
	}
	fd := &fakeDevice{power: "ON", version: "15.6.0(release-tasmota)"}
	if err := json.Unmarshal(b, &fd.status); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/cm", fd.cm)
	mux.HandleFunc("/dl", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", `attachment; filename=Config_test_15.6.0.dmp`)
		_, _ = w.Write(make([]byte, 4096))
	})
	fd.srv = httptest.NewServer(mux)
	t.Cleanup(fd.srv.Close)
	return fd
}

func (fd *fakeDevice) host() string { return strings.TrimPrefix(fd.srv.URL, "http://") }

func (fd *fakeDevice) cm(w http.ResponseWriter, r *http.Request) {
	cmd := r.URL.Query().Get("cmnd")
	fd.mu.Lock()
	defer fd.mu.Unlock()
	fd.cmds = append(fd.cmds, cmd)
	switch {
	case cmd == "Status 0":
		// Patch the fixture with the mutable state.
		st := fd.status
		st["StatusFWR"].(map[string]any)["Version"] = fd.version
		st["StatusSTS"].(map[string]any)["POWER"] = fd.power
		_ = json.NewEncoder(w).Encode(st)
	case strings.HasPrefix(cmd, "Power"):
		arg := strings.ToUpper(strings.TrimSpace(strings.TrimPrefix(cmd, "Power1")))
		switch arg {
		case "ON":
			fd.power = "ON"
		case "OFF":
			fd.power = "OFF"
		case "TOGGLE":
			if fd.power == "ON" {
				fd.power = "OFF"
			} else {
				fd.power = "ON"
			}
		}
		_, _ = w.Write([]byte(`{"POWER":"` + fd.power + `"}`))
	case strings.HasPrefix(cmd, "OtaUrl "):
		fd.ota = strings.TrimPrefix(cmd, "OtaUrl ")
		_, _ = w.Write([]byte(`{"OtaUrl":"` + fd.ota + `"}`))
	case cmd == "Upgrade 1":
		// Flash "instantly": the next Status 0 reports the new version.
		fd.version = "16.0.0(release-tasmota)"
		_, _ = w.Write([]byte(`{"Upgrade":"Successful. Restarting"}`))
	default:
		_, _ = w.Write([]byte(`{"Command":"Unknown"}`))
	}
}

func testConfig(hosts ...string) config.Config {
	cfg := config.Defaults()
	cfg.Firmware.CheckLatest = false
	cfg.Firmware.Latest = "16.0.0"
	cfg.Poll.Interval = 0
	cfg.Discovery.Interval = 0
	for _, h := range hosts {
		cfg.Devices = append(cfg.Devices, config.Device{Host: h})
	}
	return cfg
}

func TestConfiguredDeviceRekeysByMAC(t *testing.T) {
	fd := newFakeDevice(t, "status0_s31.json")
	cfg := testConfig(fd.host())
	cfg.Devices[0].Name = "Desk plug"
	f := New(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.Start(ctx)

	devs := f.Devices()
	if len(devs) != 1 {
		t.Fatalf("devices: %d", len(devs))
	}
	d := devs[0]
	if d.ID != "deadbeef0001" || d.Name != "Desk plug" || d.Source != SourceConfig || !d.Online {
		t.Errorf("device: %+v", d)
	}
	if !d.UpdateAvailable {
		t.Error("15.6.0 vs pinned 16.0.0 should be an update")
	}
	if _, ok := f.Get("host-" + strings.ReplaceAll(strings.ReplaceAll(fd.host(), ".", "-"), ":", "-")); ok {
		t.Error("provisional host id still present after re-key")
	}
}

func TestOfflineDeviceStaysListed(t *testing.T) {
	// A port nothing listens on.
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := l.Addr().String()
	_ = l.Close()
	cfg := testConfig(addr)
	cfg.Poll.Timeout = 500 * time.Millisecond
	f := New(cfg)
	f.RefreshAll(context.Background())
	d := f.Devices()[0]
	if d.Online || d.LastError == "" || d.Name != addr {
		t.Errorf("offline device: %+v", d)
	}
}

func TestPowerCommandAndBackup(t *testing.T) {
	fd := newFakeDevice(t, "status0_s31.json")
	f := New(testConfig(fd.host()))
	ctx := context.Background()
	f.RefreshAll(ctx)
	id := f.Devices()[0].ID

	on, err := f.SetPower(ctx, id, 1, "TOGGLE")
	if err != nil || on {
		t.Fatalf("toggle: on=%v err=%v", on, err)
	}
	d, _ := f.Get(id)
	if d.Status.Relays[0].On {
		t.Error("status not refreshed after power change")
	}
	if _, err := f.SetPower(ctx, id, 1, "maybe"); err == nil {
		t.Error("bad state accepted")
	}
	raw, err := f.Command(ctx, id, "Status 1")
	if err != nil || !strings.Contains(string(raw), "Unknown") {
		t.Errorf("command: %s %v", raw, err)
	}
	name, data, err := f.Backup(ctx, id)
	if err != nil || name != "Config_test_15.6.0.dmp" || len(data) != 4096 {
		t.Errorf("backup: %q %d %v", name, len(data), err)
	}
	if _, err := f.Command(ctx, "nope", "Status"); err != ErrNotFound {
		t.Errorf("unknown id: %v", err)
	}
}

func TestDiscoveryAndAddRemove(t *testing.T) {
	fd := newFakeDevice(t, "status0_bulb.json")
	cfg := testConfig()
	f := New(cfg)
	ctx := context.Background()
	if len(f.Devices()) != 0 {
		t.Fatal("inventory should start empty")
	}
	d, err := f.Add(ctx, fd.host())
	if err != nil {
		t.Fatal(err)
	}
	if d.Source != SourceManual || d.ID != "deadbeef0003" || d.Name != "patio-light" {
		t.Errorf("added: %+v", d)
	}
	// Adding the same host again merges, no duplicate.
	if _, err := f.Add(ctx, fd.host()); err != nil {
		t.Fatal(err)
	}
	if len(f.Devices()) != 1 {
		t.Errorf("duplicate after re-add: %d", len(f.Devices()))
	}
	if _, err := f.Add(ctx, "127.0.0.1:1"); err == nil {
		t.Error("unreachable host added")
	}
	if err := f.Remove(d.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.Remove(d.ID); err != ErrNotFound {
		t.Errorf("second remove: %v", err)
	}
}

func TestBulkJobsAndUpgrade(t *testing.T) {
	a := newFakeDevice(t, "status0_s31.json")
	b := newFakeDevice(t, "status0_bulb.json")
	f := New(testConfig(a.host(), b.host()))
	ctx := context.Background()
	f.RefreshAll(ctx)
	var ids []string
	for _, d := range f.Devices() {
		ids = append(ids, d.ID)
	}
	if len(ids) != 2 {
		t.Fatalf("ids: %v", ids)
	}

	job, err := f.BulkPower(ids, 1, "OFF")
	if err != nil {
		t.Fatal(err)
	}
	waitJob(t, f, job.ID)
	j, _ := f.Job(job.ID)
	if ok, failed := j.Counts(); ok != 2 || failed != 0 {
		t.Errorf("power job: %+v", j.Items)
	}
	for _, d := range f.Devices() {
		if d.Status.Relays[0].On {
			t.Errorf("%s still on after bulk off", d.Name)
		}
	}

	job, err = f.BulkCommand(ids, "SetOption65 1")
	if err != nil {
		t.Fatal(err)
	}
	waitJob(t, f, job.ID)
	if _, err := f.BulkCommand(ids, "  "); err == nil {
		t.Error("empty command accepted")
	}
	if _, err := f.BulkPower([]string{"ghost"}, 1, "ON"); err == nil {
		t.Error("unknown id accepted")
	}

	job, err = f.BulkUpgrade(ids[:1], "http://ota.example/tasmota.bin.gz")
	if err != nil {
		t.Fatal(err)
	}
	// The fake flashes instantly; the poll loop checks every upgradePoll.
	waitJobFor(t, f, job.ID, upgradePoll+5*time.Second)
	j, _ = f.Job(job.ID)
	if ok, failed := j.Counts(); ok != 1 || failed != 0 {
		t.Fatalf("upgrade job: %+v", j.Items[0])
	}
	if !strings.Contains(j.Items[0].Message, "16.0.0") {
		t.Errorf("message: %s", j.Items[0].Message)
	}
	// The upgraded device must have been told the OtaUrl first.
	for _, fd := range []*fakeDevice{a, b} {
		fd.mu.Lock()
		cmds := strings.Join(fd.cmds, "|")
		ota := fd.ota
		fd.mu.Unlock()
		if strings.Contains(cmds, "Upgrade 1") && ota != "http://ota.example/tasmota.bin.gz" {
			t.Errorf("OtaUrl not set before upgrade: %s", cmds)
		}
	}
	if len(f.Jobs()) != 3 {
		t.Errorf("jobs listed: %d", len(f.Jobs()))
	}
}

func TestBackupZip(t *testing.T) {
	a := newFakeDevice(t, "status0_s31.json")
	f := New(testConfig(a.host(), "127.0.0.1:1"))
	f.cfg.Poll.Timeout = 500 * time.Millisecond
	f.client = newClientFor(f.cfg.Poll.Timeout)
	ctx := context.Background()
	f.RefreshAll(ctx)
	var ids []string
	for _, d := range f.Devices() {
		ids = append(ids, d.ID)
	}
	var buf strings.Builder
	if err := f.BackupZip(ctx, ids, &writerAdapter{&buf}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "Config_test_15.6.0.dmp") || !strings.Contains(out, "FAILED.txt") {
		t.Error("zip should hold the good dump and a FAILED.txt for the dead host")
	}
}

type writerAdapter struct{ b *strings.Builder }

func (w *writerAdapter) Write(p []byte) (int, error) { return w.b.Write(p) }

func waitJob(t *testing.T, f *Fleet, id string) { waitJobFor(t, f, id, 10*time.Second) }

func waitJobFor(t *testing.T, f *Fleet, id string, max time.Duration) {
	t.Helper()
	deadline := time.Now().Add(max)
	for time.Now().Before(deadline) {
		if j, ok := f.Job(id); ok && j.Done() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("job %s did not finish within %s", id, max)
}

// newTestMirror points f's mirror at a TLS server standing in for GitHub's
// release downloads and returns the paths it was asked for.
func newTestMirror(t *testing.T, f *Fleet, h http.HandlerFunc) *[]string {
	t.Helper()
	var mu sync.Mutex
	var paths []string
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		h(w, r)
	}))
	t.Cleanup(up.Close)
	f.mirror.base = up.URL + "/dl"
	f.mirror.http = up.Client()
	return &paths
}

func TestMirror(t *testing.T) {
	f := New(testConfig())
	var plain *httptest.Server
	paths := newTestMirror(t, f, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/dl/v16.0.0/tasmota.bin.gz":
			_, _ = w.Write([]byte("image-8266"))
		case "/dl/v16.0.0/tasmota32.bin":
			// GitHub answers with a redirect to its asset host.
			http.Redirect(w, r, "/asset/tasmota32", http.StatusFound)
		case "/asset/tasmota32":
			_, _ = w.Write([]byte("image-32"))
		case "/dl/v16.0.0/tasmota-downgrade.bin":
			http.Redirect(w, r, plain.URL+"/evil.bin", http.StatusFound)
		case "/dl/v16.0.0/tasmota-huge.bin":
			_, _ = w.Write(make([]byte, maxImageBytes+1))
		default:
			http.NotFound(w, r)
		}
	})
	plainHit := false
	plain = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		plainHit = true
		_, _ = w.Write([]byte("tampered"))
	}))
	t.Cleanup(plain.Close)
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		b, tag, err := f.FirmwareImage(ctx, "tasmota.bin.gz")
		if err != nil || string(b) != "image-8266" || tag != "v16.0.0" {
			t.Fatalf("image: %q %q %v", b, tag, err)
		}
	}
	if len(*paths) != 1 {
		t.Errorf("second request must come from memory: %v", *paths)
	}
	if b, _, err := f.FirmwareImage(ctx, "tasmota32.bin"); err != nil || string(b) != "image-32" {
		t.Errorf("https redirect not followed: %q %v", b, err)
	}
	// A redirect out of HTTPS must fail without the plain server being asked.
	if _, _, err := f.FirmwareImage(ctx, "tasmota-downgrade.bin"); err == nil || !strings.Contains(err.Error(), "not HTTPS") || plainHit {
		t.Errorf("redirect to http: err=%v plainHit=%v", err, plainHit)
	}
	if _, _, err := f.FirmwareImage(ctx, "tasmota-huge.bin"); err == nil {
		t.Error("oversized image accepted")
	}
	// A name the release does not have is remembered: asking again must not
	// cost another round trip upstream.
	before := len(*paths)
	for i := 0; i < 3; i++ {
		if _, _, err := f.FirmwareImage(ctx, "tasmota-zz.bin.gz"); !errors.Is(err, ErrNoImage) {
			t.Errorf("image missing upstream: %v", err)
		}
	}
	if n := len(*paths) - before; n != 1 {
		t.Errorf("missing image asked upstream %d times, want 1", n)
	}
	before = len(*paths)
	for _, name := range []string{"", "..", "../v15.6.0/tasmota.bin", "tasmota.bin/../x", "tasmota.bin?x=1", "config.dmp", "tasmota.factory.bin", "TASMOTA.bin", "tasmota.bin.gz.sig", "tasmota%2f.bin"} {
		if _, _, err := f.FirmwareImage(ctx, name); !errors.Is(err, ErrNoImage) {
			t.Errorf("%q: %v", name, err)
		}
	}
	if len(*paths) != before {
		t.Errorf("an invalid name reached upstream: %v", (*paths)[before:])
	}

	// The cache stays bounded however many distinct images are asked for.
	g := New(testConfig())
	newTestMirror(t, g, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("x")) })
	for i := 0; i < maxMirrored+5; i++ {
		if _, _, err := g.FirmwareImage(ctx, fmt.Sprintf("tasmota-v%d.bin", i)); err != nil {
			t.Fatal(err)
		}
	}
	if len(g.mirror.images) != maxMirrored || len(g.mirror.order) != maxMirrored {
		t.Errorf("cache holds %d images / %d keys, want %d", len(g.mirror.images), len(g.mirror.order), maxMirrored)
	}

	// Concurrent requests share one download, a slow download does not delay
	// an image already in memory, and a caller that gives up is released.
	s := New(testConfig())
	release := make(chan struct{})
	slowPaths := newTestMirror(t, s, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/tasmota-slow.bin") {
			<-release
		}
		_, _ = w.Write([]byte("x"))
	})
	if _, _, err := s.FirmwareImage(ctx, "tasmota.bin.gz"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := s.FirmwareImage(ctx, "tasmota-slow.bin")
			errs <- err
		}()
	}
	short, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	if _, _, err := s.FirmwareImage(short, "tasmota-slow.bin"); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a waiter past its deadline must be released: %v", err)
	}
	cancel()
	done := make(chan struct{})
	go func() { _, _, _ = s.FirmwareImage(ctx, "tasmota.bin.gz"); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Error("a cached image waited behind a slow download")
	}
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("shared download: %v", err)
		}
	}
	if len(*slowPaths) != 2 {
		t.Errorf("want one download per image, upstream saw %v", *slowPaths)
	}

	// Without a reference release there is nothing to mirror.
	cfg := testConfig()
	cfg.Firmware.Latest = ""
	if _, _, err := New(cfg).FirmwareImage(ctx, "tasmota.bin.gz"); !errors.Is(err, ErrNoRelease) {
		t.Errorf("no release: %v", err)
	}
}

func TestUpgradeThroughMirror(t *testing.T) {
	a := newFakeDevice(t, "status0_s31.json")
	b := newFakeDevice(t, "status0_esp32_4relay.json")
	b.version = "15.6.0(release-tasmota32)"
	cfg := testConfig(a.host(), b.host())
	cfg.Firmware.Mirror = config.Mirror{Listen: ":8081", URL: "http://10.0.2.10:8081/"}
	f := New(cfg)
	newTestMirror(t, f, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/dl/v16.0.0/tasmota.bin.gz" {
			_, _ = w.Write([]byte("image"))
			return
		}
		http.NotFound(w, r)
	})
	f.RefreshAll(context.Background())
	var ids []string
	for _, d := range f.Devices() {
		ids = append(ids, d.ID)
	}
	job, err := f.BulkUpgrade(ids, "")
	if err != nil {
		t.Fatal(err)
	}
	waitJobFor(t, f, job.ID, upgradePoll+5*time.Second)
	j, _ := f.Job(job.ID)
	if ok, failed := j.Counts(); ok != 1 || failed != 1 {
		t.Fatalf("want the ESP8266 upgraded and the ESP32 refused: %+v %+v", j.Items[0], j.Items[1])
	}
	a.mu.Lock()
	ota := a.ota
	a.mu.Unlock()
	if ota != "http://10.0.2.10:8081/firmware/tasmota.bin.gz" {
		t.Errorf("OtaUrl sent to the device: %q", ota)
	}
	// The mirror has no tasmota32.bin: the device must not be told to upgrade
	// from a URL that cannot answer, and its OtaUrl must be left alone.
	b.mu.Lock()
	cmds := strings.Join(b.cmds, "|")
	b.mu.Unlock()
	if strings.Contains(cmds, "Upgrade 1") || strings.Contains(cmds, "OtaUrl ") {
		t.Errorf("ESP32 was commanded although its image is not mirrored: %s", cmds)
	}
	if f.Firmware().Mirror != cfg.Firmware.Mirror.URL {
		t.Errorf("firmware.mirror not reported: %+v", f.Firmware())
	}

	// The device is now on the release the mirror serves: a second run must
	// refuse it rather than reflash, and send it nothing.
	a.mu.Lock()
	a.cmds = nil
	a.mu.Unlock()
	job, err = f.BulkUpgrade(ids[:1], "")
	if err != nil {
		t.Fatal(err)
	}
	waitJob(t, f, job.ID)
	j, _ = f.Job(job.ID)
	a.mu.Lock()
	cmds = strings.Join(a.cmds, "|")
	a.mu.Unlock()
	if _, failed := j.Counts(); failed != 1 || !strings.Contains(j.Items[0].Message, "already on") || cmds != "" {
		t.Errorf("up-to-date device: %+v, commands %q", j.Items[0], cmds)
	}
}
