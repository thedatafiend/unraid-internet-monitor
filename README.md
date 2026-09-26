# unraid-internet-monitor

A lightweight Docker app for Unraid that continuously measures internet connection
quality to your ISP and beyond. It tracks latency, jitter, packet loss and outages,
keeps 30 days of history, and serves a web UI you can reach on your LAN and over
Tailscale.

The design is in [docs/PLAN.md](docs/PLAN.md).

**Status:** M3 is done. The app pings your router, your ISP's edge router and
public resolvers once a second. It detects outages (and says whether the break is
your LAN, your ISP's connection, or further upstream), slowdowns, and single-target
failures. It keeps 30 days of history in SQLite and sends Discord alerts.

The web UI has four pages:

- **Dashboard:** live state, uptime, latency, loss, call quality (MOS), a live
  15-minute chart, per-target stats and recent events.
- **History:** 1 hour to 30 days of latency, packet loss and jitter, with outages
  shaded. Drag across a chart to zoom in, pick one target for its median/p95 band,
  or switch to a table view.
- **Events:** every outage and slowdown, with uptime and downtime totals.
- **Settings:** the discovered route, the configuration and a Discord test button.

The UI follows your light or dark theme and works on a phone. Everything is served
from the container itself, so it keeps working while your internet is down.

## Install on Unraid

The image is built by GitHub Actions and published to
`ghcr.io/thedatafiend/unraid-internet-monitor` for amd64 and arm64.

1. **If you started it by hand before**, remove that container first. Your
   history in `appdata` is kept:
   ```sh
   docker rm -f internet-monitor
   ```
2. **Add the template** from an Unraid terminal:
   ```sh
   wget -O /boot/config/plugins/dockerMan/templates-user/my-internet-monitor.xml \
     https://raw.githubusercontent.com/thedatafiend/unraid-internet-monitor/HEAD/unraid/internet-monitor.xml
   ```
3. **Create the container:** go to **Docker → Add Container**, pick **internet-monitor** from
   the *Template* list (under *User templates*), paste your Discord webhook URL if you
   want alerts, and click **Apply**. The defaults (host networking, data in
   `/mnt/user/appdata/internet-monitor`, port 8765) suit most setups. Tuning options are
   under *Show more settings*.
4. **Open the UI** from the container's icon (**WebUI**), or go to `http://<unraid-ip>:8765`.
   Because the Unraid host is on your tailnet, it's also at
   `http://<unraid-tailscale-name>:8765`.

**Updating:** on the Docker tab, click **Check for Updates**, then **apply update**.

Keep the `appdata` share on the cache/SSD pool. The app writes about once a minute,
which would otherwise keep array disks spinning.

### Image tags

| Tag | What it is |
|---|---|
| `latest` | The repository's default branch |
| `1.2.3`, `1.2` | Release tags (`v1.2.3`) |
| `sha-abc1234` | A specific commit |
| `<branch-name>` | The tip of any other branch |

### Checking the setup

`diag` prints what the monitor sees: socket type, privilege drop, egress route,
gateway, ISP edge router, a traceroute and a 10-ping test to every target.

```sh
docker run --rm --network host ghcr.io/thedatafiend/unraid-internet-monitor diag
```

Look for `ICMP socket v4: raw`, `uid/gid after drop: 99/100`, your LAN interface
and `tunnel in path: none`, your router as the gateway, and 0% loss to the
internet targets. If `ISP hop:` says `none`, your ISP filters those probes; set
`ISP_HOP=off`.

## Discord alerts

1. In Discord, open the channel's **Edit Channel → Integrations → Webhooks → New
   Webhook**, then **Copy Webhook URL**.
2. Pass it to the container as `-e DISCORD_WEBHOOK_URL=https://discord.com/api/webhooks/...`.
   Treat the URL like a password: anyone who has it can post to the channel.
3. Open the status page and click **Send test alert**, or run
   `curl -X POST http://<unraid-ip>:8765/api/alerts/test`.

What you get:

- **Outage:** after 30 s, and again on recovery with the duration and likely
  cause. When the whole internet is down, Discord is unreachable too, so you get
  a single "restored" message once the connection is back.
- **Degraded:** loss of at least 2%, or p95 latency over 100 ms, sustained for 2
  minutes. You get a message when it starts and when it clears.
- **Flapping:** several outages in quick succession are batched into one digest
  instead of a burst of messages.

## Configuration

All settings are environment variables. The full list, including the ones planned
for later milestones, is in [section 7 of the plan](docs/PLAN.md#7-configuration).

| Variable | Default | |
|---|---|---|
| `LISTEN_ADDR` | `:8765` | Web UI/API address |
| `DATA_DIR` | `/data` | SQLite database directory |
| `RETENTION_DAYS` | `30` | |
| `PING_TARGETS` | `1.1.1.1,8.8.8.8,9.9.9.9` | Internet targets (IPs or hostnames) |
| `PING_TARGETS_V6` | `2606:4700:4700::1111,2001:4860:4860::8888,2620:fe::fe` | Used only when IPv6 is available |
| `IPV6` | `auto` | `auto`, `on` or `off` |
| `GATEWAY` | `auto` | `auto`, `off`, or your router's IP |
| `ISP_HOP` | `auto` | `auto`, `off`, or an IP |
| `CUSTOM_TARGETS` | unset | `name=host,name2=host2`, charted but never counted as an outage |
| `PING_INTERVAL` / `PING_TIMEOUT` | `1s` / `2s` | |
| `PUID` / `PGID` | `99` / `100` | User the app switches to after opening its ICMP socket. `PUID=0` stays root |
| `OUTAGE_THRESHOLD` | `3s` | All internet targets down this long counts as an outage |
| `DEGRADED_LOSS_PCT` / `DEGRADED_P95_MS` / `DEGRADED_MIN` | `2` / `100` / `2m` | Degraded-connection thresholds |
| `DISCORD_WEBHOOK_URL` | unset | Turns on alerts |
| `ALERT_MIN_OUTAGE` | `30s` | Shorter outages are recorded but not alerted |
| `ALERT_COALESCE` | `5m` | Window for batching repeated alerts into a digest |
| `ALERT_ISP_HOP_CHANGE` | `false` | Alert when your ISP's edge router changes |

## API

| Endpoint | |
|---|---|
| `GET /api/status` | State (`online`/`degraded`/`outage`) and since when, open events, 24 h uptime, MOS, per-target 60 s stats, discovery info and warnings |
| `GET /api/events?from=&to=&kind=` | Outages, partial failures, degradations and ISP-hop changes (default: last 7 days) |
| `GET /api/uptime?from=&to=` | Uptime %, downtime, outage count and longest outage (default: last 24 h) |
| `POST /api/alerts/test` | Send a test Discord message |
| `GET /api/targets` | All targets, including disabled ones that still have history |
| `GET /api/metrics?target=ID&from=&to=&step=` | Stored per-minute history as parallel arrays, downsampled to at most 1000 points |
| `GET /api/live?seconds=900` | Per-second RTTs from memory (up to 1 h) |
| `GET /api/stream` | Server-sent events: one message per second with every target's RTT and the current state |
| `GET /api/config` | Effective settings (the webhook URL is reported only as set or unset) |
| `GET /healthz` | 200 once probes are running. Used by the Docker healthcheck |

## Development

```sh
go test ./...
go build ./cmd/internet-monitor
sudo ./internet-monitor diag     # raw ICMP needs root or CAP_NET_RAW
docker build -t internet-monitor .
```

CI (`.github/workflows/ci.yml`) checks formatting, runs `go vet` and the tests
with the race detector, and then builds and pushes the multi-arch image. Pull
requests only run the checks. To cut a release, push a tag such as `v1.0.0`.

Without root, the app falls back to unprivileged ping sockets when the host
allows them (`net.ipv4.ping_group_range`). In that mode ISP-hop auto-detection is
unavailable.
