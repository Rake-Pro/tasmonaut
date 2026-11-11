package server

import (
	"bytes"
	"errors"
	"net/http"
	"time"

	"github.com/rake-pro/tasmonaut/internal/fleet"
)

// NewMirror builds the handler of the firmware mirror listener. It is a
// separate, unauthenticated plain-HTTP surface for the devices (an ESP8266
// cannot download an OTA image over HTTPS, nor pass the SSO gate), so it
// serves one thing only: GET /firmware/<image> of the reference release.
// Nothing of the console or its API is reachable through it.
func NewMirror(fl *fleet.Fleet) http.Handler {
	// The fleet upgrades four devices at a time. Anything beyond this is not
	// the fleet, and each request can pin one image in memory while it is
	// being sent.
	slots := make(chan struct{}, 8)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /firmware/{name}", func(w http.ResponseWriter, r *http.Request) {
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		default:
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		name := r.PathValue("name")
		b, _, err := fl.FirmwareImage(r.Context(), name)
		switch {
		case errors.Is(err, fleet.ErrNoImage):
			http.Error(w, "not found", http.StatusNotFound)
			return
		case errors.Is(err, fleet.ErrNoRelease):
			http.Error(w, "no reference release", http.StatusServiceUnavailable)
			return
		case err != nil:
			http.Error(w, "upstream download failed", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		// ServeContent sets Content-Length, which the device updaters require.
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(b))
	})
	return recoverer(accessLog(mux))
}
