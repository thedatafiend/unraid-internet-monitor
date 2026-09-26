# unraid-internet-monitor

A lightweight Docker app for Unraid that continuously measures internet connection
quality to your ISP and beyond. It tracks latency, jitter, packet loss and outages,
keeps 30 days of history, and serves a web UI you can reach on your LAN and over
Tailscale.

The design is in [docs/PLAN.md](docs/PLAN.md).

**Status:** M1 (core engine) is done. It pings your gateway, your ISP's edge
router, and public anycast resolvers once a second, keeps a 1-hour live buffer,
writes per-minute rollups to SQLite, prunes data past the retention window, and
serves a JSON API with a temporary status page. The outage detector, Discord
alerts and the real dashboard come next (M2–M3).

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

## API

| Endpoint | |
|---|---|
| `GET /api/status` | Live state, provisional online/offline, MOS, per-target 60 s stats, discovery info and warnings |
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
