package config

// Template 首次运行自动生成的默认配置模板（§6.2 原文，含全部中文注释）。
// 注意：使用反引号原始字符串，模板内不得出现反引号字符。
const Template = `{
  "general": {
    "log_level": "INFO",             // DEBUG / INFO / WARN / ERROR
    "log_dir": "logs",
    "log_keep_days": 7,
    "data_dir": "data",
    "single_instance": true,         // 防双开
    "all_nodes_down_wait_sec": 30    // 全部节点不可用时的初始重试等待（逐次翻倍，封顶600s）
  },
  "minecraft": {
    "local_host": "127.0.0.1",       // 本地 MC 服务端地址（frpc 转发目标）
    "local_port": 25565              // 本地 MC 服务端端口；启动前检测是否有服务监听
  },
  "frpc": {
    "enabled": true,               // 是否托管 frpc（隧道由服务端建立时设 false，仅测速选优）
    "binary": "frpc.exe",            // frpc 路径（相对程序目录或绝对路径）
    "work_dir": "frpc_run",          // 自动生成的配置与日志目录
    "proxy_name": "mc",              // frpc 代理名称
    "default_remote_port": 25565,    // 建议所有节点 remote_port 与此一致
    "max_restarts": 5,               // 滑动窗口内最大重启次数，超过→切换节点
    "restart_window_min": 10,        // 重启限流滑动窗口（分钟）
    "restart_backoff_max_sec": 60    // 崩溃重启指数退避上限（1→2→4…→60s）
  },
  "probing": {
    "samples": 10,                   // Phase1 每节点采样次数
    "interval_ms": 200,              // 采样间隔
    "timeout_ms": 3000,              // 单次 TCP 超时
    "concurrency": 8,                // 节点级并发上限
    "mc_check": {
      "enabled": true,               // 是否启用 MC 深度验证（Phase2）
      "mode": "direct",              // direct=ping 节点公网映射端口；tunnel=预留
      "top_k": 3,                    // 深度验证候选数
      "samples": 3,
      "timeout_ms": 5000,
      "burst": 8                     // 带负载突发小包次数（测繁忙时的 P95/尖峰；0/负=默认）
    }
  },
  "scoring": {
    "weights": { "latency": 30, "p95": 10, "jitter": 15, "loss": 20, "spike": 15, "mc": 15, "history": 10 },
    "latency": { "ideal_ms": 30,  "max_ms": 300 },
    "p95":      { "ideal_ms": 60,  "max_ms": 500 },
    "jitter":   { "ideal_ms": 10,  "max_ms": 150 },
    "loss":     { "ideal_pct": 0,  "max_pct": 20 },
    "spike":    { "ideal_pct": 0,  "max_pct": 25 },
    "mc":       { "ideal_ms": 50,  "max_ms": 800 },
    "history":  { "neutral_score": 70, "update_alpha": 0.3 },
    "robust":   { "enabled": true, "spike_mad_k": 3.5, "spike_floor_ms": 30, "trim_pct": 20 },  // 抗抖动：去尖峰后再评分
    "unstable_loss_pct": 10
  },
  "switching": {
    "min_dwell_min": 10,             // 最小驻留（更优切换不受限；故障切换不受此限）
    "advantage_margin": 15,          // 显著优势分数阈值（滞回）
    "consecutive_wins": 3,           // 连续优势轮数
    "cooldown_min": 10,              // 切换冷却
    "max_switches_per_hour": 4,      // 每小时切换上限（保险丝）
    "fail_threshold": 5,             // 当前节点连续失败次数→故障切换
    "monitor_interval_sec": 10,      // 监控探测间隔
    "retest_interval_min": 30,       // 全量重测间隔
    "blacklist_min": 15,             // 失败节点临时黑名单时长（全员黑名单时自动清空）
    "monitor_use_mc_ping": true,     // 监控用 MC ping（更真实）
    "pause_switch_when_active": true, // 有玩家在线时暂停更优切换并跳过全量重测
    "degradation_switch": true,       // 当前节点持续高延迟时自动切走
    "degradation_threshold_ms": 300,  // 高延迟阈值（毫秒）
    "degradation_ticks": 2,           // 连续命中多少次才切
    "degradation_drop_players": true, // 切走时断开现有玩家连接强制重连
    "spike_ms": 150,                  // 尖峰判定阈值（端到端 RTT 超过记一次尖峰）
    "spike_target_per_hour": 1        // 目标：每小时尖峰数不超过该值
  },
  "hooks": {
    // on_switch 切换成功后执行，环境变量注入：
    //   MC_NODE_NAME / MC_NODE_ADDR / MC_NODE_PORT / MC_REMOTE_PORT
    // 典型用途：更新 DDNS 解析，让玩家用固定域名连接、切换无感
    "on_switch": "",
    "on_all_nodes_down": ""
  },
  "remote": {
    // 远程节点列表（托管到 EdgeOne / 任意可访问的 HTTP 静态文件）
    // 拉取失败时自动回退本地 nodes[]，不影响运行
    // 文件格式：纯数组 [{...}, ...] 或 {"nodes": [...]}，字段同本地节点
    "url": "",              // 为空则只用本地节点；填写后远程为权威源，本地同名节点可覆盖远程
    "timeout_ms": 5000,     // 拉取超时
    "interval_min": 10      // 重新拉取间隔（分钟）
  },
  "multipath": {
    // 多路冗余隧道（实验特性）：玩家连接同时走所有路径，任一节点抽风/挂掉都不掉线。
    // 需服务端运行 mcnode --agent，并用 frpc 把 agent_listen 映射到多个节点。
    "enabled": false,
    "auto": true,                     // 自动挑选路径（按端到端总分）
    "adaptive": true,                 // 自适应：平时单线，劣化时才开多线
    "redundancy": 2,                  // 启用多线时的最少路径数
    "redundancy_max": 3,              // 自适应最多路径数（进多线前按实测在 redundancy~max 间自动选）
    "engage_jitter_ms": 80,           // 端到端抖动 EWMA 超过该值(ms)→启用多线
    "engage_loss_pct": 10,            // 端到端丢包 EWMA 超过该值(%)→启用多线
    "engage_ticks": 3,                // 连续劣化多少次才启用多线（滞回）
    "engage_spike_ms": 200,           // 单次端到端 RTT 超过该值(ms)→立即启用多线（抓偶发尖峰）
    "critical_always": false,         // 关键小包常开冗余：单线时也保留第 2 条路径、小包双发
    "large_paths": 1,                 // 大包冗余路径数（1=省上行，2=更抗单路丢失但上行翻倍）
    "failover_only": false,           // 只故障切换、不复制冗余（弱网/共用瓶径下最稳）
    "auto_redundancy": true,          // 按当前网质自动决定是否复制（自校准：独立+有丢包才复制）
    "queue_rise_ms": 30,              // 拥塞判据：RTT 比该路径自身基线高这么多(ms) → 停止复制
    "weak_jitter_ms": 25,             // 抖动判据：超过该值(ms) 视为需冗余的弱链路
    "disengage_sec": 120,             // 稳定多少秒后关闭多线
    "path_count": 5,                  // 自动挑选的候选路径数
    "paths": [],                      // 自选线路（优先于 auto）：填节点名或 host:port；第1条=当前节点，第2条=备用节点
    "agent_listen": "127.0.0.1:7001",  // 服务端 agent 监听地址（frpc 在另一台机器时才改 0.0.0.0:7001）
    "agent_passthrough": true,         // 允许 MC 状态 ping 直连 agent 端口；设 false 则只接受带 token 的多线会话
    "agent_mc": ""                    // 服务端 agent 转发目标（留空=127.0.0.1:25565）
  },
  "nodes": [
    {
      "name": "示例节点-请修改",        // 唯一名称
      "server_addr": "frp.example.com", // frps 地址（域名或IP）
      "server_port": 7000,             // frps bindPort（Phase1 探测端口）
      "token": "你的frps-token",
      "user": "",
      "remote_port": 25565,            // 公网映射端口（建议所有节点统一）
      "region": "cn-example",
      "tags": ["示例"],
      "comment": "备注",
      "weight": 1.0,                   // <1 降权，>1 偏好
      "enabled": true,
      "probe":    { "host": "", "port": 0 },  // 留空/0=用默认
      "mc_check": { "host": "", "port": 0 }   // 留空/0=用默认
    }
    // 任意增删节点对象，无需改代码
  ]
}
`
