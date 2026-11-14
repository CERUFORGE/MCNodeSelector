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

MIT. See `LICENSE`.

---
---

# MCNodeSelector (MCNS)（中文）

**版本 1.0** · 简称 **MCNS** · Minecraft FRP 节点选择器 + 本地转发器。

自动测速所有已配置节点，按「延迟 / P95 / 抖动 / 丢包 / 尖峰 / 端到端 MC ping / 历史」综合评分，
把游戏流量转发到最优节点，并持续监控与自动切换。可选多路冗余隧道，以及实时双语状态屏。

> 适配 frp >= 0.52.0。公共 frp 节点为共享链路、尽力而为——见[实测结果](#实测结果)与[限制](#缺点--限制)。

---

## 亮点

- **两阶段测速**：先对所有节点做快速 TCP 筛查，再对前几名做端到端 Minecraft SLP ping
  （该 ping 反映真实游戏延迟，含「节点 → 服务器」段）。
- **尖峰感知评分**：延迟、P95、抖动、丢包、尖峰率、MC RTT、历史。
- **防横跳切换**：故障通道 A / 更优通道 B，四道闸门（最小驻留、冷却、连续优势、每小时上限）。
- **本地转发**：游戏始终连 `127.0.0.1`，切节点通常只影响新连接。
- **多服务器档案**：一个工具管理多个服务器、一次运行一个；每个服务器有独立本地端口、配置、日志与历史。
- **可选多线**：在 2 个及以上节点间复制关键小包，采用自校准策略（一般在路径相对独立且健康时才复制；
  在实测中未出现比单线更差的情况）。
- **可测量而非猜测**：每路径 500ms RTT 采样、玩家可见尖峰计数、逐次尖峰归因（网络 vs 服务端）。
- **跨平台**：Windows / Linux / macOS；agent 可用 Docker 运行。

---

## 快速开始

### 玩家（最小配置）

1. 解压，双击 `start.bat`（或 `mcnode.exe` / `点我.exe`）。
2. 首次运行先选语言（`English / 中文`），然后添加服务器：
   - **名称**：随便起（如 `MyServer`）；
   - **地址**：`host:port`（直连）或腐竹提供的节点订阅 URL/文件；
   - **本地端口**：默认 25565；多个服务器用 25565 / 25566 / …。
3. 以后运行会显示**服务器菜单**：输入数字启动，`[a]` 添加，`[d]` 删除，`[q]` 退出；
   只有一台服务器时，回车即可启动。
4. 游戏里连接 `127.0.0.1:<本地端口>`。

每个服务器独立档案：`servers/<名称>/{config.jsonc,data,logs}`。

### 腐竹（可选）

把节点列表（JSON 纯数组或 `{"nodes":[...]}`，允许注释）发布到任意 URL 或文件，玩家添加服务器时填写；
也可以生成现成配置：

```bash
mcnode --make-player-config https://你的订阅地址
```

### 多线（可选，进阶）

多线需要在能访问 MC 服务器的机器上运行一个小 agent，完整步骤见
[服务端 agent 安装](#服务端-agent-安装)。简版：

```bash
mcnode --agent --config /etc/mcnode/config.jsonc
# 或 Docker（镜像已内置 --agent）
docker run -d --restart unless-stopped --network host \
  -v /opt/mcnode/config.jsonc:/etc/mcnode/config.jsonc:ro <镜像>
```

然后客户端与 agent 设置相同的 `multipath.agent_token`，启用 `multipath.enabled`，并用 frpc
把 agent 端口映射到各节点。

**公网部署请设置 `multipath.agent_token`**，它用于校验多线会话。注意 agent 默认还会转发
非多线的原始 TCP（`agent_passthrough`），所以请把 agent 端口当作 MC 端口来保护：
如果不想让 MC 从这个端口可达，设 `agent_passthrough: false` 并/或用防火墙限制。

---

## 服务端 agent 安装

agent 只在多线时使用，通常与 MC 服务器跑在同一台机器：它在 `multipath.agent_listen` 上接受
多线连接，再转发到 `multipath.agent_mc`。每个 frps 节点通过 frpc 的 TCP 代理把它暴露出去，
客户端节点列表里的 `server_addr:server_port` 就指向这个公网 agent 端点。

客户端与 agent 请使用同一版本；不建议混用。

### 1. 上传二进制

```bash
# 服务器为 amd64 / x86_64
scp mcnode_linux_amd64 user@server:/tmp/mcnode
# ARM（香橙派、树莓派等）
scp mcnode_linux_arm64 user@server:/tmp/mcnode

ssh user@server
sudo mkdir -p /opt/mcnode
sudo mv /tmp/mcnode /opt/mcnode/mcnode
sudo chmod +x /opt/mcnode/mcnode
/opt/mcnode/mcnode --version
```

### 2. 写 agent 配置

`/opt/mcnode/config.jsonc`（最小 agent 配置）：

```jsonc
{
  "general": { "log_level": "INFO", "log_dir": "logs", "data_dir": "data" },
  "minecraft": { "local_host": "127.0.0.1", "local_port": 25565 },
  "multipath": {
    "enabled": true,
    "agent_listen": "127.0.0.1:7001",   // 仅当 frpc 在另一台机器时才用 0.0.0.0:7001
    "agent_mc": "127.0.0.1:25565",      // MC 服务器实际监听的地址
    "agent_token": "换成一串足够长的随机字符",
    "agent_passthrough": true           // false = 只接受多线会话（需 token）
  }
}
```

- `agent_listen`：agent 接受连接的地址。frpc 与 agent 同机时保持 `127.0.0.1`；
  只有 frpc 在别的机器时才用 `0.0.0.0:7001`，并用防火墙限制来源。
- `agent_mc`：agent 转发进入的 MC 服务器地址。
- `agent_token`：**多线会话**的共享密钥，客户端必须填相同值。留空表示不校验会话
  （仅限可信网络）。
- `agent_passthrough`（默认 `true`）：同时转发非多线的原始 TCP，这样「MC 状态 ping 直连
  agent 端口」也能通。设为 `false` 后 agent 只接受带 token 的多线会话，此时请把各节点的
  `mc_check` 指向 MC 公网端口；如果 `server_port` 填的是 agent 端口又忘了配 `mc_check`，
  Phase 2 会失败并导致该节点被否决。

### 3. 用 systemd 常驻

`/etc/systemd/system/mcnode-agent.service`：

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

`--agent` 默认读取可执行文件所在目录下的 `config.jsonc`（即 `/opt/mcnode/`）；如果配置放在
别处，加上 `--config /etc/mcnode/config.jsonc`。

### 4. 在每个 frps 节点上暴露 agent

在该节点的 `frpc.ini`（或 `frpc.toml`）里加一个 TCP 代理：

```ini
[agent]
type = tcp
local_ip = 127.0.0.1
local_port = 7001
remote_port = 38410        # 该 frps 上的公网 agent 端口，挑一个空闲的
```

重启该 frpc，然后在另一台机器上验证：

```bash
nc -vz <frps 地址> 38410
```

### 5. 节点列表指向 agent 端点

需要承载多线流量的节点，`server_port` 填第 4 步映射出的**公网 agent 端口**（不是 frps 控制端口）；
没有 agent 的节点则填 MC 公网端口：

```json
{ "name": "node-a", "server_addr": "node-a.example.com", "server_port": 38410, "remote_port": 25565 }
```

- Phase 1 探测 `server_addr:server_port`，多线自动模式也连接这里。
- 本地转发同样连 `server_addr:server_port`，所以单线流量走同一入口（由 agent 转给 MC）。
- `remote_port` 是期望的 MC 公网端口（frpc 托管模式 / 端口一致性检查用），不是探测目标。

### 6. 客户端

```jsonc
"multipath": {
  "enabled": true,
  "agent_token": "与 agent 相同",
  "paths": []          // 留空 = 从节点列表自动生成
}
```

如果自动选择挑错了端点，可显式写 `paths`，例如
`["node-a.example.com:38410", "node-b.example.com:47680"]`。

### 7. 验证

- 服务端：`systemctl is-active mcnode-agent` 返回 `active`，且 `ss -lntp | grep 7001`
  能看到监听。
- 客户端：`nc -vz <frps 地址> <agent 公网端口>` 能连通。
- 运行 `mcnode --auto --watch`，状态屏会显示各路径 RTT 与多线状态。
- 日志：服务端 `/opt/mcnode/logs/`，客户端 `logs/`。

### Docker 方式

```bash
docker run -d --restart unless-stopped --network host \
  -v /opt/mcnode/config.jsonc:/etc/mcnode/config.jsonc:ro \
  <镜像>
```

镜像已内置 `--agent --config /etc/mcnode/config.jsonc`。`--network host` 下 agent 能访问
`127.0.0.1:25565`；若用桥接网络，请把 `agent_mc` 改成 MC 容器/宿主机的地址。

---

## 工作原理

### 测速与评分
- **Phase 1**（全部启用节点，并发）：对 `probe`（默认 `server_addr:server_port`）做 N 次 TCP 连接，
  按间隔采样；用稳健统计（MAD + 对称裁剪）降低尖峰影响后再计算 Avg / P95 / 抖动 / 丢包 / 尖峰率。
- **Phase 2**（Phase 1 前 K 名）：端到端 Minecraft SLP ping（`mc_check`，默认
  `server_addr:remote_port`），N 次采样 + 一小段突发，用来发现「空闲正常、负载变差」的节点。
- **评分** = 延迟 / P95 / 抖动 / 丢包 / 尖峰 / MC / 历史 的加权、重归一化求和，再乘节点权重。
  MC 握手未成功的节点会被否决（若所有节点都失败，通常意味着服务器未开）。

### 切换
- **通道 A（故障）**：当前节点连续失败 `fail_threshold` 次，或 frpc 重启达上限 → 立即全量重测并切换
  （不受冷却/驻留限制）。
- **通道 B（更优）**：需通过四道闸门：最小驻留、冷却、`advantage_margin` + `consecutive_wins`、每小时上限。
- **劣化切换**：当前节点持续高于 `degradation_threshold_ms` 达 N 次。

### 多线（可选）
- 每条路径每 500ms 一次 ping/pong；跟踪每路径 RTT、抖动、丢包与发送队列。
- **分级发送**：关键小包（≤256B）发往所有合格路径；普通/大包发往最稳的 1–2 条。
- **自校准**：主要在路径相对独立（RTT 相关性低）、存在丢包/抖动、且不是我们自身瓶颈时才复制；
  否则走单路。
- **安全回退**：信号不明确时退回单路（在实测中未出现比单线更差的情况）。
- **快速重传**（ACK 停滞判定）、指数退避、发送节流、背压、死链重连；重传达上限时不会强制断开会话。

### 测量
- 监控每 `monitor_interval_sec` 探测当前节点一次（默认用 MC ping）。
- 多线每 500ms 探测每条路径；尖峰按**玩家可见**的「各路径最小值」、在上升沿计数（一次卡顿记一次）。
- 每次尖峰给出归因：`路径 RTT 正常 → 服务端 / agent→MC` vs `路径 RTT 偏高 → 网络`。

### 流量 / 带宽开销
多线是额外流量的主要来源，其余相对较小。

- **复制范围**：按设计，关键小包（≤256B）发往所有合格路径；普通/大包使用 1–2 条
  （`large_paths`，默认 1）。移动/控制包较小，因此复制的流量通常是会话的一小部分；
  区块流量默认单路。
- **大致量级**：额外上行大致相当于「被复制小包的速率」，常见在**数十 KB/s** 量级，
  随活跃度和模组变化，并非固定值，繁忙场景可能更高。
- **探测流量**：周期性 TCP 连接 / MC ping，以及全量重测。全量重测（默认每 30 分钟；开启
  `pause_switch_when_active` 时玩家在线会跳过）会对每个节点执行 `samples` 次 TCP 连接与
  `mc_check.samples`（+ `burst`）次 MC ping。
- **如何压低**：
  - `failover_only: true` —— 不复制，只做故障切换（开销最小）；
  - `critical_always: false`（默认）配合 `auto_redundancy: true` —— 主要在看起来有用时复制；
  - `large_paths: 1`（默认）—— 大包走单路；
  - 降低 `redundancy` / `path_count` / `samples` / `mc_check.samples` / `burst`，或增大
    `retest_interval_min`。
- 流量敏感或低带宽链路，通常用 `failover_only`（或关闭多线）更稳妥。

---

## 配置

把 `config.example.jsonc` 复制为 `config.jsonc`，或运行 `mcnode --setup`。关键部分：

```jsonc
{
  "general": {
    "language": "zh",              // zh / en
    "auto_update_check": true,
    "update_repo": ""              // 如 "you/MCNodeSelector"
  },
  "minecraft": {
    "local_host": "127.0.0.1",
    "local_port": 25565,           // 游戏连接的端口
    "extra_local_ports": []        // 额外监听端口
  },
  "remote": {
    // http(s) URL、file:// 或本地路径；内容为 JSON 数组或 {"nodes":[...]}
    "url": ""
  },
  "multipath": {
    "enabled": false,
    "agent_token": ""              // 客户端与 agent 填相同值
  },
  "nodes": [
    // {
    //   "name": "example",
    //   "server_addr": "frp.example.com",
    //   "server_port": 7000,        // frps 端口
    //   "token": "",
    //   "remote_port": 25565,       // 节点上的 MC 公网端口
    //   "region": "cn-example",
    //   "weight": 1.0,
    //   "enabled": true
    // }
  ]
}
```

### 节点字段
| 字段 | 含义 |
|---|---|
| `name` | 唯一名称（界面显示） |
| `server_addr` / `server_port` | 游戏流量的公网入口：MC 公网端口；启用多线时填 agent 公网端口（`frpc.enabled=true` 时填 frps 的 bindPort） |
| `token` / `user` | frps 认证（没有则留空） |
| `remote_port` | 期望的 MC 公网端口（frpc 托管模式与端口一致性检查用；通常 25565） |
| `region` / `tags` | 地区/标签（多线用 `region` 做去相关） |
| `weight` | `<1` 降权，`>1` 偏好 |
| `enabled` | 是否参与测速 |

### 多线字段（常用）
| 字段 | 默认 | 含义 |
|---|---|---|
| `enabled` | false | 是否启用冗余隧道 |
| `auto` | true | 按评分自动挑选路径 |
| `adaptive` | true | 平时单路，劣化时才启用多线 |
| `redundancy` / `redundancy_max` | 2 / 3 | 最少 / 最多路径数 |
| `critical_always` | false | 单线时也保留关键小包复制 |
| `large_paths` | 1 | 大包使用的路径数（1 省上行） |
| `failover_only` | false | 不复制，只做故障切换 |
| `auto_redundancy` | true | 自校准：主要在有用时复制 |
| `queue_rise_ms` / `weak_jitter_ms` | 30 / 25 | 拥塞 / 抖动阈值 |
| `engage_spike_ms` | 200 | 单次尖峰超过该值即启用多线 |
| `agent_listen` | 127.0.0.1:7001 | agent 监听地址（服务端；frpc 在别的机器时用 0.0.0.0） |
| `agent_mc` | 127.0.0.1:25565 | agent 转发到的 MC 地址（服务端） |
| `agent_token` | "" | 共享密钥；公网部署请设置 |
| `agent_passthrough` | true | 同时转发非多线 TCP（MC 状态 ping 直连 agent 端口需要）；false = 只接受带 token 的多线会话 |

### 多服务器档案
- 档案位于 `servers/<名称>/`（配置 + 数据 + 日志），彼此独立。
- 双击进入启动菜单；命令行：`--servers`、`--add-server`、`--play`、`--remove-server`。
- 语言记录在 `settings.json`。

---

## 命令行

```
mcnode                       启动器（服务器菜单 / 添加）
mcnode --auto [--watch]      自动模式（+ 实时状态屏）
mcnode --node <名称>         固定节点（仍受监控）
mcnode --setup               快速配置向导
mcnode --servers             列出服务器档案
mcnode --add-server <名称> <地址|订阅> <端口>
mcnode --play <名称>         启动指定服务器
mcnode --remove-server <名称>
mcnode --import <文件|URL>   导入/校验节点列表
mcnode --make-player-config <URL|文件>   生成最小配置的玩家配置
mcnode --test                全量测速报告
mcnode --list / --status / --history
mcnode --export <文件.csv>   导出历史评分
mcnode --check-update        检查更新
mcnode --install-autostart / --uninstall-autostart
mcnode --diagnose            一键诊断
mcnode --agent               服务端多线 agent
mcnode --config <文件>       指定配置文件（默认 exe 同目录的 config.jsonc）
mcnode --version / --help
```

状态屏按键：`[1-9]` 选节点、`[0]` 第 10 个、`[s]` 下一节点、`[a]` 自动、`[t]` 重测、
`[r]` 重载、`[m]` 多线模式、`[b]` 备用节点、`[q]` 退出。

---

## 实测结果

### 方法
- **模拟网络**（可复现）：`cmd/netsim` + `internal/mp/sim_test.go` 对真实多线代码注入基础延迟、
  抖动与周期性拥塞（"卡顿窗口"），测量**玩家可见往返**（真正发出来的字节）。每个用例 5 分钟、
  约 11,600 个样本，8 个用例并发执行。
- **真实网络**：家宽下持续监控 15 分 40 秒（期间维持活跃会话），另做 6 分钟 A/B（单线 vs 多线）。

> 说明：以上为某一具体环境下的测量，绝对数值随网络变化；主要用于观察**相对**表现。

### 网络质量分档（分类标准）
| 档位 | p50 | p95 | 尖峰率(>200ms) | 典型表现 |
|---|---|---|---|---|
| **良好** | ≤ 80ms | ≤ 120ms | ≤ 1% | 平滑 |
| **一般** | 80–200ms | ≤ 300ms | 1–10% | 偶有抖动 |
| **较差** | 200–450ms | 300–1000ms | > 10% | 明显卡顿 |
| **恶劣** | > 450ms | > 1000ms | 频繁 | 秒级卡顿 |

### 四档实测：单线 vs 多线（各 5 分钟）
| 档位 | 单线 p50/p95/max | 单线尖峰 | 多线 p50/p95/max | 多线尖峰 | 效果 |
|---|---|---|---|---|---|
| 良好 | 67 / 71 / 165ms | 0 | 65 / 70 / 167ms | 0 | 持平（未增加延迟） |
| 一般 | 172 / 251 / 423ms | 766 (6.6%) | 163 / 180 / 408ms | **29 (0.25%)** | **尖峰 -96%，p95 -28%** |
| 较差 | 331 / 823 / 905ms | ≈100%* | 313 / **345** / 880ms | ≈100%* | **p95 -58%** |
| 恶劣 | 584 / 1535 / 1636ms | ≈100%* | 537 / **777** / 1418ms | ≈100%* | **p95 -49%**，max -13% |

\* 较差/恶劣档基线本身已高于 200ms，绝对 >200ms 计数会接近 100%，此时应看 p95/max。

### 受控情景（模拟器，约 85 秒）
| 情景 | 多线尖峰 | 单线尖峰 |
|---|---|---|
| 正常，2 条独立干净路径 | 0 | 0 |
| 单节点拥堵（A 好、B 差） | 0 | 0 |
| 晚高峰**共用**瓶颈（两路同时卡顿） | 129 | 177 |
| 差网络，**独立**路径 | 168 | 202 |

### 真实 15 分钟监控（家宽、自适应多线）
| 指标 | 结果 |
|---|---|
| 玩家可见隧道尖峰 | **5 次 / 15分40秒**（213–273ms）≈ 19 次/小时 |
| 端到端尖峰（10s 监控） | 9 次（376–3999ms） |
| — 归因**服务端 / agent→MC** | **7 / 9**（当时路径 RTT 为 48–80ms） |
| — 归因**网络** | 2 / 9 |
| 重传风暴 / 发送背压 / 切换 | 0 / 0 / 0 |
| 强制重连 | 4 |

### 真实 A/B（各 6 分钟，同一网络）
| 指标 | A 单线（热备） | B 多线（复制） |
|---|---|---|
| 隧道尖峰（>200ms） | 3 次（281/282/297ms） | **1 次（250ms）** |
| 端到端尖峰 | 0 | 0 |
| 路径 RTT | 49–57ms | 49–57ms |
| 丢包 / 重传 / 背压 | 0 / 0 / 0 | 0 / 0 / 0 |

### 结果解读
- 在实测中，多线未出现比单线更差的情况（延迟/丢包相近，尖峰不高于单线）。
- 路径**相对独立**时通常收益最大；当两条路径共用同一瓶颈（同一 ISP/上行）时，
  一般只能减少约 20–30% 的尖峰，不预期能完全消除。
- 真实测试中那些特别大的「端到端」尖峰多数是**服务端**（MC GC/tick）造成的，尖峰日志已正确归因；
  这类情况基本超出客户端能控制的范围。

---

## 优缺点

### 优点
- 测量诚实：端到端 MC ping、玩家可见尖峰、逐次尖峰归因。
- 防横跳切换较强，本地转发安全（无需改游戏配置）。
- 多线避免「帮倒忙」：共用瓶颈下不会盲目复制。
- 对节点变动较健壮：DNS 缓存带 TTL，失败时重新解析。
- 玩家侧接近零配置（服务器菜单），多服务器、双语、托盘、向导、开机自启。

### 缺点 / 限制
- **共用瓶颈基本超出客户端控制**：若两条路径共用同一家宽/ISP，多线可能只能部分减少尖峰。
- **TCP-over-TCP（公共 frp）** 在拥塞时仍可能出现较大尖峰。
- 多线需要服务端 `--agent` 与 token；主要适合独立路径。
- 自适应存在滞后（数百毫秒到数秒），可能无法预判每一次尖峰。
- 服务端 MC 卡顿（GC/tick）不在本工具范围内。
- 日志与部分 CLI 报告仍为中文（界面、帮助、通知、托盘已双语）。

---

## 安全

- agent 会校验**多线会话**的握手 token（`multipath.agent_token`）；仅在可信网络中才建议留空。
- `agent_passthrough: true`（默认）时，agent 还会转发非多线的原始 TCP（MC 状态 ping 需要）。
  这相当于 agent 端口也是通往 MC 的一条路径——请像保护 MC 端口一样保护它（防火墙/白名单），
  或设 `agent_passthrough: false`。
- frp token/凭据在日志中会脱敏。
- 正常客户端使用只发起出站连接，不开放公网端口；可选的 `--agent` 会监听你配置的端口。

---

## 构建与发布

```bash
go build -o mcnode .            # 当前系统
go build -o mcnode.exe .        # Windows（图标通过 rsrc_windows_amd64.syso 嵌入）
```

- Windows 图标由 `rsrc_windows_amd64.syso` 嵌入（由 `tb.ico` 生成）。
- GitHub Actions（`.github/workflows/release.yml`）在 `v*` 标签时构建 Windows/Linux/macOS。
- Docker：`Dockerfile` 构建精简 Linux 镜像；把配置挂载到 `/etc/mcnode/config.jsonc`。

---

## 开发

```bash
go vet ./...
go test ./...                                  # 单元测试
go test ./internal/mp -run TestScenarios -v    # 4 个网络情景，多线 vs 单线
go build ./cmd/netsim                          # 分档网络损伤工具
./netsim -base 70 -jitter 25 -stallEvery 10 -stallDur 500 -stallExtra 80 -paths 1 -secs 300 -label fair-p1
```

---

## 常见问题 / 排障

- **拉不到节点列表**：支持 `http(s)://...`、`file://...`、本地路径，或裸域名（会自动补 `http://`）。
- **端口被占用**：修改 `minecraft.local_port`（或该档案的端口）。
- **agent 拒绝连接**：两端设置相同的 `multipath.agent_token`。
- **节点反复被拉黑**：检查 DNS、节点的 frps 端口与防火墙。
- **尖峰很大但路径 RTT 正常**：多为服务端（MC/GC）卡顿，建议调服务端 JVM。

---

## 许可证

MIT。见 `LICENSE`。
