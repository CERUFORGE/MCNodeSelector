// Package model 定义全程序共享的核心数据结构：
// 节点状态机、测速统计、MC 验证结果、评分结果、运行时节点状态。
package model

import "time"

// NodeStatus 节点状态机的状态集合（F6，共 9 态）
type NodeStatus string

const (
	// StatusOnline 在线：采样全部成功且丢包率低于不稳定阈值
	StatusOnline NodeStatus = "ONLINE"
	// StatusOffline 离线：全部采样失败且错误类别为“连接被拒绝”
	StatusOffline NodeStatus = "OFFLINE"
	// StatusTimeout 超时：全部采样失败且错误类别为超时/DNS/其他
	StatusTimeout NodeStatus = "TIMEOUT"
	// StatusUnstable 不稳定：丢包率 >= unstable_loss_pct（仍可被选择，但分数通常已低）
	StatusUnstable NodeStatus = "UNSTABLE"
	// StatusDisabled 已停用：enabled=false，跳过测速与选择
	StatusDisabled NodeStatus = "DISABLED"
	// StatusTesting 测速中
	StatusTesting NodeStatus = "TESTING"
	// StatusBlacklisted 黑名单：本轮彻底失败，期内跳过测速与选择，到期自动复出
	StatusBlacklisted NodeStatus = "BLACKLISTED"
	// StatusInternalErr 内部错误：该节点测速逻辑 panic，已被隔离
	StatusInternalErr NodeStatus = "INTERNAL_ERROR"
	// StatusCooldown 冷却展示态（可并入 BLACKLISTED 展示）
	StatusCooldown NodeStatus = "COOLDOWN"
)

// String 返回状态文本
func (s NodeStatus) String() string { return string(s) }

// ProbeStats Phase1 TCP 探测统计指标（§7.1）
type ProbeStats struct {
	MinMs    float64 // 成功样本最小 RTT（毫秒）
	AvgMs    float64 // 成功样本平均 RTT（启用稳健去尖峰时为去尖峰后均值）
	MaxMs    float64 // 成功样本最大 RTT
	P95Ms    float64 // P95（启用稳健去尖峰时为去尖峰后 P95）
	JitterMs float64 // 抖动：去尖峰后排序相邻样本差的平均绝对值
	LossPct  float64 // 丢包率 = 失败次数 / 总采样次数 × 100
	Sent     int     // 总采样次数
	Received int     // 成功采样次数

	// 稳健去尖峰辅助指标
	RawAvgMs   float64 // 未去尖峰的原始平均 RTT
	MedianMs   float64 // 中位数 RTT（抗尖峰）
	SpikePct   float64 // 尖峰率（0~100）
	SpikeCount int     // 尖峰样本数
}

// RobustParams 稳健去尖峰参数（由配置构建，供 prober 计算统计）
type RobustParams struct {
	Enabled bool    // 是否启用
	MadK    float64 // 尖峰阈值系数：中位数 + k×MAD
	FloorMs float64 // 阈值下限（毫秒）
	TrimPct float64 // 去尖峰后对称裁掉最高/最低百分比
}

// MCResult Phase2 Minecraft SLP 深度验证结果（§7.6）
type MCResult struct {
	OK          bool    // 是否至少成功一次
	SuccessRate float64 // 成功率（0~1）
	AvgRTTMs    float64 // 成功样本平均应用层 RTT
	MaxRTTMs    float64 // 成功样本最大应用层 RTT
	P95RTTMs    float64 // 成功样本 P95 应用层 RTT（统一端到端口径）
	JitterMs    float64 // 成功样本相邻抖动（按时间顺序，RFC3550 风格）
	Samples     int     // 采样总次数
	Successes   int     // 成功次数

	// 带负载突发（连续小包快速 ping，测繁忙时的表现）
	BurstSamples  int     // 突发采样次数
	BurstAvgRTTMs float64 // 突发平均 RTT
	BurstP95RTTMs float64 // 突发 P95 RTT
	BurstMaxRTTMs float64 // 突发最大 RTT
	BurstSpikePct float64 // 突发尖峰率（相对空闲均值明显突增的比例）
	BurstLossPct  float64 // 突发丢包率

	VersionName   string // 服务端版本名（如 "1.20.4"）
	Protocol      int    // 协议版本号
	PlayersOnline int    // 在线人数
	PlayersMax    int    // 最大人数
	ErrMsg        string // 全部失败时的中文原因（一票否决用）
}

// ScoreResult 单节点综合评分结果（§7.2）
type ScoreResult struct {
	NodeName   string             // 节点名
	Total      float64            // 加权总分 × 节点权重
	Parts      map[string]float64 // 各分项分数：latency/p95/jitter/loss/mc/history
	Reason     string             // 一句话中文原因（供日志与状态屏展示“为什么选它”）
	Eligible   bool               // 是否可参与选择（Phase1 全失败或 MC 一票否决时为 false）
	VetoedByMC bool               // 是否被 MC 一票否决
}

// NodeRuntime 单个节点的运行时状态（由 prober/monitor 维护，ui 读取展示）
type NodeRuntime struct {
	Name           string       // 节点名称（唯一）
	Status         NodeStatus   // 当前状态
	StatusReason   string       // 当前状态的中文原因（如最后错误信息）
	Stats          *ProbeStats  // Phase1 TCP 统计（未测速时为 nil）
	MC             *MCResult    // Phase2 MC 验证结果（未验证时为 nil）
	Score          *ScoreResult // 综合评分（未评分时为 nil）
	LastProbedAt   time.Time    // 最近一次全量测速完成时间
	BlacklistUntil time.Time    // 黑名单截止时间（零值 = 不在黑名单）
	FailRounds     int          // 连续整轮失败次数
}

// Blacklisted 判断节点当前是否处于黑名单期内
func (n *NodeRuntime) Blacklisted() bool {
	return !n.BlacklistUntil.IsZero() && time.Now().Before(n.BlacklistUntil)
}

// BlacklistRemain 返回黑名单剩余时长（不在黑名单时返回 0）
func (n *NodeRuntime) BlacklistRemain() time.Duration {
	if !n.Blacklisted() {
		return 0
	}
	return time.Until(n.BlacklistUntil)
}
