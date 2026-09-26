# Unraid Internet Monitor — Plan

A small, always-on Docker app for Unraid that continuously measures the quality
of the home internet connection to the ISP (and beyond), keeps 30 days of history,
and serves a web UI reachable on the LAN and over Tailscale.

## 0. Decisions

| Topic | Decision |
|---|---|
| Language | **Go** |
| Speed tests | **Once a day** (default 04:00 local, plus a manual "Run now" button) |
| Alerts | **Discord** webhook (section 3.5), part of v1 |
| IPv6 | **Stubbed**: data model, config, and auto-detect are in place from the start. v6 probing turns on when a global IPv6 default route exists |
| Tailscale | The Unraid host is already on the tailnet, so we use host networking and reach the UI over the host's MagicDNS name (section 9, option 1) |
| Custom targets | Not needed now. They are supported through config (`CUSTOM_TARGETS`), and UI editing comes later |
| Auth | **None** in v1. Access is limited to the LAN plus Tailscale ACLs |

## 1. Goals and non-goals

**Goals**

- Continuously measure latency, jitter, and packet loss, and detect outages, at 1-second resolution.
- Tell *where* a problem is: the LAN or router, the ISP edge, or further upstream.
- Keep 30 days of history (configurable) and keep the database small.
- Stay light: under 1% of one CPU core at idle, about 30 MB RAM, and a single static binary.
- Serve a fast web UI with live and historical charts on the LAN and over Tailscale.
- Deploy on Unraid with one template and keep data in `appdata`.

**Non-goals (for v1)**

- Multi-site or distributed probes
- Authentication of any kind. The app is trusted-network only (LAN plus Tailscale ACLs); basic auth could be added later.
- Long-term (over 1 year) archival. Hourly rollups could add this later.

## 2. Language choice: Go (decided)

Both meet the performance goals easily. The workload is a few packets per second
and one DB write per minute. **Go was chosen.**

| | Go | Rust |
|---|---|---|
| Idle RSS | ~15–30 MB | ~5–10 MB |
| CPU at 5 pps | negligible | negligible |
| ICMP | `golang.org/x/net/icmp`, supports unprivileged ping sockets | `surge-ping` |
| SQLite without CGO | `modernc.org/sqlite` (pure Go, static binary) | `rusqlite` bundled (needs a C toolchain, static musl works) |
| Embed web UI | `embed` stdlib | `rust-embed` |
| Tailscale in-process | **`tsnet`** (native: the app can be its own tailnet node with HTTPS) | not available |
| Build and iteration speed | fast | slower |

Go costs roughly 20 MB more RAM. In return we get a simpler build, a standard
library that covers nearly everything, and `tsnet` as an option later.

## 3. What gets measured

### 3.1 Probe targets (auto-discovered plus configurable)

| Role | Default | Why |
|---|---|---|
| `gateway` | auto (default route) | Separates LAN or router problems from ISP problems |
| `isp` | auto: first hop that is not RFC1918 (CGNAT `100.64/10` counts as ISP) | ISP edge health |
| `internet` | `1.1.1.1`, `8.8.8.8`, `9.9.9.9` | Anycast, three independent operators |
| `custom` | none by default; set via `CUSTOM_TARGETS` (e.g. a game server, work VPN) | Personal relevance |

ISP-hop discovery uses TTL-limited ICMP echoes (TTL 1..6). It runs at startup,
hourly, and after every outage recovery. A change in the ISP hop is logged as an event.

**IPv6 (stubbed).** Every target has an address family (`ip4` or `ip6`). The v6
internet defaults are `2606:4700:4700::1111`, `2001:4860:4860::8888`, and
`2620:fe::fe`. With `IPV6=auto` (the default), v6 targets are probed only when the
host has a global IPv6 default route. Otherwise they are stored as disabled and the
UI shows "IPv6 not available". Outage detection treats v4 and v6 as separate scopes,
so a v6-only failure is shown as a `partial` event.

> Caveat: routers often rate-limit ICMP to their own control plane. Loss to the
> ISP hop alone is shown but **not** treated as an outage unless the downstream
> `internet` targets also lose packets.

### 3.2 Probe types and schedule

| Probe | Default interval | Measures |
|---|---|---|
| ICMP echo | **1/s per target**, 2 s timeout | RTT, loss, jitter (RFC 3550-style mean abs. delta) |
| DNS | 30 s | Resolve time and failures against the system resolver, `1.1.1.1`, and optional ISP resolvers |
| HTTP(S) | 60 s | DNS / TCP connect / TLS / TTFB via `httptrace` to `generate_204` endpoints. Catches problems ICMP misses |
| Public IP | 5 min | Detects ISP reconnects and IP changes (logged as event) |
| Traceroute | on outage start + hourly | Path snapshot for "where did it break" |
| Speed test | **daily at 04:00 local** (configurable, can be disabled) + manual button | Down/up throughput, **latency under load** (bufferbloat grade) |

Steady-state probe traffic is about 5 packets/s, which is under 1 KB/s.

**Speed test.** Implemented natively against Cloudflare's speed endpoints
(`speed.cloudflare.com/__down` and `__up`). It runs multiple parallel streams for a
fixed duration (about 10 s each direction), not a fixed byte count, so it scales to
gigabit links. ICMP to `1.1.1.1` continues during the test to measure loaded
latency. Each result records the bytes used.

> Data usage: one test on 1 Gbps is about 2.5 GB (down + up), so a daily test is
> about 75 GB per month. The schedule and the per-test caps are configurable.
> A random delay of 0–10 min is added to the start time so the test doesn't line up
> with other scheduled jobs.

### 3.3 Derived metrics

- **Loss %**, **RTT min/avg/p50/p95/p99/max**, and **jitter** per target per minute.
- **Quality score (MOS 1–5)** from a simplified ITU-T E-model:
  `eff = avg + 2*jitter + 10`; `R = 93.2 - eff/40` (if `eff < 160`, else `93.2 - (eff-120)/10`);
  `R -= 2.5 * loss%`; `MOS = 1 + 0.035R + 7e-6 * R(R-60)(100-R)`.
- **Uptime %** per day and per range, computed from outage events.

### 3.4 Outage and degradation detection

The detector is a per-second state machine over the ICMP results:

- **Down tick:** all `internet` targets failed in that second.
- **Outage opens** after `N=3` consecutive down ticks and **closes** after `M=3`
  consecutive up ticks. Start and end times are backdated to the first and last failed second.
- **Classification** at open time:
  - gateway down → `local` (LAN or router)
  - gateway up, ISP hop down → `isp_edge`
  - ISP hop up (or unknown), all internet down → `upstream`
  - some internet targets down → `partial` (target-specific, not counted against uptime)
- **Degraded** event: over a rolling 60 s window, loss is 2% or more or p95 RTT is
  above a threshold (default 100 ms, configurable), sustained for at least 2 min.

Each event triggers an immediate traceroute and a DNS check, and stores the results
in the event's `details`.

### 3.5 Alerts (Discord)

Set `DISCORD_WEBHOOK_URL` to turn alerts on. Messages are Discord embeds, colored
red for outages, amber for degraded, green for recovered, and blue for info.

| Alert | Default | Notes |
|---|---|---|
| Outage resolved | on | Duration, classification, affected targets, traceroute summary |
| Outage started | on (best effort) | Only reachable when the outage is `partial` or v6-only. For a full outage it is queued and sent together with the recovery message |
| Degraded started / cleared | on | Loss %, p95 RTT, which targets |
| Public IP changed | on | Old → new |
| ISP hop changed | off | Useful for spotting ISP rerouting |
| Speed test below threshold | on if `ALERT_MIN_DOWN_MBPS` / `ALERT_MIN_UP_MBPS` set | Includes the loaded-latency grade |
| Daily summary | off | Uptime %, outages, avg/p95 latency, loss, speed test result (`ALERT_DAILY_SUMMARY=08:00`) |

**Delivery:** the Discord internet path is unavailable during a real outage, so
alerts go through a small **persistent outbox** (a SQLite table). A sender loop
delivers queued alerts with exponential backoff. It respects Discord's `429
Retry-After`, and it drops alerts older than 24 h with a log line.

**Noise control:**
- Outages shorter than `ALERT_MIN_OUTAGE` (default `30s`) are recorded but not alerted.
- Flaps within `ALERT_COALESCE` (default `5m`) are merged into one message
  ("3 outages in 4 min, 1m12s total").
- Coalescing replaces a separate per-kind cooldown: after an immediate alert,
  more events of the same kind within `ALERT_COALESCE` go into one digest message.
- If the "outage started" alert is still undelivered when the outage ends (the
  usual case for a full outage), it is cancelled. Only the recovery message is
  sent, and it carries the start time and duration.
- Events left open by a crash or restart are closed at startup, at the last
  saved minute, and marked `interrupted`.
- A "Send test alert" button on the Settings page (`POST /api/alerts/test`) checks the webhook.

The notifier sits behind a small `Notifier` interface, so ntfy, Pushover, or a
generic webhook can be added later without touching the detector.

## 4. Architecture

```
┌──────────────────────────── container ─────────────────────────────┐
│                                                                     │
│  Scheduler ──► Probes (icmp | dns | http | ip | trace | speed)      │
│                    │ results (chan)                                 │
│                    ▼                                                │
│             Aggregator ──► in-memory ring buffer (last 1 h, 1 s)    │
│                │    └────► Detector (outage/degraded state machine) │
│                ▼                          │                         │
│      per-minute rollups ──► Store (SQLite, WAL) ◄── events          │
│                                  ▲                                  │
│                    Retention job (hourly, >30 d delete)             │
│                                  │                                  │
│  HTTP server: JSON API + SSE live stream + embedded web UI          │
│  (optional: /metrics Prometheus, tsnet listener)                    │
└─────────────────────────────────────────────────────────────────────┘
```

- **One ICMP socket** is shared by all targets and replies are matched by ID and sequence.
  The app tries an unprivileged ping socket first and falls back to a raw socket.
- **Writes** are batched once a minute in a single transaction, which is friendly to
  SSDs and to Unraid disk spin-down.
- **Live view** is served from the ring buffer (about 350 KB of RAM for 1 h × 12
  series), so it never touches disk.
- **Privilege drop:** the app starts as root, opens the raw socket, and then calls
  `setgid/setuid` to `PUID/PGID` (Unraid default `99:100`) before touching `/data`.

### 4.1 Storage (SQLite)

`modernc.org/sqlite` runs with `journal_mode=WAL`, `synchronous=NORMAL`, and
`auto_vacuum=INCREMENTAL`. Timestamps are stored as UTC unix seconds, and RTTs as ms (`REAL`).

```sql
-- Schema version is tracked with PRAGMA user_version; migrations are append-only.
CREATE TABLE targets (
  id INTEGER PRIMARY KEY,
  key TEXT UNIQUE,                         -- stable identity: gateway/ISP keyed by role, so
                                           -- history survives a router or ISP-hop change
  kind TEXT, role TEXT, name TEXT, address TEXT,
  family TEXT,                             -- ip4 | ip6
  enabled INTEGER DEFAULT 1
);

-- One row per target per minute; used for all probe kinds.
CREATE TABLE probe_minute (
  target_id INTEGER, ts INTEGER,           -- minute start
  sent INTEGER, recv INTEGER,
  rtt_min REAL, rtt_avg REAL, rtt_p50 REAL, rtt_p95 REAL, rtt_p99 REAL, rtt_max REAL,
  jitter REAL,
  PRIMARY KEY (target_id, ts)
) WITHOUT ROWID;

CREATE TABLE http_sample (               -- phase breakdown, 1/min/endpoint
  target_id INTEGER, ts INTEGER, ok INTEGER, status INTEGER,
  dns_ms REAL, connect_ms REAL, tls_ms REAL, ttfb_ms REAL, total_ms REAL,
  PRIMARY KEY (target_id, ts)
) WITHOUT ROWID;

CREATE TABLE events (
  id INTEGER PRIMARY KEY, kind TEXT,       -- outage|degraded|ip_change|isp_hop_change
  scope TEXT, started_at INTEGER, ended_at INTEGER, details TEXT  -- JSON
);
CREATE INDEX events_started ON events(started_at);

CREATE TABLE speedtests (
  id INTEGER PRIMARY KEY, ts INTEGER, down_bps REAL, up_bps REAL,
  idle_rtt REAL, loaded_rtt_down REAL, loaded_rtt_up REAL,
  bytes_used INTEGER, server TEXT, error TEXT
);

CREATE TABLE public_ip (ts INTEGER PRIMARY KEY, ipv4 TEXT, ipv6 TEXT);

CREATE TABLE alert_outbox (              -- persistent queue for Discord
  id INTEGER PRIMARY KEY, created_at INTEGER, kind TEXT, payload TEXT,  -- JSON
  attempts INTEGER DEFAULT 0, next_attempt_at INTEGER, sent_at INTEGER, error TEXT
);
```

**Size estimate (30 days):** about 12 series × 43,200 minutes, or about 520k rows in
`probe_minute`. At 60–80 B/row that is about **40 MB**. Everything else is under 5 MB.

**Retention:** an hourly job runs `DELETE … WHERE ts < now - RETENTION_DAYS` and then
`PRAGMA incremental_vacuum`.

**Downsampling for charts:** the server buckets rows to about 1,000 points per series
per request (`step = range/1000`). For each bucket it uses avg-of-avg,
min-of-min, max-of-max, max-of-p95 (conservative), and summed sent and recv for loss.

## 5. HTTP API

| Method | Path | Purpose |
|---|---|---|
| GET | `/api/status` | Current state: online/outage, per-target last RTT/loss, MOS, public IP |
| GET | `/api/targets` | Configured and discovered targets |
| GET | `/api/metrics?target=&from=&to=&step=` | Downsampled time series |
| GET | `/api/events?from=&to=&kind=` | Outages, degradations, IP changes |
| GET | `/api/speedtests?from=&to=` | Speed test history |
| POST | `/api/speedtest` | Trigger a speed test now |
| POST | `/api/alerts/test` | Send a test Discord message |
| GET | `/api/stream` | **SSE**: 1 s live samples plus event open/close |
| GET | `/api/export.csv?…` | CSV export (e.g. for ISP complaints) |
| GET | `/healthz` | Container healthcheck |
| GET | `/metrics` | Prometheus exposition (optional, off by default) |

## 6. Web UI

The UI is plain HTML with ES modules and **uPlot** (~50 KB, handles 100k+ points
smoothly). It is embedded in the binary with `go:embed`. There is no Node runtime
and ideally no build step. It supports light and dark themes and works on mobile.

1. **Dashboard:** a large status banner (Online / Degraded / Outage since…), per-target
   cards (RTT, loss, jitter, sparkline), a live 15-min latency chart streamed over SSE,
   today's uptime %, MOS, public IP, and the last speed test.
2. **History:** range picker (1 h / 6 h / 24 h / 7 d / 30 d / custom). It shows a latency
   p50/p95 band per target, loss bars, jitter, and outage periods shaded across all charts.
   You can click and drag to zoom.
3. **Events:** a table of outages and degradations with duration, classification, and
   expandable traceroute and DNS details.
4. **Speed tests:** a throughput chart, a loaded vs idle latency chart (bufferbloat grade),
   and a "Run now" button.
5. **Settings:** effective config (read-only in v1), discovered gateway and ISP hop,
   DB size, and retention.

## 7. Configuration

Settings come from environment variables, with an optional `/config/config.yaml` for
targets. Environment variables take precedence.

| Env | Default | |
|---|---|---|
| `LISTEN_ADDR` | `:8765` | Web UI/API bind |
| `DATA_DIR` | `/data` | SQLite DB location |
| `RETENTION_DAYS` | `30` | |
| `PING_TARGETS` | `1.1.1.1,8.8.8.8,9.9.9.9` | Internet targets (v4) |
| `PING_TARGETS_V6` | `2606:4700:4700::1111,2001:4860:4860::8888,2620:fe::fe` | Internet targets (v6) |
| `IPV6` | `auto` | `auto`, `on`, or `off` |
| `CUSTOM_TARGETS` | unset | `name=host,name2=host2`; pinged, charted, never counted as an outage |
| `GATEWAY` | `auto` | Override if auto-detect fails (e.g. bridge networking) |
| `ISP_HOP` | `auto` | `auto`, an IP, or `off` |
| `PING_INTERVAL` | `1s` | |
| `DNS_INTERVAL` / `HTTP_INTERVAL` | `30s` / `60s` | |
| `SPEEDTEST_SCHEDULE` | `04:00` (`off` to disable) | Daily local time, plus 0–10 min jitter |
| `SPEEDTEST_MAX_SECONDS` | `10` | Per direction |
| `OUTAGE_THRESHOLD` | `3s` | Consecutive down ticks |
| `DEGRADED_LOSS_PCT` / `DEGRADED_P95_MS` | `2` / `100` | Over a rolling 60 s window |
| `DEGRADED_MIN` | `2m` | How long the degraded condition must hold |
| `PUID` / `PGID` | `99` / `100` | Unraid `nobody:users` |
| `TZ` | `UTC` | Used for the speed test and summary schedules and for logs. The UI renders in browser local time. Set it to your zone (e.g. `America/Denver`) |
| `DISCORD_WEBHOOK_URL` | unset | Turns on alerts |
| `ALERT_MIN_OUTAGE` / `ALERT_COALESCE` | `30s` / `5m` | Noise control |
| `ALERT_ISP_HOP_CHANGE` | `false` | Alert when the ISP edge router changes |
| `ALERT_MIN_DOWN_MBPS` / `ALERT_MIN_UP_MBPS` | unset | Speed test thresholds |
| `ALERT_DAILY_SUMMARY` | `off` | e.g. `08:00` |
| `PROMETHEUS` | `false` | Enable `/metrics` |

## 8. Deployment on Unraid

**Image:** a multi-stage build that compiles a static Go binary into
`gcr.io/distroless/static` (or `scratch` plus CA certs and tzdata). The target size is
about 15 MB. The multi-arch image (`amd64` and `arm64`) is published to
`ghcr.io/thedatafiend/unraid-internet-monitor`.

**Networking: host mode (recommended).**

- The app sees the real default gateway, so auto-detection works.
- There is no Docker NAT or conntrack in the measurement path.
- The app binds on all host interfaces, including `tailscale0`.
- Bridge mode also works, but the app then needs `GATEWAY=<router IP>` set explicitly,
  and it adds a NAT hop.
- ICMP in host mode needs `NET_RAW`, which is in Docker's default capability set.
  The app opens the socket as root and then drops privileges (section 4).

**Volumes:** `/mnt/user/appdata/internet-monitor` → `/data`. Keep appdata on the
**cache/SSD pool**. The once-a-minute writes would otherwise keep array disks spun up.

**Unraid template:** `unraid/internet-monitor.xml` in this repo. It covers the icon,
WebUI link (`http://[IP]:[PORT:8765]/`), port, path, and the key env vars, and
can be installed with "Add Container → Template". It could be submitted to Community
Applications later.

**Healthcheck:** `/internet-monitor healthcheck` hits `/healthz`. No shell is required.

## 9. Tailscale access

**Decision: option 1.** The Unraid host is already on the tailnet. The other
options are kept here for reference.

1. **Unraid host on the tailnet (chosen):** use the Tailscale plugin or the native
   Tailscale support in Unraid 7. With host networking, the UI is at
   `http://<unraid-magicdns-name>:8765` with no app changes. Use Tailscale ACLs to limit who can reach it.
2. **Unraid 7 per-container Tailscale toggle:** gives the container its own tailnet
   node, and optionally `tailscale serve` for HTTPS. This works best with bridge networking.
3. **Embedded `tsnet` (later, optional):** set `TS_AUTHKEY`, and the app joins the tailnet
   as `internet-monitor` with an automatic HTTPS cert
   (`https://internet-monitor.<tailnet>.ts.net`). This adds about 20 MB of RAM and
   needs no Unraid-side setup.

> Watch out: if the Unraid host uses a Tailscale **exit node**, the probes would
> measure the exit node's path instead of your ISP. The Settings page should warn when
> the default route goes via `tailscale0`.

## 10. Repository layout

```
cmd/internet-monitor/main.go      # flags/env, wiring, privilege drop, healthcheck subcommand
internal/config/                  # env + yaml parsing, validation
internal/probe/                   # icmp.go dns.go http.go publicip.go trace.go speedtest.go
internal/discover/                # gateway + ISP hop discovery
internal/aggregate/               # 1s ring buffer, per-minute rollups, percentiles, MOS
internal/alert/                   # Notifier interface, Discord embeds, outbox sender, coalescing
internal/detect/                  # outage/degraded state machine
internal/store/                   # sqlite, migrations, queries, retention
internal/api/                     # handlers, SSE hub, CSV export, basic auth
web/                              # index.html, *.js, *.css, vendor/uplot (go:embed)
unraid/internet-monitor.xml       # Unraid Docker template
Dockerfile
.github/workflows/                # test + lint; build & push multi-arch image on tag
docs/PLAN.md
```

## 11. Milestones

| # | Milestone | Deliverable |
|---|---|---|
| **M0** | Spikes (½ day) | Confirm the privilege-drop plus raw-socket approach in host mode on Unraid. Confirm the ISP-hop discovery heuristic on the real network. |
| **M1** | Core engine | ICMP prober (multi-target, one socket, v4 now, v6 stubbed), gateway/ISP discovery, ring buffer, per-minute rollups, SQLite store plus retention, `/api/status` and `/api/metrics`. Unit tests for aggregation and percentiles. |
| **M2** | Detection + Discord | Outage/degraded state machine plus classification and the events table. Discord notifier with outbox, coalescing, and a test button. Table-driven tests with synthetic tick streams. |
| **M3** | Web UI v1 | Dashboard (SSE live chart), History, and Events pages. |
| **M4** | Ship it | Dockerfile, Unraid template, GitHub Actions multi-arch build to GHCR. **Deploy on Unraid and start collecting data.** |
| **M5** | More probes | DNS, HTTP phase timing, public IP tracking (plus alert), traceroute on outage. |
| **M6** | Speed tests | Daily Cloudflare-based test with loaded latency and bufferbloat grade, a manual run button, a UI page, and threshold alerts. |
| **M7** | Nice-to-haves | Live IPv6 probing (if the ISP supports it), custom targets editable in the UI, daily Discord summary, Prometheus `/metrics`, CSV export, hourly rollups for 1-year history, optional basic auth, other notifiers (ntfy/Pushover). |

Deploying at M4 means real data accumulates while M5–M7 are built.

### Progress

| # | State | Notes |
|---|---|---|
| M0 | **Done** | `internet-monitor diag` checks every M0 assumption. On the Unraid server with `--network host`: the raw socket opened as root and the app dropped to `99:100`. The kernel route lookup found the gateway, and the one-burst traceroute found an ISP edge router that answers pings. All targets showed 0% loss. The ISP has no IPv6. Discovery asks the kernel for the real egress route over netlink (`ip route get`), so policy routing (Tailscale, VPNs) is handled and flagged. |
| M1 | **Done** | Engine, discovery, ring buffer, per-minute rollups (merged safely across restarts), SQLite with retention, and `/api/status`, `/api/targets`, `/api/metrics`, `/api/live`, `/healthz`. There is a temporary status page at `/`. Measured: about 15 MB RSS and about 0.1% CPU with 6 targets at 1 pps; the image is 22 MB. |
| M2 | **Done** | Pure-function detector (outage with local / ISP-edge / upstream classification, single-target and IPv6 partials, degraded by loss or p95), events table, `/api/events`, `/api/uptime`, 24 h uptime in `/api/status`. Discord alerts through a SQLite outbox with retry, supersede, coalescing and a test button. Tested end to end with iptables: blocked ICMP, Discord unreachable during the outage, flapping, and `kill -9` mid-outage. |
| M3 | Next | Web UI: dashboard, history and events pages |

## 12. Resource budget and how it is verified

| Metric | Budget | How it is checked |
|---|---|---|
| RSS | ≤ 30 MB | `docker stats` after 24 h |
| CPU | < 1% of one core at idle | `docker stats` / `pidstat` |
| DB size | < 100 MB at 30 days | Settings page shows size |
| Disk writes | 1 txn/min | WAL checkpoint stats |
| Image | ≤ 20 MB | CI reports image size |
| UI load | < 200 KB transferred, 30-day chart < 500 ms | Browser devtools |

## 13. Open questions

All of the initial questions are answered (section 0). Two remain, to be confirmed
during M0:

1. **Does the ISP provide IPv6?** `IPV6=auto` will tell us on first start, and the
   Settings page will show the result.
2. **Is the ISP hop reachable by ping?** Some ISPs filter ICMP to their edge routers.
   If so, `isp_edge` classification falls back to `upstream`.
