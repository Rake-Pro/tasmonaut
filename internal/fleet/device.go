// Package fleet is the in-memory device inventory: configured devices plus
// whatever discovery finds, refreshed by a poller, acted on one at a time or
// in bulk jobs. Nothing here touches disk; a restart starts from the
// configuration again.
package fleet

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/rake-pro/tasmonaut/internal/tasmota"
)

// Source says how a device entered the inventory.
type Source string

const (
	SourceConfig     Source = "config"
	SourceDiscovered Source = "discovered"
	SourceManual     Source = "manual"
)

// Device is one inventory entry. Status is the last successful Status 0; it
// stays after the device goes offline so the page can still name it.
type Device struct {
	ID     string `json:"id"`
	Host   string `json:"host"`
	Name   string `json:"name"`
	Source Source `json:"source"`

	Online     bool      `json:"online"`
	LastSeen   time.Time `json:"lastSeen,omitempty"`
	LastPolled time.Time `json:"lastPolled,omitempty"`
	LastError  string    `json:"lastError,omitempty"`

	Status *tasmota.Status `json:"status,omitempty"`
	Raw    json.RawMessage `json:"-"`

	// UpdateAvailable is set when a reference version is known and the
	// device's firmware is older. Safeboot builds never count as current.
	UpdateAvailable bool `json:"updateAvailable"`

	nameOverride string
	auth         tasmota.Auth
}

// DisplayName is the configured name, else the device's own, else the host.
func (d *Device) DisplayName() string {
	if d.nameOverride != "" {
		return d.nameOverride
	}
	if d.Status != nil && d.Status.DeviceName != "" {
		return d.Status.DeviceName
	}
	return d.Host
}

// idFromMAC derives the stable inventory id from a MAC address.
func idFromMAC(mac string) string {
	return strings.ToLower(strings.ReplaceAll(mac, ":", ""))
}

// idFromHost is the provisional id used until the first successful poll
// reveals the MAC.
func idFromHost(host string) string {
	return "host-" + strings.NewReplacer(".", "-", ":", "-", "/", "-").Replace(strings.ToLower(host))
}

func (d *Device) applyStatus(st tasmota.Status, raw json.RawMessage, now time.Time) {
	d.Status = &st
	d.Raw = raw
	d.Online = true
	d.LastSeen = now
	d.LastPolled = now
	d.LastError = ""
	d.Name = d.DisplayName()
}

func (d *Device) applyError(err error, now time.Time) {
	d.Online = false
	d.LastPolled = now
	d.LastError = err.Error()
	d.Name = d.DisplayName()
}
