package fleet

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/rake-pro/tasmonaut/internal/tasmota"
	"github.com/rs/zerolog/log"
)

// releasesURL is GitHub's "latest release" endpoint for Tasmota. It is the one
// outbound request tasmonaut makes beyond the LAN, and it is optional.
const releasesURL = "https://api.github.com/repos/arendst/Tasmota/releases/latest"

// Firmware is the reference version devices are compared against.
type Firmware struct {
	Version   tasmota.Version `json:"version"`
	Tag       string          `json:"tag"`
	URL       string          `json:"url,omitempty"`
	Published time.Time       `json:"published,omitempty"`
	CheckedAt time.Time       `json:"checkedAt,omitempty"`
	Source    string          `json:"source"` // "github", "config" or "none"
	Error     string          `json:"error,omitempty"`
}

// firmwareRef tracks the reference version, pinned or fetched.
type firmwareRef struct {
	mu   sync.RWMutex
	cur  Firmware
	http *http.Client
}

func newFirmwareRef(pinned string) *firmwareRef {
	f := &firmwareRef{http: &http.Client{Timeout: 15 * time.Second}}
	if pinned != "" {
		v, err := tasmota.ParseVersion(pinned)
		if err == nil {
			f.cur = Firmware{Version: v, Tag: "v" + v.Short(), Source: "config", CheckedAt: time.Now()}
		}
	} else {
		f.cur = Firmware{Source: "none"}
	}
	return f
}

func (f *firmwareRef) get() Firmware {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.cur
}

// known reports whether there is a version to compare against.
func (f Firmware) known() bool { return f.Version.Major > 0 }

// refresh asks GitHub for the latest release and replaces the reference when
// it parses. A failure keeps the previous value and records the error.
func (f *firmwareRef) refresh(ctx context.Context) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, releasesURL, nil)
	if err != nil {
		return
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "tasmonaut")
	resp, err := f.http.Do(req)
	if err != nil {
		f.fail(err)
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		f.fail(fmt.Errorf("github: HTTP %d", resp.StatusCode))
		return
	}
	var rel struct {
		TagName     string    `json:"tag_name"`
		HTMLURL     string    `json:"html_url"`
		PublishedAt time.Time `json:"published_at"`
	}
	if err := json.Unmarshal(body, &rel); err != nil {
		f.fail(err)
		return
	}
	v, err := tasmota.ParseVersion(rel.TagName)
	if err != nil {
		f.fail(err)
		return
	}
	f.mu.Lock()
	f.cur = Firmware{Version: v, Tag: rel.TagName, URL: rel.HTMLURL, Published: rel.PublishedAt, CheckedAt: time.Now(), Source: "github"}
	f.mu.Unlock()
	log.Info().Str("tag", rel.TagName).Msg("latest Tasmota release")
}

func (f *firmwareRef) fail(err error) {
	f.mu.Lock()
	f.cur.Error = err.Error()
	f.cur.CheckedAt = time.Now()
	f.mu.Unlock()
	log.Warn().Err(err).Msg("latest Tasmota release check failed")
}

// updateAvailable compares a device version to the reference.
func (f Firmware) updateAvailable(v tasmota.Version) bool {
	if !f.known() || v.Major == 0 {
		return false
	}
	return v.Safeboot() || v.Compare(f.Version) < 0
}
