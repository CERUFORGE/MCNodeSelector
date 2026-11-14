// Package prober 实现两阶段并发测速（F2/F3/F6 / §4.2）。
//
//	Phase 1（全量 TCP 筛查）：对每个启用节点探测 probe.host:probe.port（默认 frps bindPort），
//	  采样 N 次取统计指标 —— 此探测不依赖隧道，可对全部节点并发执行；
//	Phase 2（深度 MC 验证）：仅取 Phase 1 得分前 K 名候选，对其
//	  mc_check.host:mc_check.port（默认 server_addr:remote_port）做 MC SLP ping。
//
// 并发模型（§4.3）：节点之间并发（信号量限 probing.concurrency）；
// 节点内部串行采样 N 次、固定间隔 interval_ms（jitter 依赖相邻采样节奏）。
// 单点故障隔离（F2.4）：每个节点的测速逻辑包裹 recover，panic 只标记该节点
// INTERNAL_ERROR，绝不影响其他节点与主程序。
package prober

import (
	"context"
	"fmt"
	"net"
	"sort"
	"sync"
	"time"

	"mcnode/internal/config"
	"mcnode/internal/dnscache"
	"mcnode/internal/history"
	"mcnode/internal/logger"
	"mcnode/internal/mcping"
	"mcnode/internal/model"
	"mcnode/internal/neterr"
	"mcnode/internal/score"
)

// Prober 测速器
type Prober struct {
	cfg   *config.Config // 配置快照（一轮测速内固定，热加载由 monitor 重新创建 Prober）
	hist  *history.Store
	nodes []config.Node // 实际参与测速的节点（远程+本地合并后）
}

// New 创建测速器。nodes 为合并后的生效节点列表（远程+本地），
// 为 nil 时回退用 cfg.Nodes（兼容旧调用）。
func New(cfg *config.Config, hist *history.Store, nodes []config.Node) *Prober {
	if nodes == nil {
		nodes = cfg.Nodes
	}
	return &Prober{cfg: cfg, hist: hist, nodes: nodes}
}

// RunAll 执行一轮完整的两阶段测速，返回全部节点的最新运行时状态。
// prev 为上一轮状态（用于延续黑名单与展示旧数据），可为 nil。
func (p *Prober) RunAll(ctx context.Context, prev map[string]*model.NodeRuntime) map[string]*model.NodeRuntime {
	out := make(map[string]*model.NodeRuntime, len(p.nodes))
	var toProbe []*config.Node // 本轮实际参与测速的节点

	for i := range p.nodes {
		n := &p.nodes[i]
		rt := &model.NodeRuntime{Name: n.Name, LastProbedAt: time.Now()}
		if prev != nil {
			if old, ok := prev[n.Name]; ok {
				// 延续上一轮的黑名单与统计数据（黑名单期内跳过测速）
				rt.BlacklistUntil = old.BlacklistUntil
				rt.Stats, rt.MC, rt.Score = old.Stats, old.MC, old.Score
				rt.LastProbedAt = old.LastProbedAt
				rt.FailRounds = old.FailRounds
			}
		}
		if !n.IsEnabled() {
			rt.Status = model.StatusDisabled
			rt.StatusReason = "配置中 enabled=false，已停用"
			out[n.Name] = rt
			continue
		}
		// 黑名单期内：跳过测速与选择，仅展示（F6/§7.5）
		if rt.Blacklisted() {
			rt.Status = model.StatusBlacklisted
			rt.StatusReason = fmt.Sprintf("黑名单期内（剩余 %s），跳过本轮测速", formatDur(rt.BlacklistRemain()))
			out[n.Name] = rt
			continue
		}
		rt.Status = model.StatusTesting
		out[n.Name] = rt
		toProbe = append(toProbe, n)
	}

	// 兜底：所有启用节点都在黑名单 → 清空黑名单强制全量重测（F6，保证永不放弃）
	if len(toProbe) == 0 {
		logger.Warnf("全部启用节点都在黑名单中，自动清空黑名单强制全量重测（兜底，永不放弃）")
		for i := range p.nodes {
			n := &p.nodes[i]
			if !n.IsEnabled() {
				continue
			}
			rt := out[n.Name]
			rt.BlacklistUntil = time.Time{}
			rt.Status = model.StatusTesting
			rt.StatusReason = ""
			toProbe = append(toProbe, n)
		}
	}
	if len(toProbe) == 0 {
		return out // 无启用节点（极端配置），仅返回 DISABLED 状态
	}

	// ---------- Phase 1：全量 TCP 筛查（并发，信号量限流） ----------
	sem := make(chan struct{}, p.cfg.Probing.Concurrency)
	var wg sync.WaitGroup
	var mu sync.Mutex // 保护 out 与 toProbe 排名用切片
	phase1Rank := make([]*config.Node, 0, len(toProbe))

	for _, n := range toProbe {
		wg.Add(1)
		go func(n *config.Node) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			rt := p.probeNodePhase1(ctx, n) // 内部已 recover 隔离
			mu.Lock()
			out[n.Name] = rt
			mu.Unlock()
		}(n)
	}
	wg.Wait()
	if ctx.Err() != nil {
		return out // 程序退出，及时中止
	}

	// ---------- 挑选 Phase 2 候选（Phase 1 得分前 K 名） ----------
	mcCfg := p.cfg.Probing.MCCheck
	if mcCfg.Mode == "tunnel" {
		// tunnel 模式为预留扩展点：本期未实现，降级为 direct 并提示（F3/§4.2）
		logger.Warnf("mc_check.mode=tunnel 为预留功能，本期未实现，按 direct 模式处理")
	}
	if mcCfg.IsEnabled() {
		for _, n := range toProbe {
			rt := out[n.Name]
			if rt != nil && (rt.Status == model.StatusOnline || rt.Status == model.StatusUnstable) {
				phase1Rank = append(phase1Rank, n)
			}
		}
		// 按 Phase1 部分分降序（稳定排序，同分保持配置顺序）
		sort.SliceStable(phase1Rank, func(a, b int) bool {
			return p.phase1ScoreOf(phase1Rank[a], out) > p.phase1ScoreOf(phase1Rank[b], out)
		})
		k := mcCfg.TopK
		if k > len(phase1Rank) {
			k = len(phase1Rank)
		}
		candidates := phase1Rank[:k]
		if len(candidates) > 0 {
			logger.Infof("Phase2 深度验证：%d 个候选（%s）", len(candidates), namesOf(candidates))
			// ---------- Phase 2：MC 深度验证（同样并发限流 + 节点内串行） ----------
			var wg2 sync.WaitGroup
			for _, n := range candidates {
				wg2.Add(1)
				go func(n *config.Node) {
					defer wg2.Done()
					sem <- struct{}{}
					defer func() { <-sem }()
					mc := p.probeNodePhase2(ctx, n) // 内部已 recover 隔离
					mu.Lock()
					if rt, ok := out[n.Name]; ok {
						rt.MC = mc
					}
					mu.Unlock()
				}(n)
			}
			wg2.Wait()
		}
	}

	// ---------- 最终评分 + 黑名单登记 ----------
	// 先判定是否有任何节点 MC 端到端验证成功：若全部失败，多半是服务器休眠/未启动，
	// 此时不能用 MC 一票否决（否则会把所有节点误判为不可用）。
	anyMCOK := false
	for i := range p.nodes {
		rt := out[p.nodes[i].Name]
		if rt != nil && rt.MC != nil && rt.MC.OK {
			anyMCOK = true
			break
		}
	}
	if !anyMCOK {
		for i := range p.nodes {
			if rt := out[p.nodes[i].Name]; rt != nil {
				rt.MC = nil // 视为未验证 → 评分仅用 TCP 指标，不做否决
			}
		}
	}

	blacklistDur := time.Duration(p.cfg.Switching.BlacklistMin) * time.Minute
	for i := range p.nodes {
		n := &p.nodes[i]
		rt, ok := out[n.Name]
		if !ok || rt.Stats == nil || rt.Stats.Received <= 0 {
			continue // 未测速 / 全失败节点不做最终评分
		}
		histScore := p.hist.Get(n.Name)
		rt.Score = score.Compute(n, rt.Stats, rt.MC, histScore, p.cfg)
		// MC 一票否决 → 入黑名单（§7.5）
		if rt.Score != nil && rt.Score.VetoedByMC {
			rt.BlacklistUntil = time.Now().Add(blacklistDur)
			rt.StatusReason = rt.Score.Reason
		}
	}
	// 全失败节点入黑名单（OFFLINE/TIMEOUT/INTERNAL_ERROR，§7.5）
	for i := range p.nodes {
		n := &p.nodes[i]
		rt, ok := out[n.Name]
		if !ok {
			continue
		}
		if rt.Status == model.StatusOffline || rt.Status == model.StatusTimeout || rt.Status == model.StatusInternalErr {
			if !rt.Blacklisted() {
				rt.BlacklistUntil = time.Now().Add(blacklistDur)
			}
		}
	}
	return out
}

// probeNodePhase1 单节点 Phase1：串行采样 N 次、固定间隔（F2.1 / §4.3）。
// recover 隔离：panic → INTERNAL_ERROR + 中文消息，不影响其他节点（F2.4）。
func (p *Prober) probeNodePhase1(ctx context.Context, n *config.Node) (rt *model.NodeRuntime) {
	rt = &model.NodeRuntime{Name: n.Name, Status: model.StatusTesting}
	defer func() {
		if r := recover(); r != nil {
			// 单点故障隔离：该节点标记 INTERNAL_ERROR，中文消息，其余节点与主流程不受影响
			rt.Status = model.StatusInternalErr
			rt.StatusReason = fmt.Sprintf("测速内部错误（已隔离）：%v", r)
			rt.Stats = &model.ProbeStats{Sent: p.cfg.Probing.Samples, Received: 0, LossPct: 100}
			logger.Errorf("节点 %s 测速内部错误（已隔离，不影响其他节点）：%v", n.Name, r)
		}
		rt.LastProbedAt = time.Now()
	}()

	host, port := n.ProbeAddr()
	timeout := time.Duration(p.cfg.Probing.TimeoutMs) * time.Millisecond
	interval := time.Duration(p.cfg.Probing.IntervalMs) * time.Millisecond
	addr := net.JoinHostPort(host, fmt.Sprintf("%d", port))

	rtts := make([]float64, 0, p.cfg.Probing.Samples)
	fails := 0
	lastClass, lastMsg := "", ""
	for i := 0; i < p.cfg.Probing.Samples; i++ {
		// 支持 context 取消：程序退出时能及时中止测速（F2.5）
		select {
		case <-ctx.Done():
			rt.Status = model.StatusTesting
			rt.StatusReason = "测速被取消（程序退出）"
			return rt
		default:
		}
		start := time.Now()
		conn, err := dnscache.DialContext(ctx, "tcp", addr, timeout)
		if err != nil {
			fails++
			lastClass, lastMsg = neterr.Classify(err)
			// 打了日志就脱敏；DNS/拒绝/超时按频度提示，避免刷屏
			if fails == 1 {
				logger.Debugf("节点 %s Phase1 采样失败：%s", n.Name, lastMsg)
			}
		} else {
			// TCP 连接建立耗时即样本 RTT（只连不发数据，F2.1）
			rtts = append(rtts, float64(time.Since(start).Microseconds())/1000.0)
			_ = conn.Close() // 立即关闭，不发任何数据
		}
		// 节点内固定间隔串行采样
		if i < p.cfg.Probing.Samples-1 {
			select {
			case <-ctx.Done():
			case <-time.After(interval):
			}
		}
	}

	rt.Stats = ComputeStats(rtts, fails, p.robustParams())
	// ---- 状态机判定（F6） ----
	switch {
	case rt.Stats.Received == 0:
		// 全部失败：拒绝 → OFFLINE；超时/DNS/其他 → TIMEOUT
		if lastClass == neterr.ClassRefused {
			rt.Status = model.StatusOffline
		} else {
			rt.Status = model.StatusTimeout
		}
		rt.StatusReason = lastMsg
		rt.FailRounds++
		logger.Infof("节点 %s Phase1 全部失败（%d 次）：%s", n.Name, fails, lastMsg)
	case rt.Stats.LossPct >= p.cfg.Scoring.UnstableLossPct:
		rt.Status = model.StatusUnstable
		rt.StatusReason = fmt.Sprintf("丢包率 %.0f%% ≥ 阈值 %.0f%%，状态不稳定", rt.Stats.LossPct, p.cfg.Scoring.UnstableLossPct)
		rt.FailRounds = 0
	default:
		rt.Status = model.StatusOnline
		rt.StatusReason = ""
		rt.FailRounds = 0
	}
	return rt
}

// probeNodePhase2 单节点 Phase2：MC SLP ping 采样（F3.1）。
// 同样 recover 隔离（单点故障不得影响全局，§2.3）。
func (p *Prober) probeNodePhase2(ctx context.Context, n *config.Node) (mc *model.MCResult) {
	mc = &model.MCResult{}
	defer func() {
		if r := recover(); r != nil {
			mc.OK = false
			mc.ErrMsg = fmt.Sprintf("MC 验证内部错误（已隔离）：%v", r)
			logger.Errorf("节点 %s MC 验证内部错误（已隔离）：%v", n.Name, r)
		}
	}()

	host, port := n.MCAddr()
	ip := dnscache.First(host) // 解析一次并缓存，避免每次 ping 都走 DNS
	timeout := time.Duration(p.cfg.Probing.MCCheck.TimeoutMs) * time.Millisecond
	total := p.cfg.Probing.MCCheck.Samples
	if total <= 0 {
		total = 1
	}
	successes := 0
	sumRTT := 0.0
	maxRTT := 0.0
	rtts := make([]float64, 0, total)
	var lastResult *mcping.Result
	for i := 0; i < total; i++ {
		select {
		case <-ctx.Done():
			mc.ErrMsg = "已取消"
			return mc
		default:
		}
		res, err := mcping.Ping(ip, port, timeout)
		if err != nil {
			// mcping 协议错误已是中文；网络层错误经 neterr 翻译（F11）
			mc.ErrMsg = errFriendly(err)
			logger.Debugf("节点 %s MC ping 失败：%v", n.Name, err)
		} else if res != nil {
			successes++
			sumRTT += res.RTTMs
			rtts = append(rtts, res.RTTMs)
			if res.RTTMs > maxRTT {
				maxRTT = res.RTTMs
			}
			lastResult = res
		}
		if i < total-1 {
			select {
			case <-ctx.Done():
			case <-time.After(time.Duration(p.cfg.Probing.IntervalMs) * time.Millisecond):
			}
		}
	}
	mc.Samples = total
	mc.Successes = successes
	mc.SuccessRate = float64(successes) / float64(total)
	if successes > 0 {
		mc.OK = true
		mc.AvgRTTMs = sumRTT / float64(successes)
		mc.MaxRTTMs = maxRTT
		sorted := append([]float64(nil), rtts...)
		sort.Float64s(sorted)
		mc.P95RTTMs = percentileOf(sorted, 0.95)
		mc.JitterMs = orderedJitter(rtts)
		if lastResult != nil {
			mc.VersionName = lastResult.VersionName
			mc.Protocol = lastResult.Protocol
			mc.PlayersOnline = lastResult.PlayersOnline
			mc.PlayersMax = lastResult.PlayersMax
		}
	}
	// 带负载突发：连续快速 ping（不间隔）测繁忙时的 P95/尖峰，更接近游戏实际手感。
	if mc.OK {
		if burst := p.cfg.Probing.MCCheck.Burst; burst > 0 {
			brtts := make([]float64, 0, burst)
			bfail := 0
			for i := 0; i < burst; i++ {
				select {
				case <-ctx.Done():
					return mc
				default:
				}
				res, err := mcping.Ping(ip, port, timeout)
				if err != nil {
					bfail++
				} else if res != nil {
					brtts = append(brtts, res.RTTMs)
				}
			}
			mc.BurstSamples = burst
			mc.BurstLossPct = float64(bfail) / float64(burst) * 100
			if len(brtts) > 0 {
				bs := append([]float64(nil), brtts...)
				sort.Float64s(bs)
				mc.BurstAvgRTTMs = meanOf(brtts)
				mc.BurstP95RTTMs = percentileOf(bs, 0.95)
				mc.BurstMaxRTTMs = bs[len(bs)-1]
				thr := mc.AvgRTTMs + 120 // 比空闲均值明显突增即算尖峰
				if thr < 150 {
					thr = 150
				}
				sp := 0
				for _, v := range brtts {
					if v > thr {
						sp++
					}
				}
				mc.BurstSpikePct = float64(sp) / float64(len(brtts)) * 100
			}
		}
	}
	return mc
}

// errFriendly 把 MC ping 错误转成中文（优先用 mcping 的协议错误文案）
func errFriendly(err error) string {
	if err == nil {
		return ""
	}
	// mcping 定义的协议错误直接返回（已是中文）
	switch err.Error() {
	case mcping.ErrMCTimeout.Error(), mcping.ErrMCBadPacket.Error(), mcping.ErrAddrTooLong.Error():
		return err.Error()
	}
	_, msg := neterr.Classify(err)
	if msg == "" {
		return err.Error()
	}
	return msg
}

// robustParams 从配置构建稳健去尖峰参数
func (p *Prober) robustParams() model.RobustParams {
	r := p.cfg.Scoring.Robust
	return model.RobustParams{
		Enabled: r.IsEnabled(),
		MadK:    r.SpikeMadK,
		FloorMs: r.SpikeFloor,
		TrimPct: r.TrimPct,
	}
}

// phase1ScoreOf 计算 Phase1 排名分（不含 MC 分项，§4.2）
func (p *Prober) phase1ScoreOf(n *config.Node, all map[string]*model.NodeRuntime) float64 {
	rt, ok := all[n.Name]
	if !ok || rt == nil || rt.Stats == nil || rt.Stats.Received <= 0 {
		return -1 // 不可排名
	}
	return score.Phase1Score(n, rt.Stats, p.hist.Get(n.Name), p.cfg)
}

// namesOf 候选名列表（日志用）
func namesOf(nodes []*config.Node) string {
	s := ""
	for i, n := range nodes {
		if i > 0 {
			s += ", "
		}
		s += n.Name
	}
	return s
}

// formatDur 时长格式化（中文，展示用）
func formatDur(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
}

// ---------- 轻量监控探测（F10.1 ①：当前节点单次探测） ----------

// ProbeOnce 对单个节点做一次轻量探测：useMC=true 用 MC ping（更真实），否则 TCP。
// 返回本次探测的应用层 RTT（成功时有效）与失败原因（nil 表示成功）。
func ProbeOnce(ctx context.Context, n *config.Node, useMC bool, timeout time.Duration) (time.Duration, error) {
	if useMC {
		host, port := n.MCAddr()
		ip := dnscache.First(host)
		res, err := mcping.Ping(ip, port, timeout)
		if err != nil {
			return 0, fmt.Errorf("%s", errFriendly(err))
		}
		if res != nil {
			return time.Duration(res.RTTMs * float64(time.Millisecond)), nil
		}
		return 0, nil
	}
	host, port := n.ProbeAddr()
	start := time.Now()
	conn, err := dnscache.DialContext(ctx, "tcp", net.JoinHostPort(host, fmt.Sprintf("%d", port)), timeout)
	if err != nil {
		_, msg := neterr.Classify(err)
		return 0, fmt.Errorf("%s", msg)
	}
	rtt := time.Since(start)
	_ = conn.Close()
	return rtt, nil
}

// ComputeStats 计算统计指标（稳健去尖峰版，§7.1）：
//
//	先按 MAD 判定尖峰：RTT > 中位数 + k×MAD（且有下限 floor_ms）视为尖峰；
//	去尖峰后再按 trim_pct 对称裁掉最高/最低，然后在“干净样本”上计算：
//	  Avg / P95 / Jitter / Min / Max —— 单次高延迟尖峰不会拖垮评分；
//	同时保留原始均值 RawAvgMs、中位数 MedianMs、尖峰率 SpikePct 供展示与排查。
//	loss = 失败次数 / 总采样次数。
func ComputeStats(rtts []float64, fails int, rp model.RobustParams) *model.ProbeStats {
	s := &model.ProbeStats{Sent: len(rtts) + fails, Received: len(rtts)}
	if s.Sent > 0 {
		s.LossPct = float64(fails) / float64(s.Sent) * 100
	}
	if s.Received == 0 {
		return s
	}
	sorted := make([]float64, len(rtts))
	copy(sorted, rtts)
	sort.Float64s(sorted)
	s.MinMs = sorted[0]
	s.MaxMs = sorted[len(sorted)-1]
	s.MedianMs = medianOf(sorted)
	s.RawAvgMs = meanOf(sorted)

	clean := sorted
	if rp.Enabled {
		med := s.MedianMs
		mad := madOf(sorted, med)
		thr := med + rp.MadK*mad
		if floor := med + rp.FloorMs; thr < floor {
			thr = floor
		}
		kept := make([]float64, 0, len(sorted))
		for _, v := range sorted {
			if v <= thr {
				kept = append(kept, v)
			}
		}
		s.SpikeCount = len(sorted) - len(kept)
		clean = trimSymmetric(kept, rp.TrimPct)
		if len(clean) == 0 {
			// 兜底：极端情况下全部被判尖峰，退回原始样本
			clean = sorted
			if s.SpikeCount > 0 {
				s.SpikeCount = len(sorted) - 1
			}
		}
		s.SpikePct = float64(s.SpikeCount) / float64(len(sorted)) * 100
	}
	s.AvgMs = meanOf(clean)
	s.P95Ms = percentileOf(clean, 0.95)
	// 抖动按「时间顺序」的相邻样本差计算（RFC3550 风格）——排序后相邻差测的是离散度，
	// 不是真实抖动，且会与 P95 高度共线。
	s.JitterMs = orderedJitter(rtts)
	return s
}

// meanOf 算术平均（空切片返回 0）
func meanOf(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	sum := 0.0
	for _, v := range xs {
		sum += v
	}
	return sum / float64(len(xs))
}

// medianOf 已排序切片的中位数
func medianOf(sorted []float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if n%2 == 1 {
		return sorted[n/2]
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2
}

// madOf 中位数绝对偏差（MAD），稳健的离散度度量
func madOf(sorted []float64, med float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	dev := make([]float64, len(sorted))
	for i, v := range sorted {
		d := v - med
		if d < 0 {
			d = -d
		}
		dev[i] = d
	}
	sort.Float64s(dev)
	return medianOf(dev)
}

// percentileOf 线性插值百分位（p ∈ [0,1]）
func percentileOf(sorted []float64, p float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if n == 1 {
		return sorted[0]
	}
	if p <= 0 {
		return sorted[0]
	}
	if p >= 1 {
		return sorted[n-1]
	}
	pos := p * float64(n-1)
	lo := int(pos)
	hi := lo + 1
	if hi >= n {
		return sorted[n-1]
	}
	frac := pos - float64(lo)
	return sorted[lo] + (sorted[hi]-sorted[lo])*frac
}

// trimSymmetric 对称裁掉最高/最低 pct%（pct=0 原样返回）
func trimSymmetric(sorted []float64, pct float64) []float64 {
	n := len(sorted)
	if n == 0 || pct <= 0 {
		return sorted
	}
	k := int(float64(n) * pct / 100.0)
	if 2*k >= n {
		k = (n - 1) / 2
	}
	if k <= 0 {
		return sorted
	}
	return sorted[k : n-k]
}

// orderedJitter 按时间顺序的相邻样本平均绝对差（RFC3550 风格，真实抖动）
func orderedJitter(rtts []float64) float64 {
	if len(rtts) < 2 {
		return 0
	}
	sum := 0.0
	for i := 1; i < len(rtts); i++ {
		sum += abs(rtts[i] - rtts[i-1])
	}
	return sum / float64(len(rtts)-1)
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
