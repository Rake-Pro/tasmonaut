// Package server wires the HTTP surface: pages, the JSON API the page script
// talks to, static assets and health probes. Authentication is the reverse
// proxy's job (gpm SSO gate); the server only reads the identity header and
// rejects cross-site mutating requests.
package server

import (
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/rake-pro/tasmonaut/internal/fleet"
	"github.com/rake-pro/tasmonaut/web"
	"github.com/rs/zerolog/log"
)

// Server holds the handler dependencies.
type Server struct {
	fleet   *fleet.Fleet
	version string
	pages   map[string]*template.Template
	static  http.Handler
	started time.Time
}

// New builds the fully wrapped handler: recover -> access log -> security
// headers -> identity -> cross-origin protection -> mux. identityHeader is
// the proxy header carrying the signed-in username.
func New(fl *fleet.Fleet, version, identityHeader string) (http.Handler, error) {
	s := &Server{fleet: fl, version: version, started: time.Now()}
	if err := s.parseTemplates(); err != nil {
		return nil, err
	}
	sub, err := fs.Sub(web.Static, "static")
	if err != nil {
		return nil, err
	}
	s.static = http.FileServerFS(sub)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /readyz", s.health)
	mux.HandleFunc("GET /static/", s.serveStatic)

	// Every route below assumes the proxy has already authenticated the caller.
	// admin and mutate are kept as names so the route table reads like the
	// other Rake-Pro apps; here they are identity only.
	admin := func(h http.HandlerFunc) http.HandlerFunc { return h }
	mutate := func(h http.HandlerFunc) http.HandlerFunc { return h }

	mux.HandleFunc("GET /{$}", admin(s.devicesPage))
	mux.HandleFunc("GET /devices/{id}", admin(s.devicePage))
	mux.HandleFunc("GET /jobs", admin(s.jobsPage))
	mux.HandleFunc("GET /jobs/{id}", admin(s.jobPage))

	mux.HandleFunc("GET /api/devices", admin(s.apiDevices))
	mux.HandleFunc("POST /api/devices", mutate(s.apiAddDevice))
	mux.HandleFunc("GET /api/devices/{id}", admin(s.apiDevice))
	mux.HandleFunc("DELETE /api/devices/{id}", mutate(s.apiRemoveDevice))
	mux.HandleFunc("POST /api/devices/{id}/refresh", mutate(s.apiRefreshDevice))
	mux.HandleFunc("POST /api/devices/{id}/power", mutate(s.apiPower))
	mux.HandleFunc("POST /api/devices/{id}/command", mutate(s.apiCommand))
	mux.HandleFunc("POST /api/devices/{id}/restart", mutate(s.apiRestart))
	mux.HandleFunc("GET /api/devices/{id}/backup", admin(s.apiBackup))
	mux.HandleFunc("POST /api/devices/{id}/restore", mutate(s.apiRestore))

	mux.HandleFunc("POST /api/scan", mutate(s.apiScan))
	mux.HandleFunc("POST /api/refresh", mutate(s.apiRefreshAll))
	mux.HandleFunc("GET /api/firmware", admin(s.apiFirmware))

	mux.HandleFunc("POST /api/bulk/power", mutate(s.apiBulkPower))
	mux.HandleFunc("POST /api/bulk/command", mutate(s.apiBulkCommand))
	mux.HandleFunc("POST /api/bulk/upgrade", mutate(s.apiBulkUpgrade))
	mux.HandleFunc("POST /api/bulk/backup", mutate(s.apiBulkBackup))
	mux.HandleFunc("GET /api/jobs", admin(s.apiJobs))
	mux.HandleFunc("GET /api/jobs/{id}", admin(s.apiJob))

	// CrossOriginProtection (net/http) rejects state-changing requests whose
	// Sec-Fetch-Site or Origin says they came from another site. That is the
	// CSRF defence for a browser that carries the proxy's SSO cookie: a page on
	// another origin cannot POST here even though the cookie would be sent.
	// Non-browser clients (curl, scripts) send neither header and pass.
	csrf := http.NewCrossOriginProtection()
	return recoverer(accessLog(securityHeaders(identity(identityHeader)(csrf.Handler(mux))))), nil
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = fmt.Fprintln(w, "ok")
}

// serveStatic serves the embedded assets. Everything under /static/ is
// immutable per build, so clients may cache it for a day.
func (s *Server) serveStatic(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "public, max-age=86400")
	http.StripPrefix("/static/", s.static).ServeHTTP(w, r)
}

const csp = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; " +
	"font-src 'self'; connect-src 'self'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		next.ServeHTTP(w, r)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

// Flush lets streaming responses (the backup zip) reach the client early.
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/static/") || r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r)
		ev := log.Info()
		if sw.status >= 500 {
			ev = log.Error()
		} else if sw.status >= 400 {
			ev = log.Warn()
		}
		ev.Str("method", r.Method).Str("path", r.URL.Path).Int("status", sw.status).
			Dur("took", time.Since(start)).Str("remote", r.RemoteAddr).Str("user", UserFromContext(r.Context()).Username).Msg("request")
	})
}

func recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Error().Interface("panic", rec).Str("path", r.URL.Path).Bytes("stack", debug.Stack()).Msg("handler panic")
				http.Error(w, "internal error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}
