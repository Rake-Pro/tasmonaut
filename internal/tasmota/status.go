package tasmota

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Status is the part of a `Status 0` response that tasmonaut shows and acts
// on. Everything else stays available as the raw JSON next to it.
type Status struct {
	DeviceName    string
	FriendlyNames []string
	Topic         string
	Module        int
	// Relays are the POWER states in relay order, from StatusSTS.POWER /
	// POWERn. A bulb has one, an S31 one, a four-channel board four.
	Relays []Relay

	Hostname string
	IP       string
	MAC      string
	Gateway  string

	Version       Version
	BuildDateTime string
	Hardware      string
	Core          string
	SDK           string

	OtaURL        string
	RestartReason string
	Uptime        string
	UptimeSec     int64
	BootCount     int
	StartupUTC    string

	Wifi struct {
		SSID     string
		BSSID    string
		Channel  int
		Mode     string
		RSSI     int
		Signal   int
		Downtime string
	}
	MQTT struct {
		Host   string
		Port   int
		Client string
		Count  int
	}

	// Energy is present on metering plugs (S31, POWR320D) and nil otherwise.
	Energy *Energy
	// Dimmer is -1 when the device has no light channel.
	Dimmer int

	Heap     int
	LoadAvg  int
	TimeUTC  string
	Features []string
}

// Relay is one power output.
type Relay struct {
	Index int    // 1-based, as Tasmota numbers them (Power1 ... Power8)
	Name  string // FriendlyName entry when present
	On    bool
}

// Energy is StatusSNS.ENERGY.
type Energy struct {
	Power         float64 `json:"Power"`
	ApparentPower float64 `json:"ApparentPower"`
	ReactivePower float64 `json:"ReactivePower"`
	Factor        float64 `json:"Factor"`
	Voltage       float64 `json:"Voltage"`
	Current       float64 `json:"Current"`
	Today         float64 `json:"Today"`
	Yesterday     float64 `json:"Yesterday"`
	Total         float64 `json:"Total"`
}

var powerKey = regexp.MustCompile(`^POWER(\d*)$`)

// ParseStatus0 parses a `Status 0` response. It accepts the partial responses
// a device sends when some sections are missing; the only hard requirement is
// StatusFWR, which is also how discovery tells a Tasmota device from any other
// web server on the subnet.
func ParseStatus0(raw []byte) (Status, error) {
	var top struct {
		Status *struct {
			Module       int             `json:"Module"`
			DeviceName   string          `json:"DeviceName"`
			FriendlyName []string        `json:"FriendlyName"`
			Topic        string          `json:"Topic"`
			Power        json.RawMessage `json:"Power"`
		} `json:"Status"`
		StatusPRM *struct {
			OtaURL        string `json:"OtaUrl"`
			RestartReason string `json:"RestartReason"`
			Uptime        string `json:"Uptime"`
			StartupUTC    string `json:"StartupUTC"`
			BootCount     int    `json:"BootCount"`
		} `json:"StatusPRM"`
		StatusFWR *struct {
			Version       string `json:"Version"`
			BuildDateTime string `json:"BuildDateTime"`
			Core          string `json:"Core"`
			SDK           string `json:"SDK"`
			Hardware      string `json:"Hardware"`
		} `json:"StatusFWR"`
		StatusMEM *struct {
			Features []string `json:"Features"`
		} `json:"StatusMEM"`
		StatusNET *struct {
			Hostname  string `json:"Hostname"`
			IPAddress string `json:"IPAddress"`
			Gateway   string `json:"Gateway"`
			Mac       string `json:"Mac"`
		} `json:"StatusNET"`
		StatusMQT *struct {
			MqttHost   string `json:"MqttHost"`
			MqttPort   int    `json:"MqttPort"`
			MqttClient string `json:"MqttClient"`
			MqttCount  int    `json:"MqttCount"`
		} `json:"StatusMQT"`
		StatusTIM *struct {
			UTC string `json:"UTC"`
		} `json:"StatusTIM"`
		StatusSNS *struct {
			Energy *Energy `json:"ENERGY"`
		} `json:"StatusSNS"`
		StatusSTS map[string]json.RawMessage `json:"StatusSTS"`
	}
	if err := json.Unmarshal(raw, &top); err != nil {
		return Status{}, fmt.Errorf("status: %w", err)
	}
	if top.StatusFWR == nil {
		return Status{}, fmt.Errorf("status: no StatusFWR section (not a Tasmota device, or the response was cut short)")
	}
	st := Status{Dimmer: -1}
	st.Version, _ = ParseVersion(top.StatusFWR.Version)
	st.BuildDateTime = top.StatusFWR.BuildDateTime
	st.Core = top.StatusFWR.Core
	st.SDK = top.StatusFWR.SDK
	st.Hardware = top.StatusFWR.Hardware

	if s := top.Status; s != nil {
		st.Module = s.Module
		st.DeviceName = s.DeviceName
		st.FriendlyNames = s.FriendlyName
		st.Topic = s.Topic
	}
	if p := top.StatusPRM; p != nil {
		st.OtaURL = p.OtaURL
		st.RestartReason = p.RestartReason
		st.Uptime = p.Uptime
		st.StartupUTC = p.StartupUTC
		st.BootCount = p.BootCount
	}
	if m := top.StatusMEM; m != nil {
		st.Features = m.Features
	}
	if n := top.StatusNET; n != nil {
		st.Hostname = n.Hostname
		st.IP = n.IPAddress
		st.Gateway = n.Gateway
		st.MAC = n.Mac
	}
	if q := top.StatusMQT; q != nil {
		st.MQTT.Host = q.MqttHost
		st.MQTT.Port = q.MqttPort
		st.MQTT.Client = q.MqttClient
		st.MQTT.Count = q.MqttCount
	}
	if t := top.StatusTIM; t != nil {
		st.TimeUTC = t.UTC
	}
	if s := top.StatusSNS; s != nil {
		st.Energy = s.Energy
	}
	if sts := top.StatusSTS; sts != nil {
		getInt(sts, "Heap", &st.Heap)
		getInt(sts, "LoadAvg", &st.LoadAvg)
		getInt(sts, "Dimmer", &st.Dimmer)
		var sec int64
		if raw, ok := sts["UptimeSec"]; ok {
			_ = json.Unmarshal(raw, &sec)
		}
		st.UptimeSec = sec
		if st.Uptime == "" {
			var up string
			if raw, ok := sts["Uptime"]; ok {
				_ = json.Unmarshal(raw, &up)
			}
			st.Uptime = up
		}
		if raw, ok := sts["Wifi"]; ok {
			var w struct {
				SSId     string `json:"SSId"`
				BSSId    string `json:"BSSId"`
				Channel  int    `json:"Channel"`
				Mode     string `json:"Mode"`
				RSSI     int    `json:"RSSI"`
				Signal   int    `json:"Signal"`
				Downtime string `json:"Downtime"`
			}
			if json.Unmarshal(raw, &w) == nil {
				st.Wifi.SSID, st.Wifi.BSSID, st.Wifi.Channel = w.SSId, w.BSSId, w.Channel
				st.Wifi.Mode, st.Wifi.RSSI, st.Wifi.Signal, st.Wifi.Downtime = w.Mode, w.RSSI, w.Signal, w.Downtime
			}
		}
		st.Relays = parseRelays(sts, st.FriendlyNames)
	}
	if st.IP == "" || st.Hostname == "" {
		// Some builds put these only in StatusSTS.
		if raw, ok := top.StatusSTS["IPAddress"]; ok && st.IP == "" {
			_ = json.Unmarshal(raw, &st.IP)
		}
		if raw, ok := top.StatusSTS["Hostname"]; ok && st.Hostname == "" {
			_ = json.Unmarshal(raw, &st.Hostname)
		}
	}
	return st, nil
}

func getInt(m map[string]json.RawMessage, key string, dst *int) {
	if raw, ok := m[key]; ok {
		var n int
		if json.Unmarshal(raw, &n) == nil {
			*dst = n
		}
	}
}

// parseRelays reads POWER / POWER1..n from StatusSTS. A single-relay device
// reports the bare key POWER, which is relay 1.
func parseRelays(sts map[string]json.RawMessage, names []string) []Relay {
	var relays []Relay
	for k, raw := range sts {
		m := powerKey.FindStringSubmatch(k)
		if m == nil {
			continue
		}
		idx := 1
		if m[1] != "" {
			idx, _ = strconv.Atoi(m[1])
		}
		var state string
		if json.Unmarshal(raw, &state) != nil {
			continue
		}
		r := Relay{Index: idx, On: strings.EqualFold(state, "ON")}
		if idx-1 < len(names) && names[idx-1] != "" {
			r.Name = names[idx-1]
		}
		relays = append(relays, r)
	}
	sort.Slice(relays, func(i, j int) bool { return relays[i].Index < relays[j].Index })
	return relays
}

// ParsePowerReply reads the response to a Power command ({"POWER":"ON"} or
// {"POWER2":"OFF"}) and returns the relay index and state.
func ParsePowerReply(raw []byte) (index int, on bool, err error) {
	var m map[string]string
	if err := json.Unmarshal(raw, &m); err != nil {
		return 0, false, fmt.Errorf("power reply: %w", err)
	}
	for k, v := range m {
		sub := powerKey.FindStringSubmatch(k)
		if sub == nil {
			continue
		}
		index = 1
		if sub[1] != "" {
			index, _ = strconv.Atoi(sub[1])
		}
		return index, strings.EqualFold(v, "ON"), nil
	}
	return 0, false, fmt.Errorf("power reply: no POWER key in %s", string(raw))
}
