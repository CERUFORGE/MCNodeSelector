// Package config 负责 JSONC 配置的加载、默认值回填、中文校验（§6）。
// 设计约束：
//   - 零硬编码原则的落点之一：所有可配置项的默认值集中在本文件 Default() 中；
//   - 校验错误必须中文、带节点定位信息（序号或名称）；
//   - 加载失败返回 error，绝不 panic。
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrFirstRun 首次运行信号：配置文件不存在，已生成模板，请编辑后重启。
var ErrFirstRun = errors.New("首次运行：已生成配置模板")

// ---------- 配置结构体（与 §6.2 模板字段一一对应） ----------

// General 全局设置
type General struct {
	LogLevel            string `json:"log_level"`               // DEBUG/INFO/WARN/ERROR
	LogDir              string `json:"log_dir"`                 // 日志目录
	LogKeepDays         int    `json:"log_keep_days"`           // 日志保留天数
	DataDir             string `json:"data_dir"`                // 数据目录（history/state/lock）
	SingleInstance      *bool  `json:"single_instance"`         // 防双开（默认 true）
	AllNodesDownWaitSec int    `json:"all_nodes_down_wait_sec"` // 全灭时初始重试等待（逐次翻倍封顶600s）
	Language            string `json:"language"`                // 界面语言：zh / en（默认 zh）
	AutoUpdateCheck     *bool  `json:"auto_update_check"`       // 启动时检查新版本（默认 true）
	UpdateRepo          string `json:"update_repo"`             // GitHub 仓库 owner/repo（用于更新检查）
}

// Lang 规范化语言（默认 zh）
func (g General) Lang() string {
	if strings.EqualFold(g.Language, "en") {
		return "en"
	}
	return "zh"
}

// UpdateCheckEnabled 是否检查更新（缺省 true）
func (g General) UpdateCheckEnabled() bool { return g.AutoUpdateCheck == nil || *g.AutoUpdateCheck }

// IsSingleInstance 是否启用单实例锁（缺省 true）
func (g General) IsSingleInstance() bool { return g.SingleInstance == nil || *g.SingleInstance }

// Minecraft 本地 MC 服务端
type Minecraft struct {
	LocalHost       string `json:"local_host"`        // frpc 转发目标地址
	LocalPort       int    `json:"local_port"`        // frpc 转发目标端口（启动前检测是否有服务监听）
	ExtraLocalPorts []int  `json:"extra_local_ports"` // 额外同时监听的本地端口（多端口转发，可空）
}

// Frpc frpc 进程管理
type Frpc struct {
	Enabled              *bool  `json:"enabled"`                 // 是否启用 frpc 托管（默认 true；隧道由服务端建立时设 false，仅测速选优）
	Binary               string `json:"binary"`                  // frpc.exe 路径（相对程序目录或绝对路径）
	WorkDir              string `json:"work_dir"`                // 自动生成的配置与日志目录
	ProxyName            string `json:"proxy_name"`              // frpc 代理名称
	DefaultRemotePort    int    `json:"default_remote_port"`     // 建议统一端口（仅用于提示）
	MaxRestarts          int    `json:"max_restarts"`            // 滑动窗口内最大重启次数
	RestartWindowMin     int    `json:"restart_window_min"`      // 重启限流滑动窗口（分钟）
	RestartBackoffMaxSec int    `json:"restart_backoff_max_sec"` // 崩溃重启指数退避上限（秒）
}

// IsEnabled frpc 托管是否启用（缺省 true）
func (f Frpc) IsEnabled() bool { return f.Enabled == nil || *f.Enabled }

// MCCheck Phase2 深度验证设置
type MCCheck struct {
	Enabled   *bool  `json:"enabled"`    // 是否启用 MC 深度验证（默认 true）
	Mode      string `json:"mode"`       // direct=直连公网映射端口；tunnel=预留
	TopK      int    `json:"top_k"`      // 深度验证候选数
	Samples   int    `json:"samples"`    // MC ping 次数
	TimeoutMs int    `json:"timeout_ms"` // MC ping 单次超时
	Burst     int    `json:"burst"`      // 带负载突发小包次数（0=关；用于测繁忙时的 P95/尖峰）
}

// IsEnabled MC 验证是否启用（缺省 true）
func (m MCCheck) IsEnabled() bool { return m.Enabled == nil || *m.Enabled }

// Probing Phase1 TCP 测速设置
type Probing struct {
	Samples     int     `json:"samples"`     // 每节点采样次数
	IntervalMs  int     `json:"interval_ms"` // 采样间隔（节点内串行）
	TimeoutMs   int     `json:"timeout_ms"`  // 单次 TCP 超时
	Concurrency int     `json:"concurrency"` // 节点级并发上限（信号量）
	MCCheck     MCCheck `json:"mc_check"`
}

// Weights 各分项权重（全零为配置错误）
type Weights struct {
	Latency float64 `json:"latency"`
	P95     float64 `json:"p95"`
	Jitter  float64 `json:"jitter"`
	Loss    float64 `json:"loss"`
	Spike   float64 `json:"spike"` // 尖峰率权重（抗抖动：越爱抽的节点分越低）
	MC      float64 `json:"mc"`
	History float64 `json:"history"`
}

// AllZero 是否全部权重为零
func (w Weights) AllZero() bool {
	return w.Latency == 0 && w.P95 == 0 && w.Jitter == 0 && w.Loss == 0 &&
		w.Spike == 0 && w.MC == 0 && w.History == 0
}

// IdealMax 毫秒型分项的理想值/上限
type IdealMax struct {
	IdealMs float64 `json:"ideal_ms"`
	MaxMs   float64 `json:"max_ms"`
}

// IdealMaxPct 百分比型分项（丢包）
type IdealMaxPct struct {
	IdealPct float64 `json:"ideal_pct"`
	MaxPct   float64 `json:"max_pct"`
}

// HistoryCfg 历史分设置
type HistoryCfg struct {
	NeutralScore float64 `json:"neutral_score"` // 新节点中性历史分
	UpdateAlpha  float64 `json:"update_alpha"`  // EWMA 平滑系数
}

// Robust 稳健去尖峰设置（抗网络抖动：单次高延迟尖峰不应拖垮评分）
type Robust struct {
	Enabled    *bool   `json:"enabled"`        // 是否启用（默认 true）
	SpikeMadK  float64 `json:"spike_mad_k"`    // 尖峰判定：> 中位数 + k×MAD 视为尖峰（默认 3.5）
	SpikeFloor float64 `json:"spike_floor_ms"` // 尖峰判定下限毫秒（防止低延迟节点误判，默认 30）
	TrimPct    float64 `json:"trim_pct"`       // 去尖峰后再对称裁掉最高/最低百分比（默认 20）
}

// IsEnabled 是否启用稳健去尖峰（缺省 true）
func (r Robust) IsEnabled() bool { return r.Enabled == nil || *r.Enabled }

// Scoring 评分设置
type Scoring struct {
	Weights         Weights     `json:"weights"`
	Latency         IdealMax    `json:"latency"`
	P95             IdealMax    `json:"p95"`
	Jitter          IdealMax    `json:"jitter"`
	Loss            IdealMaxPct `json:"loss"`
	MC              IdealMax    `json:"mc"`
	Spike           IdealMaxPct `json:"spike"` // 尖峰率：ideal 0% 满分，≥max 0 分
	History         HistoryCfg  `json:"history"`
	Robust          Robust      `json:"robust"`
	UnstableLossPct float64     `json:"unstable_loss_pct"` // 丢包率超过该值 → UNSTABLE
}

// Switching 切换与监控设置
type Switching struct {
	MinDwellMin           int     `json:"min_dwell_min"`            // 最小驻留（分钟）
	AdvantageMargin       float64 `json:"advantage_margin"`         // 显著优势分数阈值（滞回）
	ConsecutiveWins       int     `json:"consecutive_wins"`         // 连续优势轮数
	CooldownMin           int     `json:"cooldown_min"`             // 切换冷却（分钟）
	MaxSwitchesPerHour    int     `json:"max_switches_per_hour"`    // 每小时切换上限（保险丝）
	FailThreshold         int     `json:"fail_threshold"`           // 当前节点连续失败次数 → 故障切换
	MonitorIntervalSec    int     `json:"monitor_interval_sec"`     // 监控探测间隔（秒）
	RetestIntervalMin     int     `json:"retest_interval_min"`      // 全量重测间隔（分钟）
	BlacklistMin          int     `json:"blacklist_min"`            // 黑名单时长（分钟）
	MonitorUseMCPing      *bool   `json:"monitor_use_mc_ping"`      // 监控用 MC ping（默认 true）
	PauseSwitchWhenActive *bool   `json:"pause_switch_when_active"` // 有玩家在线时暂停更优切换（默认 true）

	// 质量劣化自动切换：当前节点持续高延迟时，自动切到更稳节点（默认开启）
	DegradationSwitch *bool   `json:"degradation_switch"`       // 默认 true
	DegradationMs     float64 `json:"degradation_threshold_ms"` // 高延迟阈值（默认 300ms）
	DegradationTicks  int     `json:"degradation_ticks"`        // 连续命中次数（默认 2）
	DegradationDrop   *bool   `json:"degradation_drop_players"` // 切换时断开玩家连接强制重连（默认 true）

	SpikeMs            float64 `json:"spike_ms"`              // 尖峰判定阈值（端到端 RTT 超过该值记一次尖峰，默认 150ms）
	SpikeTargetPerHour int     `json:"spike_target_per_hour"` // 目标：每小时尖峰数不超过该值（默认 1，用于状态屏告警）
}

// UseMCPing 监控探测是否使用 MC ping（缺省 true）
func (s Switching) UseMCPing() bool { return s.MonitorUseMCPing == nil || *s.MonitorUseMCPing }

// PauseWhenActive 有玩家在线时是否暂停“更优切换”通道B（缺省 true；故障切换通道A不受影响）
func (s Switching) PauseWhenActive() bool {
	return s.PauseSwitchWhenActive == nil || *s.PauseSwitchWhenActive
}

// DegradeEnabled 是否启用“质量劣化自动切换”（缺省 true）
func (s Switching) DegradeEnabled() bool {
	return s.DegradationSwitch == nil || *s.DegradationSwitch
}

// DegradeDropPlayers 劣化切换时是否断开玩家连接强制重连（缺省 true）
func (s Switching) DegradeDropPlayers() bool {
	return s.DegradationDrop == nil || *s.DegradationDrop
}

// Hooks 外部钩子
type Hooks struct {
	OnSwitch       string `json:"on_switch"`         // 切换成功后执行
	OnAllNodesDown string `json:"on_all_nodes_down"` // 全灭兜底每轮执行
}

// Remote 远程节点列表配置（用户需求：节点列表托管到 EdgeOne，本地回退）
type Remote struct {
	URL      string `json:"url"`          // 远程节点列表 JSONC URL
	Timeout  int    `json:"timeout_ms"`   // 拉取超时（毫秒，默认 5000）
	Interval int    `json:"interval_min"` // 重新拉取间隔（分钟，默认 10）
}

// Multipath 多路冗余隧道（实验特性：把一条连接拆成多条路径并发冗余，抗弱网/掉线）
// 需在服务端运行 mcnode --agent；客户端把玩家连接同时走选中的路径。
type Multipath struct {
	Enabled     *bool    `json:"enabled"`      // 是否启用（默认 false）
	Auto        *bool    `json:"auto"`         // 自动挑选路径（默认 true；paths 非空时自选优先）
	PathCount   int      `json:"path_count"`   // 自动模式候选路径数（默认 5）
	Paths       []string `json:"paths"`        // 自选线路（优先于 auto）：节点名或 host:port；第1条=当前，第2条=备用
	AgentListen string   `json:"agent_listen"` // 服务端 agent 监听地址（--agent 模式用）
	AgentMC     string   `json:"agent_mc"`     // 服务端 agent 转发的本地 MC 地址
	AgentToken  string   `json:"agent_token"`  // agent 共享密钥（两端一致；为空则不校验，公网部署务必设置）

	// AgentPassthrough 允许 agent 对「非多线」连接做原始 TCP 透传（默认 true）。
	// 用途：客户端 MC 状态 ping 直接连 agent 端口时也能通。
	// 设为 false 后 agent 只接受带 token 的多线会话；此时请把 mc_check 指向 MC 公网端口。
	AgentPassthrough *bool `json:"agent_passthrough"`

	// 自适应多线：平时单线，仅在网络劣化时启用多路冗余
	Adaptive       *bool   `json:"adaptive"`         // 是否自适应（默认 true）
	Redundancy     int     `json:"redundancy"`       // 启用多线时的最少路径数（默认 2）
	RedundancyMax  int     `json:"redundancy_max"`   // 自适应最多路径数（默认 3；进多线前按实测在 redundancy~max 间选）
	EngageJitterMs float64 `json:"engage_jitter_ms"` // 端到端抖动 EWMA 超过该值(ms)→启用多线（默认 80）
	EngageLossPct  float64 `json:"engage_loss_pct"`  // 端到端丢包 EWMA 超过该值(%)→启用多线（默认 10）
	EngageTicks    int     `json:"engage_ticks"`     // 连续劣化多少次才启用多线（滞回，默认 3）
	EngageSpikeMs  float64 `json:"engage_spike_ms"`  // 单次端到端 RTT 超过该值(ms)→立即启用多线（默认 200）
	CriticalAlways *bool   `json:"critical_always"`  // 关键小包常开冗余：单线时也保留第 2 条路径（默认 false）
	LargePaths     int     `json:"large_paths"`      // 大包冗余路径数（默认 1：减少共用上行放大；2=更抗单路丢失但上行翻倍）
	FailoverOnly   *bool   `json:"failover_only"`    // 只故障切换、不复制冗余（弱网/共用瓶径下最稳，默认 false）
	AutoRedundancy *bool   `json:"auto_redundancy"`  // 按当前网络质量自动决定是否复制（默认 true，自校准）
	QueueRiseMs    float64 `json:"queue_rise_ms"`    // 拥塞判据：RTT 比该路径自身基线高这么多(ms)→停止复制（默认 30）
	WeakJitterMs   float64 `json:"weak_jitter_ms"`   // 抖动判据：超过该值(ms)视为需冗余的弱链路（默认 25）
	DisengageSec   int     `json:"disengage_sec"`    // 稳定多少秒后关闭多线（默认 120）
}

// IsEnabled 是否启用多路冗余（缺省 false）
func (m Multipath) IsEnabled() bool { return m.Enabled != nil && *m.Enabled }

// IsAuto 是否自动挑选路径（缺省 true）
func (m Multipath) IsAuto() bool { return m.Auto == nil || *m.Auto }

// IsAdaptive 是否自适应（缺省 true）：平时单线，劣化时才启用多线
func (m Multipath) IsAdaptive() bool { return m.Adaptive == nil || *m.Adaptive }

// CriticalAlwaysOn 关键小包常开冗余（缺省 false）：单线时也保留第 2 条路径，小包双发
func (m Multipath) CriticalAlwaysOn() bool { return m.CriticalAlways != nil && *m.CriticalAlways }

// FailoverOnlyOn 只故障切换不复制（缺省 false）
func (m Multipath) FailoverOnlyOn() bool { return m.FailoverOnly != nil && *m.FailoverOnly }

// AutoRedundancyOn 按当前网络质量自动决定是否复制（缺省 true）
func (m Multipath) AutoRedundancyOn() bool { return m.AutoRedundancy == nil || *m.AutoRedundancy }

// AgentPassthroughOn 允许 agent 做非多线原始透传（缺省 true）。
func (m Multipath) AgentPassthroughOn() bool { return m.AgentPassthrough == nil || *m.AgentPassthrough }

// Override 节点级探测端点覆盖（留空/0 = 用默认值）
type Override struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

// IsSet 该覆盖是否生效
func (o Override) IsSet() bool { return strings.TrimSpace(o.Host) != "" && o.Port > 0 }

// Node 单个 FRP 节点定义（F1.2）
type Node struct {
	Name       string   `json:"name"`        // 唯一名称
	ServerAddr string   `json:"server_addr"` // frps 地址
	ServerPort int      `json:"server_port"` // frps bindPort
	Token      string   `json:"token"`
	User       string   `json:"user"`        // 可空
	RemotePort int      `json:"remote_port"` // 公网映射端口
	Region     string   `json:"region"`      // 扩展属性：区域
	Tags       []string `json:"tags"`        // 扩展属性：标签
	Comment    string   `json:"comment"`     // 扩展属性：备注
	Weight     float64  `json:"weight"`      // 权重（默认 1.0）
	Enabled    *bool    `json:"enabled"`     // 是否启用（缺省 true）
	Probe      Override `json:"probe"`       // Phase1 探测端点覆盖
	MCCheck    Override `json:"mc_check"`    // Phase2 MC 验证端点覆盖
}

// IsEnabled 节点是否启用（缺省 true）
func (n *Node) IsEnabled() bool { return n.Enabled == nil || *n.Enabled }

// ProbeAddr Phase1 探测端点：覆盖值或默认 server_addr:server_port（§4.2）
func (n *Node) ProbeAddr() (string, int) {
	if n.Probe.IsSet() {
		return n.Probe.Host, n.Probe.Port
	}
	return n.ServerAddr, n.ServerPort
}

// MCAddr Phase2 MC 验证端点：覆盖值优先；否则用「玩家实际连接的公网 MC 端口」——
// 即与转发目标一致（server_addr:server_port）。这样才能测到含服务器段的端到端 RTT。
func (n *Node) MCAddr() (string, int) {
	if n.MCCheck.IsSet() {
		return n.MCCheck.Host, n.MCCheck.Port
	}
	return n.ServerAddr, n.ServerPort
}

// Config 根配置
type Config struct {
	General   General   `json:"general"`
	Minecraft Minecraft `json:"minecraft"`
	Frpc      Frpc      `json:"frpc"`
	Probing   Probing   `json:"probing"`
	Scoring   Scoring   `json:"scoring"`
	Switching Switching `json:"switching"`
	Hooks     Hooks     `json:"hooks"`
	Remote    Remote    `json:"remote"`
	Multipath Multipath `json:"multipath"`
	Nodes     []Node    `json:"nodes"`
}

// ---------- 默认值 ----------

func boolPtr(b bool) *bool { return &b }

// Default 返回一份带全部默认值的配置（数值即 §6.2 模板注释中的默认值）
func Default() *Config {
	t := true
	return &Config{
		General: General{
			LogLevel: "INFO", LogDir: "logs", LogKeepDays: 7, DataDir: "data",
			SingleInstance: &t, AllNodesDownWaitSec: 30,
			Language: "zh", AutoUpdateCheck: &t,
		},
		Minecraft: Minecraft{LocalHost: "127.0.0.1", LocalPort: 25565},
		Frpc: Frpc{
			Binary: "frpc.exe", WorkDir: "frpc_run", ProxyName: "mc",
			DefaultRemotePort: 25565, MaxRestarts: 5, RestartWindowMin: 10, RestartBackoffMaxSec: 60,
		},
		Probing: Probing{
			Samples: 10, IntervalMs: 200, TimeoutMs: 3000, Concurrency: 8,
			MCCheck: MCCheck{Enabled: &t, Mode: "direct", TopK: 3, Samples: 3, TimeoutMs: 5000, Burst: 8},
		},
		Scoring: Scoring{
			Weights:         Weights{Latency: 30, P95: 10, Jitter: 15, Loss: 20, Spike: 15, MC: 15, History: 10},
			Latency:         IdealMax{IdealMs: 30, MaxMs: 300},
			P95:             IdealMax{IdealMs: 60, MaxMs: 500},
			Jitter:          IdealMax{IdealMs: 10, MaxMs: 150},
			Loss:            IdealMaxPct{IdealPct: 0, MaxPct: 20},
			MC:              IdealMax{IdealMs: 50, MaxMs: 800},
			Spike:           IdealMaxPct{IdealPct: 0, MaxPct: 25},
			History:         HistoryCfg{NeutralScore: 70, UpdateAlpha: 0.3},
			Robust:          Robust{Enabled: &t, SpikeMadK: 3.5, SpikeFloor: 30, TrimPct: 20},
			UnstableLossPct: 10,
		},
		Switching: Switching{
			MinDwellMin: 10, AdvantageMargin: 15, ConsecutiveWins: 3, CooldownMin: 10,
			MaxSwitchesPerHour: 4, FailThreshold: 5, MonitorIntervalSec: 10,
			RetestIntervalMin: 30, BlacklistMin: 15, MonitorUseMCPing: &t,
			PauseSwitchWhenActive: &t,
			DegradationSwitch:     &t, DegradationMs: 300, DegradationTicks: 2, DegradationDrop: &t,
			SpikeMs: 150, SpikeTargetPerHour: 1,
		},
		Remote: Remote{Timeout: 5000, Interval: 10},
		Multipath: Multipath{AgentListen: "127.0.0.1:7001", Auto: boolPtr(true), PathCount: 5,
			Adaptive: boolPtr(true), Redundancy: 2, RedundancyMax: 3, EngageJitterMs: 80, EngageLossPct: 10, EngageTicks: 3, EngageSpikeMs: 200, CriticalAlways: boolPtr(false), LargePaths: 1, FailoverOnly: boolPtr(false), AutoRedundancy: boolPtr(true), QueueRiseMs: 30, WeakJitterMs: 25, DisengageSec: 120},
	}
}

// isZeroScoring 判断 scoring 节是否整体缺失（用于区分“没写”与“写了但全零”）
func isZeroScoring(s Scoring) bool {
	return s.Weights.AllZero() && s.Latency == IdealMax{} && s.P95 == IdealMax{} &&
		s.Jitter == IdealMax{} && s.Loss == IdealMaxPct{} && s.Spike == IdealMaxPct{} &&
		s.History == HistoryCfg{} && s.Robust == Robust{} && s.UnstableLossPct == 0
}

// applyDefaults 用默认值回填空字段（0 / 空串 / 缺省布尔）
func applyDefaults(c *Config) {
	d := Default()
	// general
	if c.General.LogLevel == "" {
		c.General.LogLevel = d.General.LogLevel
	}
	if c.General.LogDir == "" {
		c.General.LogDir = d.General.LogDir
	}
	if c.General.LogKeepDays <= 0 {
		c.General.LogKeepDays = d.General.LogKeepDays
	}
	if c.General.DataDir == "" {
		c.General.DataDir = d.General.DataDir
	}
	if c.General.SingleInstance == nil {
		c.General.SingleInstance = d.General.SingleInstance
	}
	if c.General.AllNodesDownWaitSec <= 0 {
		c.General.AllNodesDownWaitSec = d.General.AllNodesDownWaitSec
	}
	if c.General.Language == "" {
		c.General.Language = d.General.Language
	}
	if c.General.AutoUpdateCheck == nil {
		c.General.AutoUpdateCheck = d.General.AutoUpdateCheck
	}
	// minecraft
	if c.Minecraft.LocalHost == "" {
		c.Minecraft.LocalHost = d.Minecraft.LocalHost
	}
	if c.Minecraft.LocalPort <= 0 {
		c.Minecraft.LocalPort = d.Minecraft.LocalPort
	}
	// frpc
	if c.Frpc.Enabled == nil {
		c.Frpc.Enabled = d.Frpc.Enabled
	}
	if c.Frpc.Binary == "" {
		c.Frpc.Binary = d.Frpc.Binary
	}
	if c.Frpc.WorkDir == "" {
		c.Frpc.WorkDir = d.Frpc.WorkDir
	}
	if c.Frpc.ProxyName == "" {
		c.Frpc.ProxyName = d.Frpc.ProxyName
	}
	if c.Frpc.DefaultRemotePort <= 0 {
		c.Frpc.DefaultRemotePort = d.Frpc.DefaultRemotePort
	}
	if c.Frpc.MaxRestarts <= 0 {
		c.Frpc.MaxRestarts = d.Frpc.MaxRestarts
	}
	if c.Frpc.RestartWindowMin <= 0 {
		c.Frpc.RestartWindowMin = d.Frpc.RestartWindowMin
	}
	if c.Frpc.RestartBackoffMaxSec <= 0 {
		c.Frpc.RestartBackoffMaxSec = d.Frpc.RestartBackoffMaxSec
	}
	// probing
	if c.Probing.Samples <= 0 {
		c.Probing.Samples = d.Probing.Samples
	}
	if c.Probing.IntervalMs <= 0 {
		c.Probing.IntervalMs = d.Probing.IntervalMs
	}
	if c.Probing.TimeoutMs <= 0 {
		c.Probing.TimeoutMs = d.Probing.TimeoutMs
	}
	if c.Probing.Concurrency <= 0 {
		c.Probing.Concurrency = d.Probing.Concurrency
	}
	if c.Probing.MCCheck.Enabled == nil {
		c.Probing.MCCheck.Enabled = d.Probing.MCCheck.Enabled
	}
	if c.Probing.MCCheck.Mode == "" {
		c.Probing.MCCheck.Mode = d.Probing.MCCheck.Mode
	}
	if c.Probing.MCCheck.TopK <= 0 {
		c.Probing.MCCheck.TopK = d.Probing.MCCheck.TopK
	}
	if c.Probing.MCCheck.Samples <= 0 {
		c.Probing.MCCheck.Samples = d.Probing.MCCheck.Samples
	}
	if c.Probing.MCCheck.TimeoutMs <= 0 {
		c.Probing.MCCheck.TimeoutMs = d.Probing.MCCheck.TimeoutMs
	}
	if c.Probing.MCCheck.Burst <= 0 {
		c.Probing.MCCheck.Burst = d.Probing.MCCheck.Burst
	}
	// scoring：整个节点缺失时用默认；部分填写则只回填空项
	if isZeroScoring(c.Scoring) {
		c.Scoring = d.Scoring
	} else {
		if c.Scoring.Latency == (IdealMax{}) {
			c.Scoring.Latency = d.Scoring.Latency
		}
		if c.Scoring.P95 == (IdealMax{}) {
			c.Scoring.P95 = d.Scoring.P95
		}
		if c.Scoring.Jitter == (IdealMax{}) {
			c.Scoring.Jitter = d.Scoring.Jitter
		}
		if c.Scoring.Loss == (IdealMaxPct{}) {
			c.Scoring.Loss = d.Scoring.Loss
		}
		if c.Scoring.Spike == (IdealMaxPct{}) {
			c.Scoring.Spike = d.Scoring.Spike
		}
		if c.Scoring.MC == (IdealMax{}) {
			c.Scoring.MC = d.Scoring.MC
		}
		if c.Scoring.History == (HistoryCfg{}) {
			c.Scoring.History = d.Scoring.History
		}
		if c.Scoring.UnstableLossPct <= 0 {
			c.Scoring.UnstableLossPct = d.Scoring.UnstableLossPct
		}
		// 稳健去尖峰（抗抖动）
		if c.Scoring.Robust.Enabled == nil {
			c.Scoring.Robust.Enabled = d.Scoring.Robust.Enabled
		}
		if c.Scoring.Robust.SpikeMadK <= 0 {
			c.Scoring.Robust.SpikeMadK = d.Scoring.Robust.SpikeMadK
		}
		if c.Scoring.Robust.SpikeFloor <= 0 {
			c.Scoring.Robust.SpikeFloor = d.Scoring.Robust.SpikeFloor
		}
		if c.Scoring.Robust.TrimPct < 0 {
			c.Scoring.Robust.TrimPct = 0
		}
		if c.Scoring.Robust.TrimPct > 45 {
			c.Scoring.Robust.TrimPct = 45
		}
	}
	// switching
	if c.Switching.MinDwellMin <= 0 {
		c.Switching.MinDwellMin = d.Switching.MinDwellMin
	}
	if c.Switching.AdvantageMargin <= 0 {
		c.Switching.AdvantageMargin = d.Switching.AdvantageMargin
	}
	if c.Switching.ConsecutiveWins <= 0 {
		c.Switching.ConsecutiveWins = d.Switching.ConsecutiveWins
	}
	if c.Switching.CooldownMin <= 0 {
		c.Switching.CooldownMin = d.Switching.CooldownMin
	}
	if c.Switching.MaxSwitchesPerHour <= 0 {
		c.Switching.MaxSwitchesPerHour = d.Switching.MaxSwitchesPerHour
	}
	if c.Switching.FailThreshold <= 0 {
		c.Switching.FailThreshold = d.Switching.FailThreshold
	}
	if c.Switching.MonitorIntervalSec <= 0 {
		c.Switching.MonitorIntervalSec = d.Switching.MonitorIntervalSec
	}
	if c.Switching.RetestIntervalMin <= 0 {
		c.Switching.RetestIntervalMin = d.Switching.RetestIntervalMin
	}
	if c.Switching.BlacklistMin <= 0 {
		c.Switching.BlacklistMin = d.Switching.BlacklistMin
	}
	if c.Switching.MonitorUseMCPing == nil {
		c.Switching.MonitorUseMCPing = d.Switching.MonitorUseMCPing
	}
	if c.Switching.PauseSwitchWhenActive == nil {
		c.Switching.PauseSwitchWhenActive = d.Switching.PauseSwitchWhenActive
	}
	if c.Switching.DegradationSwitch == nil {
		c.Switching.DegradationSwitch = d.Switching.DegradationSwitch
	}
	if c.Switching.DegradationMs <= 0 {
		c.Switching.DegradationMs = d.Switching.DegradationMs
	}
	if c.Switching.DegradationTicks <= 0 {
		c.Switching.DegradationTicks = d.Switching.DegradationTicks
	}
	if c.Switching.SpikeMs <= 0 {
		c.Switching.SpikeMs = d.Switching.SpikeMs
	}
	if c.Switching.SpikeTargetPerHour < 0 {
		c.Switching.SpikeTargetPerHour = d.Switching.SpikeTargetPerHour
	}
	if c.Switching.DegradationDrop == nil {
		c.Switching.DegradationDrop = d.Switching.DegradationDrop
	}
	// remote
	if c.Remote.Timeout <= 0 {
		c.Remote.Timeout = d.Remote.Timeout
	}
	if c.Remote.Interval <= 0 {
		c.Remote.Interval = d.Remote.Interval
	}
	// multipath
	if c.Multipath.AgentListen == "" {
		c.Multipath.AgentListen = d.Multipath.AgentListen
	}
	if c.Multipath.Auto == nil {
		c.Multipath.Auto = d.Multipath.Auto
	}
	if c.Multipath.PathCount <= 0 {
		c.Multipath.PathCount = d.Multipath.PathCount
	}
	if c.Multipath.Adaptive == nil {
		c.Multipath.Adaptive = d.Multipath.Adaptive
	}
	if c.Multipath.Redundancy <= 0 {
		c.Multipath.Redundancy = d.Multipath.Redundancy
	}
	if c.Multipath.RedundancyMax <= 0 {
		c.Multipath.RedundancyMax = d.Multipath.RedundancyMax
	}
	if c.Multipath.RedundancyMax < c.Multipath.Redundancy {
		c.Multipath.RedundancyMax = c.Multipath.Redundancy
	}
	if c.Multipath.EngageJitterMs <= 0 {
		c.Multipath.EngageJitterMs = d.Multipath.EngageJitterMs
	}
	if c.Multipath.EngageLossPct <= 0 {
		c.Multipath.EngageLossPct = d.Multipath.EngageLossPct
	}
	if c.Multipath.EngageTicks <= 0 {
		c.Multipath.EngageTicks = d.Multipath.EngageTicks
	}
	if c.Multipath.EngageSpikeMs <= 0 {
		c.Multipath.EngageSpikeMs = d.Multipath.EngageSpikeMs
	}
	if c.Multipath.CriticalAlways == nil {
		c.Multipath.CriticalAlways = d.Multipath.CriticalAlways
	}
	if c.Multipath.LargePaths <= 0 {
		c.Multipath.LargePaths = d.Multipath.LargePaths
	}
	if c.Multipath.FailoverOnly == nil {
		c.Multipath.FailoverOnly = d.Multipath.FailoverOnly
	}
	if c.Multipath.AutoRedundancy == nil {
		c.Multipath.AutoRedundancy = d.Multipath.AutoRedundancy
	}
	if c.Multipath.QueueRiseMs <= 0 {
		c.Multipath.QueueRiseMs = d.Multipath.QueueRiseMs
	}
	if c.Multipath.WeakJitterMs <= 0 {
		c.Multipath.WeakJitterMs = d.Multipath.WeakJitterMs
	}
	if c.Multipath.DisengageSec <= 0 {
		c.Multipath.DisengageSec = d.Multipath.DisengageSec
	}
	// nodes：weight 缺省 1.0（0 视为未填写）
	for i := range c.Nodes {
		if c.Nodes[i].Weight == 0 {
			c.Nodes[i].Weight = 1.0
		}
	}
}

// ---------- 加载与校验 ----------

// Load 加载配置文件：
//  1. 文件不存在 → 自动生成模板并返回 ErrFirstRun（F15）；
//  2. 剥离注释 → JSON 解析（语法错误给中文提示 + 常见原因三连）；
//  3. 回填默认值 → 校验（错误含节点定位）。
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// 首次运行：生成带完整中文注释的模板（§6.2）
			if werr := os.WriteFile(path, []byte(Template), 0o644); werr != nil {
				return nil, fmt.Errorf("生成配置模板失败（%s）：%w，请检查目录权限后重试", path, werr)
			}
			return nil, ErrFirstRun
		}
		return nil, fmt.Errorf("读取配置文件失败（%s）：%w", path, err)
	}
	// Windows 记事本 / PowerShell 可能写入 UTF-8 BOM；先剥掉再解析，避免报
	// "invalid character '\ufeff'".
	raw = bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF})
	stripped := StripComments(raw)
	var c Config
	if err := json.Unmarshal(stripped, &c); err != nil {
		return nil, fmt.Errorf("配置文件语法错误（%s）：%w%s", path, err, jsonHints(stripped, err))
	}
	applyDefaults(&c)
	if err := Validate(&c); err != nil {
		return nil, err
	}
	return &c, nil
}

// jsonHints 为 JSON 语法错误附加行号定位与“常见原因三连”（§6.3）
func jsonHints(stripped []byte, err error) string {
	line := 0
	var se *json.SyntaxError
	if errors.As(err, &se) {
		off := int(se.Offset)
		if off > len(stripped) {
			off = len(stripped)
		}
		if off < 0 {
			off = 0
		}
		for _, b := range stripped[:off] {
			if b == '\n' {
				line++
			}
		}
	}
	return fmt.Sprintf("\n  位置：第 %d 行附近\n  常见原因：1) 少了引号或逗号；2) 多了尾随逗号（JSON 不允许最后一项后面带逗号）；3) 误用了中文引号 “” ‘’（必须用英文引号）", line+1)
}

// Validate 校验配置，全部返回中文错误并带节点定位（§6.3）
// 若配置了 remote.url，则本地 nodes[] 允许为空（远程为主，本地作回退兜底）。
func Validate(c *Config) error {
	// 节点来源为空不再硬报错（允许先 --setup 生成、再补充节点）；引擎会在无节点时进入退避等待。
	// 节点级校验（本地节点）
	seen := make(map[string]bool, len(c.Nodes))
	for i, n := range c.Nodes {
		where := fmt.Sprintf("节点[%d]", i+1)
		if strings.TrimSpace(n.Name) != "" {
			where = fmt.Sprintf("节点[%d]（%s）", i+1, n.Name)
		}
		if strings.TrimSpace(n.Name) == "" {
			return fmt.Errorf("配置错误：%s 的 name 为空，每个节点必须有唯一名称", where)
		}
		if seen[n.Name] {
			return fmt.Errorf("配置错误：%s 的 name 重复（“%s”已出现过），节点名称必须唯一", where, n.Name)
		}
		seen[n.Name] = true
		if strings.TrimSpace(n.ServerAddr) == "" {
			return fmt.Errorf("配置错误：%s 的 server_addr 为空，请填写 frps 服务器地址（域名或 IP）", where)
		}
		if n.ServerPort < 1 || n.ServerPort > 65535 {
			return fmt.Errorf("配置错误：%s 的 server_port（%d）必须在 1~65535 之间（frps 的 bindPort）", where, n.ServerPort)
		}
		if n.RemotePort < 1 || n.RemotePort > 65535 {
			return fmt.Errorf("配置错误：%s 的 remote_port（%d）必须在 1~65535 之间（玩家连接的公网映射端口）", where, n.RemotePort)
		}
		if n.Weight < 0 {
			return fmt.Errorf("配置错误：%s 的 weight（%.2f）不能为负数（<1 降权，>1 偏好）", where, n.Weight)
		}
	}
	// 评分权重全零
	if c.Scoring.Weights.AllZero() {
		return errors.New("配置错误：scoring.weights 六个权重全为零，请至少给一个分项配置大于 0 的权重")
	}
	return nil
}

// ---------- 路径解析 ----------

// ExeDir 返回可执行文件所在目录（相对路径的解析基准）
func ExeDir() string {
	exe, err := os.Executable()
	if err != nil {
		// 极端情况下取当前工作目录兜底，绝不 panic
		wd, _ := os.Getwd()
		return wd
	}
	return filepath.Dir(exe)
}

// AbsPath 相对路径基于程序目录解析；绝对路径原样返回
func AbsPath(p string) string {
	if p == "" {
		return p
	}
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(ExeDir(), p)
}

// LogDir 日志目录（绝对路径）
func (c *Config) LogDir() string { return AbsPath(c.General.LogDir) }

// DataDir 数据目录（绝对路径）
func (c *Config) DataDir() string { return AbsPath(c.General.DataDir) }

// FrpcWorkDir frpc 工作目录（绝对路径）
func (c *Config) FrpcWorkDir() string { return AbsPath(c.Frpc.WorkDir) }

// FrpcBinary frpc 可执行文件路径（相对程序目录或绝对路径，§6.2）
func (c *Config) FrpcBinary() string { return AbsPath(c.Frpc.Binary) }

// HistoryPath 历史数据文件路径
func (c *Config) HistoryPath() string { return filepath.Join(c.DataDir(), "history.json") }

// StatePath 运行状态文件路径
func (c *Config) StatePath() string { return filepath.Join(c.DataDir(), "state.json") }

// LockPath 单实例锁文件路径
func (c *Config) LockPath() string { return filepath.Join(c.DataDir(), "mcnode.lock") }

// FindNode 按名称查找节点（不存在返回 nil）
func (c *Config) FindNode(name string) *Node {
	for i := range c.Nodes {
		if c.Nodes[i].Name == name {
			return &c.Nodes[i]
		}
	}
	return nil
}

// EnabledNodes 返回全部启用节点
func (c *Config) EnabledNodes() []*Node {
	out := make([]*Node, 0, len(c.Nodes))
	for i := range c.Nodes {
		if c.Nodes[i].IsEnabled() {
			out = append(out, &c.Nodes[i])
		}
	}
	return out
}
