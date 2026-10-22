package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"time"

	"github.com/rake-pro/tasmonaut/internal/fleet"
	"github.com/rake-pro/tasmonaut/web"
	"github.com/rs/zerolog/log"
)

// pageNames are the page templates; each is parsed together with the layout.
var pageNames = []string{"devices", "device", "jobs", "job", "error"}

func (s *Server) parseTemplates() error {
	funcs := template.FuncMap{
		"json": func(v any) (template.JS, error) {
			b, err := json.Marshal(v)
			if err != nil {
				return "", err
			}
			// A JSON data block inside <script type="application/json">: the
			// only sequence that could break out of it is "</script".
			b = bytes.ReplaceAll(b, []byte("</"), []byte(`<\/`))
			return template.JS(b), nil
		},
		"ago": func(t time.Time) string {
			if t.IsZero() {
				return "never"
			}
			d := time.Since(t).Round(time.Second)
			switch {
			case d < time.Minute:
				return fmt.Sprintf("%ds ago", int(d.Seconds()))
			case d < time.Hour:
				return fmt.Sprintf("%dm ago", int(d.Minutes()))
			default:
				return t.Local().Format("2006-01-02 15:04")
			}
		},
		"lower": strings.ToLower,
		// dict builds the object a page's data block serialises.
		"dict": func(kv ...any) (map[string]any, error) {
			if len(kv)%2 != 0 {
				return nil, errors.New("dict: odd number of arguments")
			}
			m := make(map[string]any, len(kv)/2)
			for i := 0; i < len(kv); i += 2 {
				k, ok := kv[i].(string)
				if !ok {
					return nil, errors.New("dict: keys must be strings")
				}
				m[k] = kv[i+1]
			}
			return m, nil
		},
	}
	s.pages = make(map[string]*template.Template, len(pageNames))
	for _, name := range pageNames {
		t, err := template.New(name).Funcs(funcs).ParseFS(web.Templates, "templates/layout.html", "templates/"+name+".html")
		if err != nil {
			return fmt.Errorf("template %s: %w", name, err)
		}
		s.pages[name] = t
	}
	return nil
}

// pageData is what every page template receives.
type pageData struct {
	Title     string
	Nav       string // current rail item
	Version   string
	User      User
	Firmware  fleet.Firmware
	Devices   []fleet.Device
	Device    *fleet.Device
	Jobs      []fleet.Job
	Job       *fleet.Job
	Error     string
	ErrorCode int
}

func (s *Server) base(r *http.Request, title, nav string) pageData {
	return pageData{Title: title, Nav: nav, Version: s.version, User: UserFromContext(r.Context()), Firmware: s.fleet.Firmware()}
}

func (s *Server) render(w http.ResponseWriter, status int, name string, data pageData) {
	t := s.pages[name]
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "layout", data); err != nil {
		log.Error().Err(err).Str("page", name).Msg("render failed")
		http.Error(w, "render failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = buf.WriteTo(w)
}

func (s *Server) errorPage(w http.ResponseWriter, r *http.Request, code int, msg string) {
	d := s.base(r, http.StatusText(code), "")
	d.Error, d.ErrorCode = msg, code
	s.render(w, code, "error", d)
}

func (s *Server) devicesPage(w http.ResponseWriter, r *http.Request) {
	d := s.base(r, "Devices", "devices")
	d.Devices = s.fleet.Devices()
	s.render(w, http.StatusOK, "devices", d)
}

func (s *Server) devicePage(w http.ResponseWriter, r *http.Request) {
	dev, ok := s.fleet.Get(r.PathValue("id"))
	if !ok {
		s.errorPage(w, r, http.StatusNotFound, "No device with that id is in the inventory. It may have been removed, or the process restarted and discovery has not found it again yet.")
		return
	}
	d := s.base(r, dev.Name, "devices")
	d.Device = &dev
	s.render(w, http.StatusOK, "device", d)
}

func (s *Server) jobsPage(w http.ResponseWriter, r *http.Request) {
	d := s.base(r, "Jobs", "jobs")
	d.Jobs = s.fleet.Jobs()
	s.render(w, http.StatusOK, "jobs", d)
}

func (s *Server) jobPage(w http.ResponseWriter, r *http.Request) {
	j, ok := s.fleet.Job(r.PathValue("id"))
	if !ok {
		s.errorPage(w, r, http.StatusNotFound, "No job with that id. Jobs live in memory; only the newest fifty are kept and none survive a restart.")
		return
	}
	d := s.base(r, "Job "+j.ID, "jobs")
	d.Job = &j
	s.render(w, http.StatusOK, "job", d)
}
