**English** | [简体中文](README.zh-CN.md)

# MCNodeSelector (MCNS)

**Version 1.0** · short name **MCNS** · A Minecraft FRP node selector & local forwarder.

It probes every configured node, scores them (latency / P95 / jitter / loss / spike /
end-to-end MC ping / history), forwards your game traffic to the best one, and keeps
monitoring + auto-switching. Optional multi-path redundant tunnel and a live bilingual
terminal status screen.

> Optional host mode (the tool runs `frpc` itself) requires frp >= 0.52.0 (TOML config).
> Players normally do not need `frpc`. Public frp nodes are shared, best-effort paths —
> see [Test results](#test-results) and [Limitations](#cons--limits).

---

## Highlights

- **Two-phase probing**: fast TCP screening for every node, then an end-to-end Minecraft
  SLP ping for the top candidates (the ping reflects the real game latency, including the
  node -> server segment).
- **Spike-aware scoring**: latency, P95, jitter, loss, spike %, MC RTT, history.
- **Anti-flap switching**: failure channel A / better channel B with four gates (min dwell,
  cooldown, consecutive wins, hourly cap).
- **Local forwarder**: your game connects to `127.0.0.1`; node switches are generally
  transparent to new connections.
- **Multi-server profiles**: one tool, many servers, one at a time; each server has its own
  local port, config, logs and history.
- **Optional multipath**: duplicate critical packets across 2+ nodes, with a self-calibrating
  policy (typically only when the paths are reasonably independent and healthy; in our tests
  it did not perform worse than single-path).
- **Measured, not guessed**: per-path 500 ms RTT sampling, player-visible spike counting, and
  per-spike attribution (network vs server).
- **Cross-platform**: Windows / Linux / macOS. The agent can run in Docker.

---

## Quick start

### Players (minimal setup)

1. Unzip and double-click `start.bat` (or `mcnode.exe` / `点我.exe`).
2. First run asks the language (`English / 中文`), then lets you **add a server**:
   - **name** (anything, e.g. `MyServer`),
   - **address**: `host:port` (direct) or a node-subscription URL/file from the server owner,
   - **local port**: 25565 by default; use 25565 / 25566 / ... for different servers.
3. Next runs show a **server menu**: pick a number to start, `[a]` add, `[d]` delete, `[q]` quit.
   With a single server, press Enter to start.
4. In the game connect to `127.0.0.1:<local port>`.

Each server keeps its own `servers/<name>/{config.jsonc,data,logs}`.

### Server owners (optional)

Publish a node list (JSON array or `{"nodes":[...]}`, comments allowed) at any URL or file.
Players paste it when adding a server. You can also generate a ready config:

```bash
mcnode --make-player-config https://your/subscription/url
```

### Multipath (optional, advanced)

Multipath needs a small agent on the machine that can reach the MC server. Full steps are in
[Agent installation (server)](#agent-installation-server). In short:

```bash
mcnode --agent --config /etc/mcnode/config.jsonc
# or Docker (the image already runs --agent)
docker run -d --restart unless-stopped --network host \
  -v /opt/mcnode/config.jsonc:/etc/mcnode/config.jsonc:ro <image>
```

Then set the same `multipath.agent_token` on the client and the agent, enable
`multipath.enabled`, and expose the agent port on each frps node with frpc.

**For public deployments, set `multipath.agent_token`** — it authenticates multipath sessions.
Note the agent also relays raw MC traffic by default (`agent_passthrough`), so treat its port
like the MC port: set `agent_passthrough: false` and/or firewall it if MC should not be
reachable there.

---

## Agent installation (server)

The agent is only used by multipath. It usually runs on the same machine as the MC server:
it accepts multipath connections on `multipath.agent_listen` and forwards them to
`multipath.agent_mc`. Each frps node exposes it through an frpc TCP proxy, and the client's
node list points `server_addr:server_port` at that public agent endpoint.

Use the same release for the client and the agent; mixed versions are not supported.

### 1. Copy the binary

```bash
# on the server (amd64 / x86_64)
scp mcnode_linux_amd64 user@server:/tmp/mcnode
# ARM (Orange Pi, Raspberry Pi, etc.)
scp mcnode_linux_arm64 user@server:/tmp/mcnode

ssh user@server
sudo mkdir -p /opt/mcnode
sudo mv /tmp/mcnode /opt/mcnode/mcnode
sudo chmod +x /opt/mcnode/mcnode
/opt/mcnode/mcnode --version
```

### 2. Write the agent config

`/opt/mcnode/config.jsonc` (a minimal agent config):

```jsonc
{
  "general": { "log_level": "INFO", "log_dir": "logs", "data_dir": "data" },
  "minecraft": { "local_host": "127.0.0.1", "local_port": 25565 },
  "multipath": {
    "enabled": true,
    "agent_listen": "127.0.0.1:7001",   // 0.0.0.0:7001 only if frpc runs on another machine
    "agent_mc": "127.0.0.1:25565",      // where the MC server actually listens
    "agent_token": "change-me-to-a-long-random-string",
    "agent_passthrough": true           // false = multipath sessions only (token required)
  }
}
```

- `agent_listen` — where the agent accepts connections. Keep it on `127.0.0.1` when frpc runs
  on the same machine; use `0.0.0.0:7001` only if frpc is elsewhere, and firewall it.
- `agent_mc` — the MC server address the agent forwards into.
- `agent_token` — shared secret for **multipath sessions**; the client must use the same value.
  Empty means no session authentication (trusted networks only).
- `agent_passthrough` (default `true`) — also relay non-multipath TCP, which is what lets an
  MC status ping to the agent port work. With `false`, the agent accepts only token-authenticated
  multipath sessions; then point each node's `mc_check` at the public MC port instead.
  If you disable it while `server_port` is the agent port and forget `mc_check`, Phase 2 will
  fail and the node will be vetoed.

### 3. Run it as a systemd service

`/etc/systemd/system/mcnode-agent.service`:

```ini
[Unit]
Description=MCNodeSelector multipath agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
# create the user once:
#   sudo useradd -r -s /usr/sbin/nologin mcnode
#   sudo chown -R mcnode:mcnode /opt/mcnode
User=mcnode
WorkingDirectory=/opt/mcnode
ExecStart=/opt/mcnode/mcnode --agent
Restart=always
RestartSec=3

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now mcnode-agent
systemctl status mcnode-agent --no-pager
ss -lntp | grep 7001
```

`--agent` reads `config.jsonc` from the executable's directory (i.e. `/opt/mcnode/`); add
`--config /etc/mcnode/config.jsonc` if you keep the config elsewhere.

### 4. Expose the agent on each frps node

In each node's `frpc.ini` (or `frpc.toml`), add a TCP proxy:

```ini
[agent]
type = tcp
local_ip = 127.0.0.1
local_port = 7001
remote_port = 38410        # public agent port on this frps; pick a free one
```

Restart that frpc, then check from another machine:

```bash
nc -vz <frps-host> 38410
```

### 5. Point the node list at the agent endpoint

For nodes that should carry multipath traffic, set `server_port` to the **public agent port**
you mapped in step 4 (not the frps control port). Nodes without an agent use the public MC
port instead:

```json
{ "name": "node-a", "server_addr": "node-a.example.com", "server_port": 38410, "remote_port": 25565 }
```

- Phase 1 probes `server_addr:server_port`, and multipath auto mode connects there.
- The local forwarder also connects to `server_addr:server_port`, so single-line traffic uses
  the same endpoint (the agent relays it to MC).
- `remote_port` is the expected public MC port (frpc host mode / consistency check), not the
  probing target.

### 6. Client side

```jsonc
"multipath": {
  "enabled": true,
  "agent_token": "same-value-as-the-agent",
  "paths": []          // empty = auto from the node list
}
```

If auto-selection picks the wrong endpoint, set `paths` explicitly, e.g.
`["node-a.example.com:38410", "node-b.example.com:47680"]`.

### 7. Verify

- Server: `systemctl is-active mcnode-agent` returns `active`, and `ss -lntp | grep 7001`
  shows the listener.
- Client: `nc -vz <frps-host> <agent public port>` succeeds.
- Run `mcnode --auto --watch`; the status screen shows the per-path RTT and multipath state.
- Logs: `/opt/mcnode/logs/` (server) and `logs/` (client).

### Docker alternative

```bash
docker run -d --restart unless-stopped --network host \
  -v /opt/mcnode/config.jsonc:/etc/mcnode/config.jsonc:ro \
  <image>
```

The image already runs `--agent --config /etc/mcnode/config.jsonc`. With `--network host` the
agent can reach `127.0.0.1:25565`; with a bridge network, set `agent_mc` to the MC
container/host address instead.

---

## How it works

### Probing & scoring
- **Phase 1** (all enabled nodes, concurrent): N TCP connects to `probe` (default
  `server_addr:server_port`), with interval sampling; robust statistics reduce spikes
  (MAD + symmetric trim) before computing Avg / P95 / jitter / loss / spike %.
- **Phase 2** (top-K by Phase 1): end-to-end Minecraft SLP ping (`mc_check`, default
  `server_addr:remote_port`), N samples + a short burst to catch "fine when idle, bad under
  load".
- **Score** = weighted, re-normalized sum of latency / P95 / jitter / loss / spike / MC /
  history, multiplied by the node weight. A node whose MC handshake does not succeed is
  vetoed (unless every node fails, which often means the server is down).

### Switching
- **Channel A (failure)**: current node fails `fail_threshold` times, or the frpc restart
  limit is hit -> immediate full retest and switch (no cooldown/dwell).
- **Channel B (better)**: needs all four gates: min dwell, cooldown, `advantage_margin` +
  `consecutive_wins`, hourly cap.
- **Degradation switch**: current node stays above `degradation_threshold_ms` for N ticks.

### Multipath (optional)
- Every path carries a 500 ms ping/pong; per-path RTT, jitter, loss and send-queue are tracked.
- **Tiered send**: tiny frames (<= 256 B) on all eligible paths; medium/large on the best 1-2.
- **Self-calibrating**: duplicate mainly when paths are reasonably independent (low RTT
  correlation), there is loss/jitter, and we are not the bottleneck; otherwise send a single
  path.
- **Fail-safe**: when the signals are inconclusive, it falls back to a single path (in our
  tests this did not perform worse than single-path).
- **Fast retransmit** (ACK-stall detection), exponential backoff, pacing, backpressure,
  dead-path reconnect, and no forced session shutdown on retransmit limits.

### Measurement
- Monitor probes the current node every `monitor_interval_sec` (MC ping by default).
- Multipath pings each path every 500 ms; spikes are counted on the **player-visible**
  minimum across paths, on the rising edge (one stall = one spike).
- Each spike is attributed: `path RTT normal -> server / agent->MC` vs `path RTT high ->
  network`.

### Traffic / bandwidth overhead
Multipath is the main source of additional traffic; the rest is relatively small.

- **What gets duplicated**: by design, tiny frames (<= 256 B) are sent on all eligible paths;
  medium/large frames use 1-2 paths (`large_paths`, default 1). Movement/control packets are
  small, so the duplicated volume is usually a modest share of the session; chunk traffic is
  single-path by default.
- **Rough expectation**: the extra upload is roughly on the order of the duplicated
  small-packet rate — often tens of KB/s, varying with activity and modpack. It is not a fixed
  figure and can be higher in busy scenes.
- **Probing traffic**: periodic TCP connects / MC pings plus full retests. A full retest
  (default every 30 min; skipped while players are online when `pause_switch_when_active`)
  performs `samples` TCP connects and `mc_check.samples` (+ `burst`) MC pings per node.
- **Ways to cap it**:
  - `failover_only: true` — no duplication, failover only (smallest overhead);
  - `critical_always: false` (default) with `auto_redundancy: true` — duplicate mainly when
    it appears useful;
  - `large_paths: 1` (default) — large frames on one path;
  - lower `redundancy` / `path_count` / `samples` / `mc_check.samples` / `burst`, or raise
    `retest_interval_min`.
- On metered or low-bandwidth links, `failover_only` (or disabling multipath) is usually the
  safer choice.

---

## Configuration

Copy `config.example.jsonc` to `config.jsonc`, or run `mcnode --setup`. Key sections:

```jsonc
{
  "general": {
    "language": "zh",              // zh / en
    "auto_update_check": true,
    "update_repo": ""              // e.g. "you/MCNodeSelector"
  },
  "minecraft": {
    "local_host": "127.0.0.1",
    "local_port": 25565,           // port your game connects to
    "extra_local_ports": []        // extra listen ports
  },
  "remote": {
    // http(s) URL, file://, or a local path. Accepts a JSON array or {"nodes":[...]}.
    "url": ""
  },
  "multipath": {
    "enabled": false,
    "agent_token": ""              // SAME value on client and agent
  },
  "nodes": [
    // {
    //   "name": "example",
    //   "server_addr": "frp.example.com",
    //   "server_port": 7000,        // frps port
    //   "token": "",
    //   "remote_port": 25565,       // public MC port on the node
    //   "region": "cn-example",
    //   "weight": 1.0,
    //   "enabled": true
    // }
  ]
}
```

### Node fields
| Field | Meaning |
|---|---|
| `name` | unique name (shown in the UI) |
| `server_addr` / `server_port` | public endpoint for game traffic: the MC public port, or the agent port when multipath is used (with `frpc.enabled=true`, the frps bindPort) |
| `token` / `user` | frps auth (blank if none) |
| `remote_port` | expected public MC port (used by frpc host mode and the port-consistency check; usually 25565) |
| `region` / `tags` | region/tags (multipath uses `region` for diversity) |
| `weight` | `<1` de-prioritize, `>1` prefer |
| `enabled` | include in probing |

### Multipath fields (most useful)
| Field | Default | Meaning |
|---|---|---|
| `enabled` | false | enable the redundant tunnel |
| `auto` | true | auto-pick paths by score |
| `adaptive` | true | single path normally, engage on degradation |
| `redundancy` / `redundancy_max` | 2 / 3 | min / max paths |
| `critical_always` | false | keep tiny packets duplicated even in single-line |
| `large_paths` | 1 | paths for large frames (1 saves upstream) |
| `failover_only` | false | do not duplicate; only fail over |
| `auto_redundancy` | true | self-calibrating: duplicate mainly when useful |
| `queue_rise_ms` / `weak_jitter_ms` | 30 / 25 | congestion / jitter thresholds |
| `engage_spike_ms` | 200 | a single spike above this engages multipath |
| `agent_listen` | 127.0.0.1:7001 | agent listen address (server side; use 0.0.0.0 if frpc runs elsewhere) |
| `agent_mc` | 127.0.0.1:25565 | MC target the agent relays to (server side) |
| `agent_token` | "" | shared secret; set it for public deployments |
| `agent_passthrough` | true | also relay non-multipath TCP (needed for MC status pings to the agent port); false = token-only multipath |

### Multi-server profiles
- Profiles live in `servers/<name>/` (config + data + logs), independent of each other.
- Launcher menu on double-click; CLI: `--servers`, `--add-server`, `--play`, `--remove-server`.
- Language is remembered in `settings.json`.

---

## CLI

```
mcnode                       launcher (server menu / add)
mcnode --auto [--watch]      auto mode (+ live status screen)
mcnode --node <name>         fixed node (still monitored)
mcnode --setup               quick setup wizard
mcnode --servers             list server profiles
mcnode --add-server <name> <addr|subscription> <port>
mcnode --play <name>         start a server profile
mcnode --remove-server <name>
mcnode --import <file|URL>   import/validate a node list
mcnode --make-player-config <URL|file>   generate a minimal-setup config
mcnode --test                full speed test report
mcnode --list / --status / --history
mcnode --export <file.csv>   export history scores
mcnode --check-update        check for a newer release
mcnode --install-autostart / --uninstall-autostart
mcnode --diagnose            one-click diagnostics
mcnode --agent               server-side multipath agent
mcnode --config <file>       use a specific config file (default: config.jsonc next to the exe)
mcnode --version / --help
```

Status-screen keys: `[1-9]` node, `[0]` 10th, `[s]` next, `[a]` auto, `[t]` retest,
`[r]` reload, `[m]` multipath mode, `[b]` backup node, `[q]` quit.

---

## Test results

### Method
- **Simulated network** (reproducible): `cmd/netsim` + `internal/mp/sim_test.go` inject base
  latency, jitter and periodic congestion ("stalls") into the real multipath code, then
  measure **player-visible round-trip** (the bytes that actually come out). 5 minutes per
  case, ~11,600 samples, 8 cases run concurrently.
- **Real network**: 15 min 40 s continuous monitoring with an active session on a home
  broadband, plus a 6 min A/B (single vs multi).

> Caveat: these are measurements on one specific environment; absolute numbers vary with the
> network. They are indicative of the *relative* behavior.

### Network quality tiers (classification)
| Tier | p50 | p95 | Spike rate (>200 ms) | Typical |
|---|---|---|---|---|
| **Good** | <= 80 ms | <= 120 ms | <= 1% | smooth |
| **Fair** | 80–200 ms | <= 300 ms | 1–10% | occasional jitter |
| **Poor** | 200–450 ms | 300–1000 ms | > 10% | noticeable stutter |
| **Severe** | > 450 ms | > 1000 ms | frequent | second-level stalls |

### Four-tier test: single vs multipath (5 min each)
| Tier | Single p50/p95/max | Single spikes | Multi p50/p95/max | Multi spikes | Effect |
|---|---|---|---|---|---|
| Good | 67 / 71 / 165 ms | 0 | 65 / 70 / 167 ms | 0 | equal (no added latency) |
| Fair | 172 / 251 / 423 ms | 766 (6.6%) | 163 / 180 / 408 ms | **29 (0.25%)** | **spikes -96%, p95 -28%** |
| Poor | 331 / 823 / 905 ms | ≈100%* | 313 / **345** / 880 ms | ≈100%* | **p95 -58%** |
| Severe | 584 / 1535 / 1636 ms | ≈100%* | 537 / **777** / 1418 ms | ≈100%* | **p95 -49%**, max -13% |

\* In Poor/Severe the baseline itself is above 200 ms, so an absolute >200 ms counter is close
to 100%; look at p95/max instead.

### Controlled scenarios (simulator, ~85 s)
| Scenario | Multi spikes | Single spikes |
|---|---|---|
| Normal, 2 independent clean paths | 0 | 0 |
| One node congested (A good, B bad) | 0 | 0 |
| Peak-hour **shared** bottleneck (both paths stall together) | 129 | 177 |
| Bad network, **independent** paths | 168 | 202 |

### Real 15-minute monitoring (home broadband, adaptive multipath)
| Metric | Result |
|---|---|
| Player-visible tunnel spikes | **5 / 15m40s** (213–273 ms) ~= 19/hour |
| End-to-end spikes (10 s monitor) | 9 (376–3999 ms) |
| — attributed to **server / agent->MC** | **7 / 9** (path RTT was 48–80 ms at that moment) |
| — attributed to **network** | 2 / 9 |
| Retransmit storm / send backpressure / switches | 0 / 0 / 0 |
| Forced reconnect | 4 |

### Real A/B (6 min each, same network)
| Metric | A single (failover) | B multi (duplicate) |
|---|---|---|
| Tunnel spikes (>200 ms) | 3 (281/282/297 ms) | **1 (250 ms)** |
| End-to-end spikes | 0 | 0 |
| Path RTT | 49–57 ms | 49–57 ms |
| Loss / retrans / backpressure | 0 / 0 / 0 | 0 / 0 / 0 |

### What the results mean
- In our tests, multipath did not perform worse than single-path (latency/loss similar,
  spikes at or below single-path).
- It tends to help most when the paths are **reasonably independent**; when both paths share
  the same bottleneck (same ISP/uplink) it may only reduce spikes by roughly 20–30%, and it is
  not expected to eliminate them.
- Most very large "end-to-end" spikes in the real test were **server-side** (MC GC/tick),
  correctly attributed by the spike log; these are largely outside the client's control.

---

## Pros and cons

### Pros
- Honest measurement: end-to-end MC ping, player-visible spikes, per-spike attribution.
- Strong anti-flap switching and a safe local forwarder (no game reconfiguration).
- Multipath that avoids self-harm: no blind duplication on a shared bottleneck.
- Robust to node churn: DNS cache with TTL + re-resolve on failure.
- Near-zero setup for players (server menu), multi-server, bilingual, tray, wizard, autostart.

### Cons / limits
- **Shared bottlenecks are largely outside client-side control**: if both paths use the same
  home uplink / ISP, multipath may only reduce spikes partially.
- **TCP-over-TCP (public frp)** can still produce large spikes under congestion.
- Multipath requires a server-side `--agent` and a token; it tends to help mainly with
  independent paths.
- Adaptation has latency (hundreds of ms to seconds); it may not anticipate every spike.
- Server-side MC lag (GC/tick) is out of scope.
- Logs and some CLI report tables are Chinese (UI, help, notifications, tray are bilingual).

---

## Security

- The agent validates a handshake token (`multipath.agent_token`) for **multipath sessions**;
  leave it empty only on trusted networks.
- With `agent_passthrough: true` (default) the agent also relays non-multipath TCP so MC status
  pings work. That makes the agent port an alternate route to MC — protect it like the MC port
  (firewall / whitelist), or set `agent_passthrough: false`.
- frp tokens/credentials are redacted from logs.
- In normal client use the tool makes outbound connections and does not open a public port;
  the optional `--agent` listens on the port you configure for it.

---

## Build & release

```bash
go build -o mcnode .            # current OS
go build -o mcnode.exe .        # Windows (icon embedded via rsrc_windows_amd64.syso)
```

- The Windows icon is embedded from `rsrc_windows_amd64.syso` (generated from `tb.ico`).
- GitHub Actions (`.github/workflows/release.yml`) builds Windows/Linux/macOS on `v*` tags.
- Docker: `Dockerfile` builds a small Linux image; mount the config at
  `/etc/mcnode/config.jsonc`.

---

## Development

```bash
go vet ./...
go test ./...                                  # unit tests
go test ./internal/mp -run TestScenarios -v    # 4 network scenarios, multi vs single
go build ./cmd/netsim                          # tiered network-impairment tool
./netsim -base 70 -jitter 25 -stallEvery 10 -stallDur 500 -stallExtra 80 -paths 1 -secs 300 -label fair-p1
```

---

## FAQ / troubleshooting

- **Can't fetch the node list**: use `http(s)://...`, `file://...`, a local path, or a bare
  domain (auto-prefixed with `http://`). Both URL and local file are supported.
- **Port already in use**: change `minecraft.local_port` (or the profile's port).
- **Agent rejects connections**: set the same `multipath.agent_token` on both ends.
- **Node keeps getting blacklisted**: check DNS and the node's frps port/firewall.
- **Large spikes but path RTT is fine**: the server (MC/GC) is lagging; tune the server JVM.

---

## License

GNU Affero General Public License v3.0 (AGPL-3.0). See `LICENSE`.

Copyright (C) 2026 MCNS contributors.

If you modify this program and let others use it over a network, the AGPL requires you to
offer them the corresponding source code.