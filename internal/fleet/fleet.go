package fleet

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rake-pro/tasmonaut/internal/config"
	"github.com/rake-pro/tasmonaut/internal/tasmota"
	"github.com/rs/zerolog/log"
)

// ErrNotFound is returned for an unknown device or job id.
var ErrNotFound = errors.New("not found")

// Fleet is the inventory plus the workers that keep it fresh.
type Fleet struct {
	cfg    config.Config
	client *tasmota.Client // poll timeout
	probe  *tasmota.Client // discovery timeout
	fw     *firmwareRef
	jobs   *jobStore

	mu      sync.RWMutex
	devices map[string]*Device
	// scanning serialises discovery sweeps; a sweep in progress makes the
	// next request a no-op rather than a second burst of 250 connections.
	scanning sync.Mutex
	lastScan time.Time
}

// New builds the inventory from the configured devices. Nothing is contacted
// until Start.
func New(cfg config.Config) *Fleet {
	f := &Fleet{
		cfg:     cfg,
		client:  tasmota.New(cfg.Poll.Timeout),
		probe:   tasmota.New(cfg.Discovery.Timeout),
		fw:      newFirmwareRef(cfg.Firmware.Latest),
		jobs:    newJobStore(),
		devices: make(map[string]*Device),
	}
	for _, d := range cfg.Devices {
		host := strings.TrimSpace(d.Host)
		dev := &Device{ID: idFromHost(host), Host: host, Source: SourceConfig, nameOverride: d.Name, auth: f.authFor(d.Username, d.Password)}
		dev.Name = dev.DisplayName()
		f.devices[dev.ID] = dev
	}
	return f
}

func (f *Fleet) authFor(user, pass string) tasmota.Auth {
	a := tasmota.Auth{Username: f.cfg.Tasmota.Username, Password: f.cfg.Tasmota.Password}
	if user != "" {
		a.Username = user
	}
	if pass != "" {
		a.Password = pass
	}
	return a
}

// Start runs the first refresh, the optional startup sweep and the background
// loops until ctx is cancelled. It returns once the initial pass is done so
// the first page already has data.
func (f *Fleet) Start(ctx context.Context) {
	if f.cfg.Firmware.CheckLatest {
		f.fw.refresh(ctx)
	}
	f.RefreshAll(ctx)
	if f.cfg.Discovery.AtStartup && len(f.cfg.Discovery.CIDRs) > 0 {
		f.Scan(ctx)
	}
	go f.loop(ctx)
}

func (f *Fleet) loop(ctx context.Context) {
	poll := newTicker(f.cfg.Poll.Interval)
	scan := newTicker(f.cfg.Discovery.Interval)
	fw := newTicker(time.Hour)
	defer poll.Stop()
	defer scan.Stop()
	defer fw.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-poll.C:
			f.RefreshAll(ctx)
		case <-scan.C:
			if len(f.cfg.Discovery.CIDRs) > 0 {
				f.Scan(ctx)
			}
		case <-fw.C:
			if f.cfg.Firmware.CheckLatest {
				f.fw.refresh(ctx)
				f.recomputeUpdates()
			}
		}
	}
}

// newTicker returns a ticker that never fires when d is zero (interval off).
func newTicker(d time.Duration) *time.Ticker {
	if d <= 0 {
		t := time.NewTicker(time.Hour)
		t.Stop()
		return t
	}
	return time.NewTicker(d)
}

// Devices returns a sorted snapshot of the inventory.
func (f *Fleet) Devices() []Device {
	f.mu.RLock()
	defer f.mu.RUnlock()
	out := make([]Device, 0, len(f.devices))
	for _, d := range f.devices {
		out = append(out, *d)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := strings.ToLower(out[i].Name), strings.ToLower(out[j].Name)
		if a != b {
			return a < b
		}
		return out[i].Host < out[j].Host
	})
	return out
}

// Get returns one device by id.
func (f *Fleet) Get(id string) (Device, bool) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	d, ok := f.devices[id]
	if !ok {
		return Device{}, false
	}
	return *d, true
}

// Firmware returns the current reference version.
func (f *Fleet) Firmware() Firmware { return f.fw.get() }

// Jobs lists bulk jobs, newest first.
func (f *Fleet) Jobs() []Job { return f.jobs.list() }

// Job returns one job.
func (f *Fleet) Job(id string) (Job, bool) { return f.jobs.get(id) }

// Add probes host and, when it answers as a Tasmota device, adds it to the
// inventory for the life of the process. It returns the stored device.
func (f *Fleet) Add(ctx context.Context, host string) (Device, error) {
	host = strings.TrimSpace(host)
	if host == "" {
		return Device{}, errors.New("host: must not be empty")
	}
	auth := f.authFor("", "")
	st, raw, err := f.client.Status0(ctx, host, auth)
	if err != nil {
		if errors.Is(err, tasmota.ErrNotTasmota) || errors.Is(err, tasmota.ErrUnauthorized) {
			return Device{}, fmt.Errorf("%s: %w", host, err)
		}
		return Device{}, err
	}
	dev := &Device{ID: idFromHost(host), Host: host, Source: SourceManual, auth: auth}
	dev.applyStatus(st, raw, time.Now())
	return *f.upsert(dev), nil
}

// Remove drops a device from the in-memory inventory. A configured device
// comes back on restart; a discovered one comes back on the next sweep.
func (f *Fleet) Remove(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.devices[id]; !ok {
		return ErrNotFound
	}
	delete(f.devices, id)
	return nil
}

// upsert stores dev, merging with any entry that already has its MAC or
// host. The configured name and credentials win over discovery.
func (f *Fleet) upsert(dev *Device) *Device {
	f.mu.Lock()
	defer f.mu.Unlock()
	if dev.Status != nil && dev.Status.MAC != "" {
		dev.ID = idFromMAC(dev.Status.MAC)
	}
	if existing, ok := f.devices[dev.ID]; ok {
		f.merge(existing, dev)
		return existing
	}
	// A configured device is keyed by host until its first poll; re-key it.
	for id, existing := range f.devices {
		if existing.Host == dev.Host && id != dev.ID {
			delete(f.devices, id)
			f.merge(existing, dev)
			existing.ID = dev.ID
			f.devices[dev.ID] = existing
			return existing
		}
	}
	f.devices[dev.ID] = dev
	return dev
}

// merge copies the fresh poll result into an existing entry.
func (f *Fleet) merge(into, from *Device) {
	into.Host = from.Host
	if from.Status != nil {
		into.Status, into.Raw = from.Status, from.Raw
		into.Online, into.LastSeen = true, from.LastSeen
		into.LastError = ""
	}
	into.LastPolled = from.LastPolled
	if into.Source == "" {
		into.Source = from.Source
	}
	into.Name = into.DisplayName()
	into.UpdateAvailable = into.Status != nil && f.fw.get().updateAvailable(into.Status.Version)
}

// Refresh polls one device now.
func (f *Fleet) Refresh(ctx context.Context, id string) (Device, error) {
	f.mu.RLock()
	d, ok := f.devices[id]
	f.mu.RUnlock()
	if !ok {
		return Device{}, ErrNotFound
	}
	f.pollOne(ctx, d)
	dev, ok := f.Get(id)
	if !ok {
		return Device{}, ErrNotFound
	}
	return dev, nil
}

// RefreshAll polls every device with bounded parallelism.
func (f *Fleet) RefreshAll(ctx context.Context) {
	f.mu.RLock()
	list := make([]*Device, 0, len(f.devices))
	for _, d := range f.devices {
		list = append(list, d)
	}
	f.mu.RUnlock()
	sem := make(chan struct{}, f.cfg.Poll.Parallel)
	var wg sync.WaitGroup
	for _, d := range list {
		wg.Add(1)
		sem <- struct{}{}
		go func(d *Device) {
			defer wg.Done()
			defer func() { <-sem }()
			f.pollOne(ctx, d)
		}(d)
	}
	wg.Wait()
}

func (f *Fleet) pollOne(ctx context.Context, d *Device) {
	host, auth := d.Host, d.auth
	st, raw, err := f.client.Status0(ctx, host, auth)
	now := time.Now()
	if err != nil {
		f.mu.Lock()
		d.applyError(err, now)
		f.mu.Unlock()
		return
	}
	fresh := &Device{ID: d.ID, Host: host, Source: d.Source}
	fresh.applyStatus(st, raw, now)
	f.upsert(fresh)
}

// Scan sweeps the configured CIDRs. Hosts that answer Status 0 like a Tasmota
// device join the inventory as discovered devices. It returns how many
// devices were new.
func (f *Fleet) Scan(ctx context.Context) int {
	if !f.scanning.TryLock() {
		return 0
	}
	defer f.scanning.Unlock()
	known := make(map[string]bool)
	f.mu.RLock()
	for _, d := range f.devices {
		known[d.Host] = true
	}
	before := len(f.devices)
	f.mu.RUnlock()

	var hosts []string
	for _, cidr := range f.cfg.Discovery.CIDRs {
		hs, err := config.Hosts(cidr)
		if err != nil {
			log.Warn().Str("cidr", cidr).Err(err).Msg("discovery: bad range")
			continue
		}
		hosts = append(hosts, hs...)
	}
	start := time.Now()
	auth := f.authFor("", "")
	sem := make(chan struct{}, f.cfg.Discovery.Parallel)
	var wg sync.WaitGroup
	for _, h := range hosts {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(h string) {
			defer wg.Done()
			defer func() { <-sem }()
			st, raw, err := f.probe.Status0(ctx, h, auth)
			if err != nil {
				if errors.Is(err, tasmota.ErrUnauthorized) {
					log.Warn().Str("host", h).Msg("discovery: device needs a web password; set tasmota.password or a devices[] entry")
				}
				return
			}
			dev := &Device{ID: idFromHost(h), Host: h, Source: SourceDiscovered, auth: auth}
			dev.applyStatus(st, raw, time.Now())
			f.upsert(dev)
		}(h)
	}
	wg.Wait()
	f.mu.Lock()
	f.lastScan = time.Now()
	after := len(f.devices)
	f.mu.Unlock()
	log.Info().Int("hosts", len(hosts)).Int("devices", after).Int("new", after-before).Dur("took", time.Since(start)).Msg("discovery sweep done")
	return after - before
}

// LastScan is when the last sweep finished (zero if none yet).
func (f *Fleet) LastScan() time.Time {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.lastScan
}

func (f *Fleet) recomputeUpdates() {
	fw := f.fw.get()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, d := range f.devices {
		d.UpdateAvailable = d.Status != nil && fw.updateAvailable(d.Status.Version)
	}
}

// Command sends one command to one device and refreshes it afterwards when
// the command is one that changes state.
func (f *Fleet) Command(ctx context.Context, id, cmd string) (json.RawMessage, error) {
	d, ok := f.deviceRef(id)
	if !ok {
		return nil, ErrNotFound
	}
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return nil, errors.New("command: must not be empty")
	}
	raw, err := f.client.Command(ctx, d.Host, d.auth, cmd)
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(strings.ToLower(cmd), "status") {
		go f.pollOne(context.WithoutCancel(ctx), d)
	}
	return raw, nil
}

// SetPower switches one relay: state is "ON", "OFF" or "TOGGLE".
func (f *Fleet) SetPower(ctx context.Context, id string, relay int, state string) (bool, error) {
	d, ok := f.deviceRef(id)
	if !ok {
		return false, ErrNotFound
	}
	state = strings.ToUpper(strings.TrimSpace(state))
	if state != "ON" && state != "OFF" && state != "TOGGLE" {
		return false, errors.New("state: must be ON, OFF or TOGGLE")
	}
	if relay < 1 || relay > 8 {
		return false, errors.New("relay: must be 1 to 8")
	}
	raw, err := f.client.Command(ctx, d.Host, d.auth, fmt.Sprintf("Power%d %s", relay, state))
	if err != nil {
		return false, err
	}
	_, on, err := tasmota.ParsePowerReply(raw)
	if err != nil {
		return false, err
	}
	f.pollOne(ctx, d)
	return on, nil
}

// Backup downloads one device's configuration.
func (f *Fleet) Backup(ctx context.Context, id string) (string, []byte, error) {
	d, ok := f.deviceRef(id)
	if !ok {
		return "", nil, ErrNotFound
	}
	return f.client.Backup(ctx, d.Host, d.auth)
}

// Restore uploads a configuration dump to one device; the device restarts.
func (f *Fleet) Restore(ctx context.Context, id, filename string, data []byte) error {
	d, ok := f.deviceRef(id)
	if !ok {
		return ErrNotFound
	}
	if len(data) == 0 {
		return errors.New("file: empty")
	}
	return f.client.Restore(ctx, d.Host, d.auth, filename, data)
}

// Restart reboots one device (Restart 1).
func (f *Fleet) Restart(ctx context.Context, id string) error {
	d, ok := f.deviceRef(id)
	if !ok {
		return ErrNotFound
	}
	_, err := f.client.Command(ctx, d.Host, d.auth, "Restart 1")
	return err
}

func (f *Fleet) deviceRef(id string) (*Device, bool) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	d, ok := f.devices[id]
	return d, ok
}

func (f *Fleet) deviceRefs(ids []string) ([]*Device, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	out := make([]*Device, 0, len(ids))
	seen := make(map[string]bool)
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		d, ok := f.devices[id]
		if !ok {
			return nil, fmt.Errorf("device %q: %w", id, ErrNotFound)
		}
		out = append(out, d)
	}
	if len(out) == 0 {
		return nil, errors.New("ids: select at least one device")
	}
	return out, nil
}

// bulkParallel bounds how many devices a job touches at once. OTA downloads
// all come from the same upstream server, so this is also kind to it.
const bulkParallel = 4

// runJob executes fn per device with bounded parallelism and finishes the job.
func (f *Fleet) runJob(j *Job, devs []*Device, fn func(ctx context.Context, d *Device, it *JobItem)) {
	go func() {
		ctx := context.Background()
		sem := make(chan struct{}, bulkParallel)
		var wg sync.WaitGroup
		for i, d := range devs {
			it := j.Items[i]
			wg.Add(1)
			sem <- struct{}{}
			go func(d *Device, it *JobItem) {
				defer wg.Done()
				defer func() { <-sem }()
				f.jobs.set(j, it, "running", "")
				fn(ctx, d, it)
			}(d, it)
		}
		wg.Wait()
		f.jobs.finish(j)
		ok, failed := j.Counts()
		log.Info().Str("job", j.ID).Str("kind", j.Kind).Int("ok", ok).Int("failed", failed).Msg("job finished")
	}()
}

// BulkPower switches relay 1 (or all relays with relay 0) on the selected
// devices.
func (f *Fleet) BulkPower(ids []string, relay int, state string) (Job, error) {
	devs, err := f.deviceRefs(ids)
	if err != nil {
		return Job{}, err
	}
	state = strings.ToUpper(strings.TrimSpace(state))
	if state != "ON" && state != "OFF" && state != "TOGGLE" {
		return Job{}, errors.New("state: must be ON, OFF or TOGGLE")
	}
	if relay < 0 || relay > 8 {
		return Job{}, errors.New("relay: must be 0 (all) or 1 to 8")
	}
	cmd := fmt.Sprintf("Power%d %s", relay, state)
	j := f.jobs.create("power", fmt.Sprintf("Power %s on %d devices", strings.ToLower(state), len(devs)), devs)
	f.runJob(j, devs, func(ctx context.Context, d *Device, it *JobItem) {
		ctx, cancel := context.WithTimeout(ctx, f.cfg.Poll.Timeout*2)
		defer cancel()
		raw, err := f.client.Command(ctx, d.Host, d.auth, cmd)
		if err != nil {
			f.jobs.set(j, it, "failed", err.Error())
			return
		}
		f.jobs.set(j, it, "ok", string(raw))
		f.pollOne(ctx, d)
	})
	return j.snapshot(), nil
}

// BulkCommand sends the same console command to the selected devices.
func (f *Fleet) BulkCommand(ids []string, cmd string) (Job, error) {
	devs, err := f.deviceRefs(ids)
	if err != nil {
		return Job{}, err
	}
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return Job{}, errors.New("command: must not be empty")
	}
	j := f.jobs.create("command", fmt.Sprintf("%q on %d devices", cmd, len(devs)), devs)
	f.runJob(j, devs, func(ctx context.Context, d *Device, it *JobItem) {
		ctx, cancel := context.WithTimeout(ctx, f.cfg.Poll.Timeout*2)
		defer cancel()
		raw, err := f.client.Command(ctx, d.Host, d.auth, cmd)
		if err != nil {
			f.jobs.set(j, it, "failed", err.Error())
			return
		}
		f.jobs.set(j, it, "ok", string(raw))
		if !strings.HasPrefix(strings.ToLower(cmd), "status") {
			f.pollOne(ctx, d)
		}
	})
	return j.snapshot(), nil
}

// Upgrade timing. An ESP8266 flashes a release in well under a minute. An
// ESP32 goes through safeboot: the first Upgrade swaps it into the safeboot
// partition, and a second Upgrade from there writes the real image. Some
// boards go silent for a long time while flashing; they are reported as
// "no answer", not failed, because they usually finish on their own.
const (
	upgradePoll     = 10 * time.Second
	upgradeDeadline = 8 * time.Minute
	safebootGrace   = 2 * time.Minute
)

// BulkUpgrade runs an OTA upgrade on the selected devices. otaURL, when not
// empty, is written as the device's OtaUrl first; otherwise the device's own
// setting (and the configured firmware.otaUrl) decide.
func (f *Fleet) BulkUpgrade(ids []string, otaURL string) (Job, error) {
	devs, err := f.deviceRefs(ids)
	if err != nil {
		return Job{}, err
	}
	otaURL = strings.TrimSpace(otaURL)
	if otaURL == "" {
		otaURL = f.cfg.Firmware.OtaURL
	}
	target := "each device's OtaUrl"
	if otaURL != "" {
		target = otaURL
	}
	j := f.jobs.create("upgrade", fmt.Sprintf("Firmware upgrade of %d devices from %s", len(devs), target), devs)
	f.runJob(j, devs, func(ctx context.Context, d *Device, it *JobItem) {
		f.upgradeOne(ctx, j, d, it, otaURL)
	})
	return j.snapshot(), nil
}

func (f *Fleet) upgradeOne(ctx context.Context, j *Job, d *Device, it *JobItem, otaURL string) {
	host, auth := d.Host, d.auth
	before := tasmota.Version{}
	if d.Status != nil {
		before = d.Status.Version
	}
	short := func(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
		return context.WithTimeout(ctx, timeout)
	}
	if otaURL != "" {
		c, cancel := short(ctx, f.cfg.Poll.Timeout)
		_, err := f.client.Command(c, host, auth, "OtaUrl "+otaURL)
		cancel()
		if err != nil {
			f.jobs.set(j, it, "failed", "set OtaUrl: "+err.Error())
			return
		}
	}
	c, cancel := short(ctx, f.cfg.Poll.Timeout)
	raw, err := f.client.Command(c, host, auth, "Upgrade 1")
	cancel()
	if err != nil {
		f.jobs.set(j, it, "failed", "Upgrade 1: "+err.Error())
		return
	}
	f.jobs.set(j, it, "running", "upgrade started: "+string(raw))

	deadline := time.Now().Add(upgradeDeadline)
	var safebootSince time.Time
	reissued := false
	lastAnswer := time.Now()
	for time.Now().Before(deadline) {
		time.Sleep(upgradePoll)
		c, cancel := short(ctx, f.cfg.Poll.Timeout)
		st, sraw, err := f.client.Status0(c, host, auth)
		cancel()
		if err != nil {
			continue
		}
		lastAnswer = time.Now()
		fresh := &Device{ID: d.ID, Host: host, Source: d.Source}
		fresh.applyStatus(st, sraw, lastAnswer)
		f.upsert(fresh)
		v := st.Version
		switch {
		case v.Safeboot():
			if safebootSince.IsZero() {
				safebootSince = time.Now()
				f.jobs.set(j, it, "running", "in safeboot ("+v.Raw+"), waiting for the second stage")
			} else if !reissued && time.Since(safebootSince) > safebootGrace {
				// The board is sitting in safeboot and not flashing on its own;
				// a second Upgrade 1 from safeboot completes the job.
				c, cancel := short(ctx, f.cfg.Poll.Timeout)
				_, err := f.client.Command(c, host, auth, "Upgrade 1")
				cancel()
				reissued = true
				if err == nil {
					f.jobs.set(j, it, "running", "re-issued Upgrade 1 from safeboot")
				}
			}
		case before.Major != 0 && v.Compare(before) > 0,
			before.Major == 0 && !v.Safeboot():
			f.jobs.set(j, it, "ok", "now "+v.Raw)
			return
		case v.Compare(before) == 0 && v.Variant == before.Variant && time.Since(j.StartedAt) > 2*time.Minute && safebootSince.IsZero():
			// Answering with the old version two minutes in: the download
			// failed (bad OtaUrl, no route, image too large for the flash).
			f.jobs.set(j, it, "failed", "still on "+v.Raw+"; check OtaUrl and the device console")
			return
		}
	}
	if time.Since(lastAnswer) > upgradeDeadline/2 {
		f.jobs.set(j, it, "failed", fmt.Sprintf("no answer for %s. ESP32 boards can keep flashing for 2-3 hours while silent; leave it powered and refresh later", time.Since(lastAnswer).Round(time.Minute)))
		return
	}
	f.jobs.set(j, it, "failed", "upgrade did not complete within "+upgradeDeadline.String())
}

// BackupZip downloads the configuration of every selected device and writes
// one zip archive to w. Failures are listed in a FAILED.txt inside the archive
// rather than aborting the download.
func (f *Fleet) BackupZip(ctx context.Context, ids []string, w io.Writer) error {
	devs, err := f.deviceRefs(ids)
	if err != nil {
		return err
	}
	type result struct {
		name string
		data []byte
		err  error
		dev  *Device
	}
	results := make([]result, len(devs))
	sem := make(chan struct{}, bulkParallel)
	var wg sync.WaitGroup
	for i, d := range devs {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, d *Device) {
			defer wg.Done()
			defer func() { <-sem }()
			c, cancel := context.WithTimeout(ctx, f.cfg.Poll.Timeout*2)
			defer cancel()
			name, data, err := f.client.Backup(c, d.Host, d.auth)
			results[i] = result{name: name, data: data, err: err, dev: d}
		}(i, d)
	}
	wg.Wait()
	zw := zip.NewWriter(w)
	var failed []string
	stamp := time.Now().UTC()
	for _, r := range results {
		if r.err != nil {
			failed = append(failed, fmt.Sprintf("%s (%s): %v", r.dev.DisplayName(), r.dev.Host, r.err))
			continue
		}
		hdr := &zip.FileHeader{Name: r.name, Method: zip.Deflate, Modified: stamp}
		fw, err := zw.CreateHeader(hdr)
		if err != nil {
			return err
		}
		if _, err := fw.Write(r.data); err != nil {
			return err
		}
	}
	if len(failed) > 0 {
		fw, err := zw.CreateHeader(&zip.FileHeader{Name: "FAILED.txt", Method: zip.Deflate, Modified: stamp})
		if err != nil {
			return err
		}
		_, _ = io.WriteString(fw, strings.Join(failed, "\n")+"\n")
	}
	return zw.Close()
}

// newClientFor is a test seam: it builds a device client with a given timeout.
func newClientFor(timeout time.Duration) *tasmota.Client { return tasmota.New(timeout) }
