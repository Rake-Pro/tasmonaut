# Design

## Problem

TasmoAdmin covered the fleet jobs (see what is on the network, flip relays, send a console command to many devices, upgrade firmware in bulk, download configuration dumps) but it is PHP, and the Home Assistant add-on build lagged a PHP security release by weeks. The fleet is 17 devices on one VLAN; none of TasmoAdmin's device database, user accounts or scheduled tasks were in use.

## Decisions

| Decision | Choice | Why |
|---|---|---|
| State | None on disk. Inventory = configuration + discovery, held in memory | The devices are the source of truth; a sweep rebuilds everything in seconds. No volume, no backup of the tool itself, free rescheduling |
| Identity | Devices keyed by MAC once polled; by host until then | DHCP changes and a configured device that moves IP stay one entry |
| Device protocol | Tasmota HTTP API only (`/cm`, `/dl`, `/u2`) | Works without MQTT; one route to the VLAN is the only dependency |
| Discovery | Parallel `Status 0` over configured CIDRs, 3 s per host, /22 cap | 254 hosts take about 12 s at 64 parallel; `StatusFWR` present = Tasmota |
| Upgrades | Per device: optional `OtaUrl`, `Upgrade 1`, poll `Status 0`; safeboot second stage after 2 min; silent boards reported, not failed | Learned on the real fleet: a POWR320D needs the second `Upgrade 1`, a T-Relay goes silent for two hours and completes by itself |
| Backups | Streamed to the browser, zip for many | The tool holds nothing; the operator's backup location is the operator's |
| Auth | None in the app. The gpm SSO gate (Authentik forward auth) fronts it; the proxy's username header is read for display and logs. `net/http` CrossOriginProtection refuses cross-site mutating requests | Owner decision 2026-10-22: one admin tool for one household does not need its own OIDC client; the gate is already there for every internal app |
| UI | Server-rendered shell, vanilla page script, drift stylesheet verbatim, self-hosted fonts | No build step, strict CSP (`script-src 'self'`), zero third-party requests |
| Jobs | In-memory, newest 50, four devices in flight | Enough for a home fleet; results are read once |

## Layout

```
cmd/tasmonaut        main: flags, logger, lifecycle
internal/config      YAML + TASMONAUT_* env, validation, CIDR expansion
internal/tasmota     device client: Command, Status0, Backup, Restore, version parsing
internal/fleet       inventory, discovery sweep, poller, firmware reference, jobs, bulk actions
internal/server      routes, middleware (recover, access log, security headers, proxy identity, cross-origin protection), pages, JSON API
web/templates        layout + one template per page
web/static           drift.css (design system), app.css, app.js, theme-init.js, fonts
```

## Flows

- Startup: fetch latest release (optional), poll configured devices, sweep CIDRs if `discovery.atStartup`, then serve. The first page already has data.
- Poll: every `poll.interval`, all devices, `poll.parallel` at a time. A failed poll marks the device offline but keeps its last status so the page still names it.
- Power from the UI: `Power<n> TOGGLE`, reply parsed, then an immediate re-poll so the row shows the real state.
- Bulk: a job is created with one item per device and runs detached; pages poll `/api/jobs/{id}` every 3 s until `finishedAt` is set.

## Deployment

Single replica (jobs are in memory). The pod needs a route to the device VLAN and, when `firmware.checkLatest` is on, egress to api.github.com. The Ingress must carry the SSO gate; the listener itself trusts everyone. Secrets: optional device web password. No PVC.

## Not built (on purpose)

- Device groups, templates, scheduled commands, user management, per-device notes: no need in the fleet this replaces.
- MQTT transport: would add a broker dependency for nothing the HTTP API cannot do here.
- Persisting manual additions: put them in the configuration.
