// Package switcher 实现双通道四闸门切换决策（F7 / §7.4）—— 防横跳的核心。
//
//	通道 A（故障切换，高优先级）：当前节点连续 fail_threshold 次监控失败，
//	  或 frpc 滑动窗口内重启超 max_restarts 次 → 立即全量重测并切换，
//	  不受任何冷却/驻留/上限限制。例外：它是唯一可用节点 → 只退避重启 frpc，不切换。
//	通道 B（更优切换）：四道闸门全部通过才切换：
//	  ① 距上次切换 ≥ min_dwell_min（最小驻留）
//	  ② 不在 cooldown_min 冷却期内
//	  ③ 候选分 > 当前分 + advantage_margin 且连续 consecutive_wins 轮均满足
//	  ④ 本小时切换总数 < max_switches_per_hour
//
// 并发安全（F7.4）：监控协程与重测协程都会调用，全部方法加锁。
// 每次评估必附中文原因并写日志（F7.3）。
package switcher

import (
	"fmt"
	"sync"
	"time"

	"mcnode/internal/config"
	"mcnode/internal/logger"
	"mcnode/internal/model"
)

// Decision 一次切换决策的结果
type Decision struct {
	Switch  bool   // 是否切换
	Channel string // "A"=故障切换 / "B"=更优切换 / ""=不切换
	Reason  string // 中文原因（无论切与不切都必须给出）
}

// Switcher 切换决策器（并发安全）
type Switcher struct {
	mu  sync.Mutex
	cfg func() *config.Config // 取最新配置（热加载后阈值即时生效）

	lastSwitch      time.Time   // 上次切换时间（驻留起点）
	cooldownUntil   time.Time   // 冷却截止时间
	consecutiveWins int         // 候选连续优势轮数
	switchLog       []time.Time // 切换时间滑动记录（1 小时窗口）
	failCount       int         // 当前节点连续监控失败计数
	totalSwitches   int         // 进程生命周期内切换总数（统计展示用）
}

// New 创建切换决策器
func New(cfgFn func() *config.Config) *Switcher {
	return &Switcher{cfg: cfgFn}
}

// switchesInHour 统计最近 1 小时内的切换次数（调用方须持锁）
func (s *Switcher) switchesInHour(now time.Time) int {
	cut := now.Add(-time.Hour)
	n := 0
	for _, t := range s.switchLog {
		if t.After(cut) {
			n++
		}
	}
	return n
}

// RecordMonitorFailure 登记一次当前节点监控失败；返回是否已达故障阈值（通道 A 触发条件）。
func (s *Switcher) RecordMonitorFailure(reason string) (dead bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failCount++
	threshold := s.cfg().Switching.FailThreshold
	logger.Debugf("当前节点监控失败 %d/%d 次：%s", s.failCount, threshold, reason)
	return s.failCount >= threshold
}

// ResetMonitorFailures 监控探测成功后清零失败计数
func (s *Switcher) ResetMonitorFailures() {
	s.mu.Lock()
	s.failCount = 0
	s.mu.Unlock()
}

// FrpcRestartLimited frpc 重启限流判定：滑动窗口内重启数 ≥ max_restarts → 触发通道 A。
// 由 frpc 管理器在每次重启后调用。
func (s *Switcher) FrpcRestartLimited(restartsInWindow, maxRestarts int) bool {
	return restartsInWindow >= maxRestarts
}

// EvaluateChannelA 通道 A 决策：当前节点已判死时的处理。
// onlyNode=true 表示它是唯一可用节点 → 不切换，只退避重启 frpc（F7.1 例外条款）。
func (s *Switcher) EvaluateChannelA(curName string, onlyNode bool) Decision {
	if onlyNode {
		return Decision{
			Switch:  false,
			Channel: "A",
			Reason:  fmt.Sprintf("通道A：当前节点 %s 已判死，但它是唯一可用节点，只退避重启 frpc 不切换", curName),
		}
	}
	return Decision{
		Switch:  true,
		Channel: "A",
		Reason:  fmt.Sprintf("通道A（故障切换）：当前节点 %s 连续失败达阈值，立即全量重测并切换（不受冷却/驻留限制）", curName),
	}
}

// Evaluate 通道 B 决策：更优切换四道闸门（§7.4 伪代码逐行对应）。
// cur/cand 为当前节点与最优候选（cand 可为 nil = 无候选）。
func (s *Switcher) Evaluate(cur, cand *model.NodeRuntime) Decision {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	cfg := s.cfg()
	sw := cfg.Switching

	if cand == nil || cand.Score == nil || !cand.Score.Eligible {
		s.consecutiveWins = 0 // 无有效候选，连胜清零
		return Decision{Reason: "无更优候选节点（其余节点不可用或分数无效），不切换"}
	}
	curScore := 0.0
	curName := "（无）"
	if cur != nil && cur.Score != nil {
		curScore = cur.Score.Total
		curName = cur.Name
	}

	// 闸门③a：优势不足 → 连胜清零，不切
	if cand.Score.Total <= curScore+sw.AdvantageMargin {
		s.consecutiveWins = 0
		return Decision{Reason: fmt.Sprintf(
			"候选 %s 分数 %.1f 未显著高于当前 %s（%.1f），优势需 ≥ %.1f 分（滞回防横跳），连胜清零，不切换",
			cand.Name, cand.Score.Total, curName, curScore, sw.AdvantageMargin)}
	}

	// 闸门①：最小驻留
	dwell := time.Duration(sw.MinDwellMin) * time.Minute
	if !s.lastSwitch.IsZero() && now.Sub(s.lastSwitch) < dwell {
		return Decision{Reason: fmt.Sprintf(
			"候选 %s 分数占优，但距上次切换仅 %s < 最小驻留 %s（闸门①），不切换",
			cand.Name, fmtDur(now.Sub(s.lastSwitch)), fmtDur(dwell))}
	}

	// 闸门②：冷却期
	if now.Before(s.cooldownUntil) {
		return Decision{Reason: fmt.Sprintf(
			"候选 %s 分数占优，但处于切换冷却期（剩余 %s，闸门②），不切换",
			cand.Name, fmtDur(time.Until(s.cooldownUntil)))}
	}

	// 闸门③b：连续优势轮数
	s.consecutiveWins++
	if s.consecutiveWins < sw.ConsecutiveWins {
		return Decision{Reason: fmt.Sprintf(
			"候选 %s 连续优势第 %d/%d 轮（闸门③），未达连续 %d 轮，不切换",
			cand.Name, s.consecutiveWins, sw.ConsecutiveWins, sw.ConsecutiveWins)}
	}

	// 闸门④：每小时切换上限（保险丝）
	if s.switchesInHour(now) >= sw.MaxSwitchesPerHour {
		return Decision{Reason: fmt.Sprintf(
			"候选 %s 分数占优且连胜达标，但本小时已切换 %d 次 ≥ 上限 %d（闸门④保险丝），不切换",
			cand.Name, s.switchesInHour(now), sw.MaxSwitchesPerHour)}
	}

	return Decision{
		Switch:  true,
		Channel: "B",
		Reason: fmt.Sprintf(
			"通道B（更优切换）：候选 %s 分数 %.1f 显著优于当前 %s（%.1f）且连续 %d 轮领先，四道闸门全部通过",
			cand.Name, cand.Score.Total, curName, curScore, sw.ConsecutiveWins),
	}
}

// RegisterSwitch 切换执行成功后登记：刷新驻留起点、冷却截止、滑动窗口计数（F7.2）
func (s *Switcher) RegisterSwitch() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	cfg := s.cfg()
	s.lastSwitch = now
	s.cooldownUntil = now.Add(time.Duration(cfg.Switching.CooldownMin) * time.Minute)
	s.switchLog = append(s.switchLog, now)
	s.totalSwitches++
	s.consecutiveWins = 0
	s.failCount = 0
	// 清理超出 1 小时的滑动窗口记录，防止无限增长
	cut := now.Add(-time.Hour)
	kept := s.switchLog[:0]
	for _, t := range s.switchLog {
		if t.After(cut) {
			kept = append(kept, t)
		}
	}
	s.switchLog = kept
}

// SwitchesLastHour 最近 1 小时切换次数（状态屏展示）
func (s *Switcher) SwitchesLastHour() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.switchesInHour(time.Now())
}

// TotalSwitches 进程生命周期内总切换次数（状态屏展示）
func (s *Switcher) TotalSwitches() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.totalSwitches
}

// CooldownRemain 冷却剩余时长（状态屏展示）
func (s *Switcher) CooldownRemain() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if time.Now().Before(s.cooldownUntil) {
		return time.Until(s.cooldownUntil)
	}
	return 0
}

// FailCount 当前连续监控失败计数（状态屏展示）
func (s *Switcher) FailCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failCount
}

// fmtDur 时长格式化（m 分 s 秒）
func fmtDur(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
}
