package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/rake-pro/tasmonaut/internal/fleet"
	"github.com/rake-pro/tasmonaut/internal/tasmota"
	"github.com/rs/zerolog/log"
)

// maxJSONBody bounds API request bodies; the largest legitimate one is a
// bulk command with a few hundred ids.
const maxJSONBody = 256 << 10

// maxRestoreBody bounds a configuration upload. ESP32 dumps are 8 KB.
const maxRestoreBody = 64 << 10

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// apiError maps an error to a status and a one-line message the page shows
// in a toast. Messages lead with the field or device, like the writing rules
// say.
func apiError(w http.ResponseWriter, err error) {
	status := http.StatusBadGateway
	switch {
	case errors.Is(err, fleet.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, tasmota.ErrUnauthorized):
		status = http.StatusBadGateway
	case isValidation(err):
		status = http.StatusBadRequest
	}
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

// isValidation recognises the "field: what is wrong" errors the fleet raises
// before contacting a device.
func isValidation(err error) bool {
	msg := err.Error()
	for _, p := range []string{"host:", "command:", "state:", "relay:", "ids:", "file:", "device \""} {
		if strings.HasPrefix(msg, p) {
			return true
		}
	}
	return false
}

func readJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "body: " + err.Error()})
		return false
	}
	return true
}

func (s *Server) apiDevices(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"devices":  s.fleet.Devices(),
		"firmware": s.fleet.Firmware(),
		"lastScan": s.fleet.LastScan(),
		"now":      time.Now(),
	})
}

func (s *Server) apiDevice(w http.ResponseWriter, r *http.Request) {
	dev, ok := s.fleet.Get(r.PathValue("id"))
	if !ok {
		apiError(w, fleet.ErrNotFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"device": dev, "raw": dev.Raw, "firmware": s.fleet.Firmware()})
}

func (s *Server) apiAddDevice(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Host string `json:"host"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	dev, err := s.fleet.Add(r.Context(), in.Host)
	if err != nil {
		apiError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"device": dev})
}

func (s *Server) apiRemoveDevice(w http.ResponseWriter, r *http.Request) {
	if err := s.fleet.Remove(r.PathValue("id")); err != nil {
		apiError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"removed": true})
}

func (s *Server) apiRefreshDevice(w http.ResponseWriter, r *http.Request) {
	dev, err := s.fleet.Refresh(r.Context(), r.PathValue("id"))
	if err != nil {
		apiError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"device": dev, "raw": dev.Raw})
}

func (s *Server) apiPower(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Relay int    `json:"relay"`
		State string `json:"state"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if in.Relay == 0 {
		in.Relay = 1
	}
	on, err := s.fleet.SetPower(r.Context(), r.PathValue("id"), in.Relay, in.State)
	if err != nil {
		apiError(w, err)
		return
	}
	dev, _ := s.fleet.Get(r.PathValue("id"))
	writeJSON(w, http.StatusOK, map[string]any{"relay": in.Relay, "on": on, "device": dev})
}

func (s *Server) apiCommand(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Command string `json:"command"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	raw, err := s.fleet.Command(r.Context(), r.PathValue("id"), in.Command)
	if err != nil {
		apiError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"command": in.Command, "reply": raw})
}

func (s *Server) apiRestart(w http.ResponseWriter, r *http.Request) {
	if err := s.fleet.Restart(r.Context(), r.PathValue("id")); err != nil {
		apiError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"restarting": true})
}

func (s *Server) apiBackup(w http.ResponseWriter, r *http.Request) {
	name, data, err := s.fleet.Backup(r.Context(), r.PathValue("id"))
	if err != nil {
		apiError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}

func (s *Server) apiRestore(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRestoreBody+4096)
	f, hdr, err := r.FormFile("file")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "file: a configuration dump (.dmp) is required"})
		return
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxRestoreBody))
	if err != nil {
		apiError(w, err)
		return
	}
	if err := s.fleet.Restore(r.Context(), r.PathValue("id"), hdr.Filename, data); err != nil {
		apiError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"restored": true, "bytes": len(data)})
}

func (s *Server) apiScan(w http.ResponseWriter, _ *http.Request) {
	// A sweep can take a minute; run it detached and let the page poll
	// /api/devices. A sweep already in progress makes this a no-op.
	go s.fleet.Scan(context.Background())
	writeJSON(w, http.StatusAccepted, map[string]bool{"scanning": true})
}

func (s *Server) apiRefreshAll(w http.ResponseWriter, r *http.Request) {
	s.fleet.RefreshAll(r.Context())
	s.apiDevices(w, r)
}

func (s *Server) apiFirmware(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.fleet.Firmware())
}

func (s *Server) apiBulkPower(w http.ResponseWriter, r *http.Request) {
	var in struct {
		IDs   []string `json:"ids"`
		Relay int      `json:"relay"`
		State string   `json:"state"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	job, err := s.fleet.BulkPower(in.IDs, in.Relay, in.State)
	if err != nil {
		apiError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"job": job})
}

func (s *Server) apiBulkCommand(w http.ResponseWriter, r *http.Request) {
	var in struct {
		IDs     []string `json:"ids"`
		Command string   `json:"command"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	job, err := s.fleet.BulkCommand(in.IDs, in.Command)
	if err != nil {
		apiError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"job": job})
}

func (s *Server) apiBulkUpgrade(w http.ResponseWriter, r *http.Request) {
	var in struct {
		IDs    []string `json:"ids"`
		OtaURL string   `json:"otaUrl"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	job, err := s.fleet.BulkUpgrade(in.IDs, in.OtaURL)
	if err != nil {
		apiError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"job": job})
}

// apiBulkBackup streams a zip of the selected devices' configuration dumps.
// It is a POST because the selection travels in the body; the page script
// turns the response into a download.
func (s *Server) apiBulkBackup(w http.ResponseWriter, r *http.Request) {
	var in struct {
		IDs []string `json:"ids"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	// Validate before any header is written so a bad selection is a JSON
	// error rather than a broken download.
	if len(in.IDs) == 0 {
		apiError(w, errors.New("ids: select at least one device"))
		return
	}
	for _, id := range in.IDs {
		if _, ok := s.fleet.Get(id); !ok {
			apiError(w, fmt.Errorf("device %q: %w", id, fleet.ErrNotFound))
			return
		}
	}
	name := "tasmota-configs-" + time.Now().UTC().Format("20060102-150405") + ".zip"
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
	w.Header().Set("Cache-Control", "no-store")
	if err := s.fleet.BackupZip(r.Context(), in.IDs, w); err != nil {
		// Headers are out; the best that can be done is to log it.
		log.Error().Err(err).Msg("bulk backup failed mid-stream")
	}
}

func (s *Server) apiJobs(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"jobs": s.fleet.Jobs()})
}

func (s *Server) apiJob(w http.ResponseWriter, r *http.Request) {
	j, ok := s.fleet.Job(r.PathValue("id"))
	if !ok {
		apiError(w, fleet.ErrNotFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"job": j})
}
