# Unraid Internet Monitor — Plan

A small, always-on Docker app for Unraid that continuously measures the quality
of the home internet connection to the ISP (and beyond), keeps 30 days of history,
and serves a web UI reachable on the LAN and over Tailscale.

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
- User accounts or RBAC. The app is trusted-network only, with optional basic auth.
- Long-term (over 1 year) archival. Hourly rollups could add this later.

## 2. Language choice: Go (recommended) vs Rust

Both meet the performance goals easily. The workload is a few packets per second
and one DB write per minute. **Recommendation: Go.**

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
| `custom` | user-defined (e.g. a game server, work VPN) | Personal relevance |

ISP-hop discovery uses TTL-limited ICMP echoes (TTL 1..6). It runs at startup,
hourly, and after every outage recovery. A change in the ISP hop is logged as an event.

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
| Speed test | **every 6 h** (configurable, can be disabled) + manual button | Down/up throughput, **latency under load** (bufferbloat grade) |

Steady-state probe traffic is about 5 packets/s, which is under 1 KB/s.

**Speed test.** Implemented natively against Cloudflare's speed endpoints
(`speed.cloudflare.com/__down` and `__up`). It runs multiple parallel streams for a
fixed duration (about 10 s each direction), not a fixed byte count, so it scales to
gigabit links. ICMP to `1.1.1.1` continues during the test to measure loaded
latency. Each result records the bytes used.

> Data usage warning: one test on 1 Gbps is about 2.5 GB (down + up). Every 6 h
> that adds up to about 300 GB per month. The interval and the per-test caps are
> configurable. Default off, or every 6 h? → see open questions.

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
CREATE TABLE targets (
  id INTEGER PRIMARY KEY, kind TEXT, role TEXT, name TEXT, address TEXT,
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
CREATE TABLE schema_version (version INTEGER);
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
| `PING_TARGETS` | `1.1.1.1,8.8.8.8,9.9.9.9` | Internet targets |
| `GATEWAY` | `auto` | Override if auto-detect fails (e.g. bridge networking) |
| `ISP_HOP` | `auto` | `auto`, an IP, or `off` |
| `PING_INTERVAL` | `1s` | |
| `DNS_INTERVAL` / `HTTP_INTERVAL` | `30s` / `60s` | |
| `SPEEDTEST_INTERVAL` | `6h` (`0` = off) | |
| `SPEEDTEST_MAX_SECONDS` | `10` | Per direction |
| `OUTAGE_THRESHOLD` | `3s` | Consecutive down ticks |
| `DEGRADED_LOSS_PCT` / `DEGRADED_P95_MS` | `2` / `100` | |
| `PUID` / `PGID` | `99` / `100` | Unraid `nobody:users` |
| `TZ` | `UTC` | Only affects logs; the UI renders in browser local time |
| `BASIC_AUTH` | unset | `user:bcrypt-hash`, optional |
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

These options are listed in order of simplicity:

1. **Unraid host on the tailnet (recommended):** use the Tailscale plugin or the native
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
| **M1** | Core engine | ICMP prober (multi-target, one socket), gateway/ISP discovery, ring buffer, per-minute rollups, SQLite store plus retention, `/api/status` and `/api/metrics`. Unit tests for aggregation and percentiles. |
| **M2** | Detection | Outage/degraded state machine plus classification and the events table. Table-driven tests with synthetic tick streams. |
| **M3** | Web UI v1 | Dashboard (SSE live chart), History, and Events pages. |
| **M4** | Ship it | Dockerfile, Unraid template, GitHub Actions multi-arch build to GHCR. **Deploy on Unraid and start collecting data.** |
| **M5** | More probes | DNS, HTTP phase timing, public IP tracking, traceroute on outage. |
| **M6** | Speed tests | Cloudflare-based test with loaded latency and bufferbloat grade, plus scheduled and manual runs and a UI page. |
| **M7** | Nice-to-haves | Alerts (ntfy / Discord / Gotify / generic webhook), Prometheus `/metrics`, CSV export, IPv6 targets, hourly rollups for 1-year history, embedded `tsnet`, editable settings in the UI. |

Deploying at M4 means real data accumulates while M5–M7 are built.

## 12. Resource budget and how it is verified

| Metric | Budget | How it is checked |
|---|---|---|
| RSS | ≤ 30 MB (≤ 50 MB with tsnet) | `docker stats` after 24 h |
| CPU | < 1% of one core at idle | `docker stats` / `pidstat` |
| DB size | < 100 MB at 30 days | Settings page shows size |
| Disk writes | 1 txn/min | WAL checkpoint stats |
| Image | ≤ 20 MB | CI reports image size |
| UI load | < 200 KB transferred, 30-day chart < 500 ms | Browser devtools |

## 13. Open questions

1. **Go or Rust?** The plan assumes Go (section 2).
2. **Speed tests:** is there a data cap? Should the default be every 6 h, daily, or manual only?
3. **Alerts:** are notifications wanted in v1? If so, which channel (ntfy, Discord, Pushover, email)?
4. **IPv6:** does the ISP provide IPv6? If so, dual-stack targets move up from M7.
5. **Tailscale:** is the Unraid host already on the tailnet (option 1), or should the app be its own node?
6. **Auth:** is LAN plus Tailscale ACLs enough, or should basic auth be on by default?
7. **Custom targets:** are there specific hosts to watch (work VPN, game servers, a VPS)?
