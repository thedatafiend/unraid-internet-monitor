# unraid-internet-monitor

A lightweight Docker app for Unraid that continuously measures internet connection
quality to your ISP and beyond. It tracks latency, jitter, packet loss and outages,
keeps 30 days of history, and serves a web UI you can reach on your LAN and over
Tailscale.

The design is in [docs/PLAN.md](docs/PLAN.md).

**Status:** M2 is done. The app pings your router, your ISP's edge router and
public resolvers once a second. It detects outages (and says whether the break is
your LAN, your ISP's connection, or further upstream), slowdowns, and single-target
failures. It keeps 30 days of history in SQLite and sends Discord alerts. There is
a temporary status page at `/`; the real dashboard comes next (M3).

## Try it on Unraid

There is no published image yet (the GitHub Actions build comes in M4), so build
it on the server from an Unraid terminal:

```sh
cd /tmp
git clone -b claude/internet-quality-monitor-app-mnglkp https://github.com/thedatafiend/unraid-internet-monitor.git
cd unraid-internet-monitor
docker build -t internet-monitor .
```

### 1. Check that monitoring works on your network (milestone M0)

```sh
docker run --rm --network host internet-monitor diag
```

`diag` reports the socket type, the privilege drop, the detected gateway and ISP
hop, a traceroute, IPv6 availability, and a 10-ping test to every target. What to
look for:

- `ICMP socket v4: raw` and `uid/gid after drop: 99/100`
- `egress route:` names your LAN interface (usually `br0` or `eth0`) and
  `tunnel in path: none`. If it names `tailscale0` or a `wg` interface, the
  server's internet traffic goes through a Tailscale exit node or VPN. The
  measurements would then describe that tunnel, not your ISP.
- `gateway:` shows your router's LAN IP
- `ISP hop:` shows a public or `100.64.x.x` address. If it says `none`, your ISP
  filters these probes. Set `ISP_HOP=off`, or set it to a hop you trust.
- The internet targets show 0% loss

### 2. Run it

```sh
docker run -d --name internet-monitor --restart unless-stopped \
  --network host \
  -v /mnt/user/appdata/internet-monitor:/data \
  -e TZ=America/Denver \
  internet-monitor
```

Open `http://<unraid-ip>:8765`. Because the Unraid host is on your tailnet, the same
page is also at `http://<unraid-tailscale-name>:8765`.

Keep the `appdata` share on the cache/SSD pool. The app writes about once a
minute, which would otherwise keep array disks spinning.

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
| `GET /healthz` | 200 once probes are running. Used by the Docker healthcheck |

## Development

```sh
go test ./...
go build ./cmd/internet-monitor
sudo ./internet-monitor diag     # raw ICMP needs root or CAP_NET_RAW
```

Without root, the app falls back to unprivileged ping sockets when the host
allows them (`net.ipv4.ping_group_range`). In that mode ISP-hop auto-detection is
unavailable.
