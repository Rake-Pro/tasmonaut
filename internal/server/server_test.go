package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/rake-pro/tasmonaut/internal/config"
	"github.com/rake-pro/tasmonaut/internal/fleet"
)

// newTestServer runs the whole handler with auth off against one fake device.
func newTestServer(t *testing.T) (http.Handler, string) {
	t.Helper()
	status0, err := os.ReadFile("../tasmota/testdata/status0_s31.json")
	if err != nil {
		t.Fatal(err)
	}
	dev := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/cm" && r.URL.Query().Get("cmnd") == "Status 0":
			_, _ = w.Write(status0)
		case r.URL.Path == "/cm":
			_, _ = w.Write([]byte(`{"POWER":"OFF"}`))
		case r.URL.Path == "/dl":
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Disposition", `attachment; filename=Config_x.dmp`)
			_, _ = w.Write(make([]byte, 16))
		}
	}))
	t.Cleanup(dev.Close)
	cfg := config.Defaults()
	cfg.Firmware.CheckLatest = false
	cfg.Firmware.Latest = "16.0.0"
	cfg.Poll.Interval, cfg.Discovery.Interval = 0, 0
	cfg.Devices = []config.Device{{Host: strings.TrimPrefix(dev.URL, "http://"), Name: "Test plug"}}
	fl := fleet.New(cfg)
	fl.RefreshAll(context.Background())
	h, err := New(fl, "test", "X-Authentik-Username")
	if err != nil {
		t.Fatal(err)
	}
	return h, fl.Devices()[0].ID
}

func get(h http.Handler, method, path string, body string) *httptest.ResponseRecorder {
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func TestPagesRender(t *testing.T) {
	h, id := newTestServer(t)
	for _, p := range []string{"/", "/jobs", "/devices/" + id} {
		rec := get(h, "GET", p, "")
		if rec.Code != 200 {
			t.Errorf("%s: %d", p, rec.Code)
		}
		body := rec.Body.String()
		if !strings.Contains(body, "<title>") || !strings.Contains(body, `id="page-data"`) || strings.Contains(body, "render failed") {
			t.Errorf("%s: unexpected body", p)
		}
		if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self'") {
			t.Errorf("%s: csp %q", p, csp)
		}
	}
	if rec := get(h, "GET", "/devices/nope", ""); rec.Code != 404 || !strings.Contains(rec.Body.String(), "No device") {
		t.Errorf("missing device page: %d", rec.Code)
	}
	if rec := get(h, "GET", "/login", ""); rec.Code != 404 {
		t.Errorf("there is no login page: %d", rec.Code)
	}
	if rec := get(h, "GET", "/static/app.js", ""); rec.Code != 200 || !strings.Contains(rec.Header().Get("Cache-Control"), "max-age") {
		t.Errorf("static: %d %q", rec.Code, rec.Header().Get("Cache-Control"))
	}
	if rec := get(h, "GET", "/healthz", ""); rec.Code != 200 {
		t.Errorf("healthz: %d", rec.Code)
	}
}

func TestDataBlockEscapesScriptClose(t *testing.T) {
	h, _ := newTestServer(t)
	body := get(h, "GET", "/", "").Body.String()
	start := strings.Index(body, `<script type="application/json" id="page-data">`)
	end := strings.Index(body[start:], "</script>")
	block := body[start : start+end]
	if strings.Count(block, "</") != 0 {
		t.Error("data block must not contain a raw closing tag sequence")
	}
}

func TestAPI(t *testing.T) {
	h, id := newTestServer(t)
	rec := get(h, "GET", "/api/devices", "")
	var list struct {
		Devices []fleet.Device `json:"devices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list.Devices) != 1 || list.Devices[0].Name != "Test plug" {
		t.Fatalf("devices: %d %s", rec.Code, rec.Body.String())
	}
	if rec := get(h, "POST", "/api/devices/"+id+"/power", `{"relay":1,"state":"OFF"}`); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"on":false`) {
		t.Errorf("power: %d %s", rec.Code, rec.Body.String())
	}
	if rec := get(h, "POST", "/api/devices/"+id+"/power", `{"relay":1,"state":"sideways"}`); rec.Code != 400 {
		t.Errorf("bad power state: %d", rec.Code)
	}
	if rec := get(h, "POST", "/api/devices/"+id+"/command", `{"command":"Status 2"}`); rec.Code != 200 {
		t.Errorf("command: %d %s", rec.Code, rec.Body.String())
	}
	if rec := get(h, "POST", "/api/devices/"+id+"/command", `{"command":"Status 2","extra":1}`); rec.Code != 400 {
		t.Errorf("unknown field accepted: %d", rec.Code)
	}
	if rec := get(h, "GET", "/api/devices/"+id+"/backup", ""); rec.Code != 200 || rec.Header().Get("Content-Disposition") == "" {
		t.Errorf("backup: %d", rec.Code)
	}
	rec = get(h, "POST", "/api/bulk/backup", `{"ids":["`+id+`"]}`)
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "application/zip" {
		t.Errorf("bulk backup: %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	if rec := get(h, "POST", "/api/bulk/backup", `{"ids":["ghost"]}`); rec.Code != 404 {
		t.Errorf("bulk backup unknown id: %d %s", rec.Code, rec.Body.String())
	}
	rec = get(h, "POST", "/api/bulk/command", `{"ids":["`+id+`"],"command":"SetOption65 1"}`)
	if rec.Code != 202 {
		t.Fatalf("bulk command: %d %s", rec.Code, rec.Body.String())
	}
	var job struct {
		Job fleet.Job `json:"job"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &job)
	if rec := get(h, "GET", "/jobs/"+job.Job.ID, ""); rec.Code != 200 {
		t.Errorf("job page: %d", rec.Code)
	}
	if rec := get(h, "GET", "/api/jobs/"+job.Job.ID, ""); rec.Code != 200 {
		t.Errorf("job api: %d", rec.Code)
	}
	if rec := get(h, "DELETE", "/api/devices/"+id, ""); rec.Code != 200 {
		t.Errorf("remove: %d", rec.Code)
	}
	if rec := get(h, "GET", "/api/devices/"+id, ""); rec.Code != 404 {
		t.Errorf("after remove: %d", rec.Code)
	}
}

func TestIdentityAndCrossOrigin(t *testing.T) {
	h, id := newTestServer(t)
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("X-Authentik-Username", "sample-user")
	r.Header.Set("X-Authentik-Name", "Sample User")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if !strings.Contains(rec.Body.String(), `title="sample-user"`) || !strings.Contains(rec.Body.String(), "Sample User") {
		t.Error("identity header not rendered")
	}
	if !strings.Contains(get(h, "GET", "/", "").Body.String(), `title="anonymous"`) {
		t.Error("missing header should render anonymous")
	}
	// A cross-site POST (what a CSRF page produces) is refused; a same-site
	// one and a header-less client request pass.
	post := func(site string) int {
		r := httptest.NewRequest("POST", "/api/devices/"+id+"/refresh", nil)
		if site != "" {
			r.Header.Set("Sec-Fetch-Site", site)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec.Code
	}
	if c := post("cross-site"); c != 403 {
		t.Errorf("cross-site POST: %d", c)
	}
	if c := post("same-origin"); c != 200 {
		t.Errorf("same-origin POST: %d", c)
	}
	if c := post(""); c != 200 {
		t.Errorf("header-less POST: %d", c)
	}
}
