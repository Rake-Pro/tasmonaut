# tasmonaut

Fleet console for [Tasmota](https://tasmota.github.io/docs/) devices, in one Go binary with no database and no disk state. A replacement for the parts of TasmoAdmin a home fleet actually uses, without PHP.

## What it does

| Area | Feature |
|---|---|
| Inventory | Configured device list plus subnet discovery (Tasmota HTTP API sweep); re-keyed by MAC so a DHCP change is not a new device |
| Status | `Status 0` polling: name, module, hardware, firmware, relays, Wi-Fi signal, uptime, energy metering, MQTT |
| Power | Per-relay switches on the list and the device page; bulk on/off |
| Console | Any Tasmota command to one device (replies shown) or to many (results on a job page) |
| Firmware | "Update available" against the newest GitHub release (or a pinned version); OTA upgrade singly or in bulk, with the ESP32 safeboot second stage handled |
| Configuration | Download one device's dump, download many as one zip, restore a dump to a device (typed confirmation) |
| Auth | None of its own: it runs behind the SSO gate of the reverse proxy (Authentik forward auth via go-proxy-manager) and shows the username the proxy passes. Cross-site mutating requests are refused (`net/http` CrossOriginProtection), so the proxy session cookie cannot be ridden by another site |

## What it deliberately does not do

- Store anything. Devices added through the UI last until the process restarts; put them in the configuration to keep them. Jobs (bulk results) are kept in memory, newest fifty.
- Hold backups. Configuration dumps stream to the browser; keep them where you keep backups.
- Talk MQTT. Everything goes over each device's HTTP API, so the only requirement is a route from the container to the device network.
- Authenticate. Every request that reaches the listener is trusted; never expose the port without an authenticating proxy in front.

## Run

```
cp config.example.yaml config.yaml   # edit: discovery.cidrs, devices
make run                             # port 8080, no auth: local only
```

Container:

```
docker run --rm -p 127.0.0.1:8080:8080 \
  -e TASMONAUT_DISCOVERY_CIDRS=10.0.4.0/24 \
  ghcr.io/rake-pro/tasmonaut:latest
```

The image is distroless, runs as nonroot and has no volume. In Kubernetes nothing beyond the Deployment, a Service and an Ingress behind the SSO gate is needed; the pod must be able to reach the device VLAN.

## Configuration

Every key in `config.example.yaml` has a `TASMONAUT_*` environment override; the environment wins. The table lists the ones that need a decision.

| Key | Env | Default | Meaning |
|---|---|---|---|
| `discovery.cidrs` | `TASMONAUT_DISCOVERY_CIDRS` | none | IPv4 ranges to sweep (comma-separated, /22 or smaller each) |
| `discovery.interval` | `TASMONAUT_DISCOVERY_INTERVAL` | `15m` | Re-sweep period; `0` = on demand only |
| `poll.interval` | `TASMONAUT_POLL_INTERVAL` | `60s` | Status refresh period |
| `devices` | `TASMONAUT_DEVICES` | none | Always-present devices; per-device name and web password in the file form |
| `tasmota.password` | `TASMONAUT_DEVICE_PASSWORD` | none | Fleet-wide Tasmota `WebPassword` |
| `firmware.checkLatest` | `TASMONAUT_FIRMWARE_CHECK_LATEST` | `true` | Ask GitHub hourly for the newest release (HTTPS) |
| `firmware.latest` | `TASMONAUT_FIRMWARE_LATEST` | none | Pin the reference version instead |
| `firmware.otaUrl` | `TASMONAUT_FIRMWARE_OTA_URL` | none | Written to every device before an upgrade; empty keeps each device's own `OtaUrl` |
| `firmware.mirror.listen` | `TASMONAUT_FIRMWARE_MIRROR_LISTEN` | none | Second, plain-HTTP listener for the firmware mirror (`:8081`); see "Firmware mirror" |
| `firmware.mirror.url` | `TASMONAUT_FIRMWARE_MIRROR_URL` | none | How the devices reach that listener (`http://10.0.2.10:8081`) |
| `identityHeader` | `TASMONAUT_IDENTITY_HEADER` | `X-Authentik-Username` | Proxy header with the signed-in user, shown in the masthead and the access log |

## HTTP API

The page script uses it; it is plain JSON and usable with curl through the SSO gate. Browser requests from another origin are refused on mutating routes.

| Method | Path | Does |
|---|---|---|
| GET | `/api/devices` | Inventory, reference firmware, last scan time |
| POST | `/api/devices` `{host}` | Probe and add a device (until restart) |
| GET | `/api/devices/{id}` | One device with its raw `Status 0` |
| DELETE | `/api/devices/{id}` | Remove from the inventory |
| POST | `/api/devices/{id}/refresh` | Poll now |
| POST | `/api/devices/{id}/power` `{relay, state}` | `ON`, `OFF` or `TOGGLE` one relay |
| POST | `/api/devices/{id}/command` `{command}` | Console command, reply returned |
| POST | `/api/devices/{id}/restart` | `Restart 1` |
| GET | `/api/devices/{id}/backup` | Configuration dump download |
| POST | `/api/devices/{id}/restore` (multipart `file`) | Configuration upload; the device restarts |
| POST | `/api/scan` | Start a discovery sweep |
| POST | `/api/refresh` | Poll every device now |
| GET | `/api/firmware` | Reference version |
| POST | `/api/bulk/power` `{ids, relay, state}` | Job: relay 1 (or `0` = all) on each device |
| POST | `/api/bulk/command` `{ids, command}` | Job: same command to each device |
| POST | `/api/bulk/upgrade` `{ids, otaUrl}` | Job: OTA upgrade, four devices at a time |
| POST | `/api/bulk/backup` `{ids}` | Zip of configuration dumps |
| GET | `/api/jobs`, `/api/jobs/{id}` | Job list and detail |

## Firmware upgrades

- Without the firmware mirror, the device's own `OtaUrl` is used unless an override is given; ESP8266 and ESP32 release channels have different URLs, so a fleet-wide override only makes sense for a single-platform fleet.
- ESP32 boards flash in two stages through the `safeboot` partition. The job waits, re-issues `Upgrade 1` once if a board sits in safeboot for two minutes, and reports a board that goes silent as "no answer" rather than failed: some boards flash for a long time without answering and finish on their own. Do not power-cycle those early.
- Upgrade multi-relay boards that control real machinery one at a time and when nothing is running: every relay drops during the reboot.

## Firmware mirror

- Why: stock ESP8266 Tasmota can only download an OTA image over plain HTTP, and ESP32 Tasmota does HTTPS without checking the certificate. With the default `OtaUrl` every upgrade crosses the internet unprotected.
- What: tasmonaut downloads the image from the Tasmota GitHub release over verified HTTPS (redirects that leave HTTPS are refused) and serves it to the devices. The only plain-HTTP hop left is device to tasmonaut on the local network.

| Item | Behaviour |
|---|---|
| Enable | Set `firmware.mirror.listen` and `firmware.mirror.url` together |
| Listener | Separate port, plain HTTP, no login; answers only `GET /firmware/<image>`, nothing of the console or API |
| Images | Release OTA images by name (`tasmota.bin.gz`, `tasmota-minimal.bin.gz`, `tasmota32.bin`, `tasmota32-safeboot.bin`, ...), from the reference release (`firmware.checkLatest` or `firmware.latest`) |
| Upgrade | Each device's `OtaUrl` is rewritten to `<mirror.url>/firmware/<its current image name>` and stays there, so ESP8266 and ESP32 devices and build variants all work through one mirror |
| Refused | A device whose `OtaUrl` does not name a release image, or that already runs the reference release or newer, fails its job item before anything is sent to it |
| Limits | 8 downloads at a time; more get `503` |
| Override | An `OtaUrl` override in the upgrade dialog bypasses the mirror for that job |
| Storage | The last 8 images, in memory |

How to expose it:

1. Publish the mirror port to the device network only (a dedicated Service, not the Ingress that carries the SSO gate).
2. Allow the device network to reach that address and port, nothing else.
3. Set `firmware.mirror.url` to that address, as an IP or a name the devices resolve.

## Development

```
make vet test build
```

- Go standard library plus zerolog (logging) and go.yaml.in/yaml (config). Dependencies are vendored.
- `internal/tasmota` is the device client (one device, no state). `internal/fleet` is the inventory, discovery, poller and jobs. `internal/server` is pages plus JSON API; `web/` holds the templates and the drift stylesheet.
- Tests run against `httptest` fake devices built from real `Status 0` captures in `internal/tasmota/testdata`.

## Release flow

Dependabot PRs land on `dev`; CI builds and tests. The sync workflow opens a "Merge dev to main" PR; merging it mints the next semver tag and publishes `ghcr.io/rake-pro/tasmonaut:<version>`. Label the promotion PR `release:minor` or `release:major` to change the bump.

## License

MIT, see `LICENSE`. Fonts under `web/static/fonts` are Red Hat Text, Display and Mono (OFL 1.1, license alongside).
