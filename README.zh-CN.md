[English](README.md) | **简体中文**

# MCNodeSelector (MCNS)

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

GNU Affero General Public License v3.0（AGPL-3.0），见 `LICENSE`。

版权所有 (C) 2026 MCNS 贡献者。

如果你修改本程序并通过网络向他人提供服务，AGPL 要求你向他们提供对应的源代码。
