// Package monitor 是后台主循环（F10）：四职责由 select 驱动、互不阻塞：
//
//	①每 monitor_interval_sec 轻量探测当前节点一次（MC ping 或 TCP）；
//	②每 retest_interval_min 全量重测 → 更新历史 → Switcher 决策；
//	③每 1s 轮询配置 mtime 热加载；
//	④Ctrl-C/信号 → 停 frpc → 保存 state → 退出（不留孤儿 frpc 进程）。
//
// 另含：通道 A/B 切换执行、全部节点不可用兜底循环（F8）、钩子执行（§8.3）、
// 状态屏数据快照（§15）、state.json 持久化（§10）。
package monitor

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"mcnode/internal/config"
	"mcnode/internal/forwarder"
	"mcnode/internal/frpc"
	"mcnode/internal/history"
	"mcnode/internal/i18n"
	"mcnode/internal/logger"
	"mcnode/internal/model"
	"mcnode/internal/mp"
	"mcnode/internal/prober"
	"mcnode/internal/remotenodes"
	"mcnode/internal/switcher"
	"mcnode/internal/ui"
)

// Notifier 桌面通知接口（由 tray 实现；为 nil 时忽略通知）
type Notifier interface {
	Notify(title, body string)
}

// Monitor 主循环协调器
type Monitor struct {
	cfgPath string

	mu       sync.RWMutex
	cfg      *config.Config
	nodes    map[string]*model.NodeRuntime // 最新一轮测速结果
	current  *config.Node                  // 当前托管节点
	mode     string                        // "自动" / "手动"
	notifier Notifier                      // 桌面通知（可空）

	hist *history.Store
	sw   *switcher.Switcher
	fm   *frpc.Manager
	fwd  *forwarder.Forwarder // 本地 TCP 转发器（玩家连 127.0.0.1:local_port → 自动转发到当前最优节点隧道）
	ui   *ui.UI
	rm   *remotenodes.Manager // 远程节点列表（EdgeOne 等，拉取失败回退本地）

	startTime  time.Time
	lastCfgMod time.Time
	savedNode  string // state.json 恢复的节点名
	cancel     context.CancelFunc
	ctx        context.Context // 主 ctx（热加载等后台任务复用）

	// 当前节点实时探测延迟（monitorTick 每轮记录，状态屏展示）
	rttWindow     []time.Duration // 最近若干次成功探测 RTT（取中位数展示，抗抖）
	spikeTimes    []time.Time     // 最近 1 小时内的尖峰时刻（端到端 RTT 超阈值）
	lastRTTAt     time.Time
	lastRTTOK     bool
	degradeTicks  int     // 当前节点连续高延迟计数（质量劣化自动切换用）
	degradeStreak int     // 端到端质量连续劣化次数（自适应多线滞回用）
	jitterEWMA    float64 // 端到端抖动 EWMA（ms，用于自适应劣化判定）
	lossEWMA      float64 // 端到端丢包 EWMA（%，用于自适应劣化判定）
	rttEWMA       float64 // 端到端 RTT EWMA（ms，用于计算抖动的基准）

	// 自适应多线状态
	mpPaths       []string // 候选多路路径（按总分排序，端点）
	mpPathNames   []string // 候选多路路径对应的节点名
	mpEngaged     bool     // 自适应是否已因网络劣化启用多线
	mpManual      int      // 手动模式：0=自适应 1=强制多线 2=强制单线
	mpStableSince time.Time

	// 用户交互自选的线路（按优先级），优先于配置 paths 与自动挑选；
	// 数字键选当前节点写入 [0]，[b] 后数字键选备用节点写入 [1]。
	mpPinned        []string
	selectingBackup bool   // 是否处于「选择备用节点」模式（按 [b] 进入）
	mpRedundancy    int    // 自适应进入多线时选定的路数（0=用配置 redundancy）
	mpReason        string // 自适应路数判定原因（状态屏展示）

	mpPathQ    map[string]mpPathQuality // 多线路径实时质量（端点 → 质量）
	pathDownAt map[string]time.Time     // 路径断开去重时间戳

	manualInitial string // --node 指定的初始节点名
	watch         bool

	// 可重置的定时器（热加载改变间隔后重建）
	monitorT *time.Ticker
	retestT  *time.Ticker
}

// New 创建主循环
func New(cfgPath string, cfg *config.Config, manualNode string, watch bool) *Monitor {
	m := &Monitor{
		cfgPath:       cfgPath,
		cfg:           cfg,
		nodes:         make(map[string]*model.NodeRuntime),
		mode:          "自动",
		startTime:     time.Now(),
		manualInitial: manualNode,
		watch:         watch,
	}
	m.sw = switcher.New(m.Cfg)
	m.fm = frpc.New(m.Cfg)
	m.hist = history.New(cfg.HistoryPath(), cfg.Scoring.History.UpdateAlpha, cfg.Scoring.History.NeutralScore)
	m.rm = remotenodes.New(m.Cfg)
	listen := fmt.Sprintf("%s:%d", cfg.Minecraft.LocalHost, cfg.Minecraft.LocalPort)
	m.fwd = forwarder.New(listen)
	var extras []string
	for _, p := range cfg.Minecraft.ExtraLocalPorts {
		if p > 0 && p != cfg.Minecraft.LocalPort {
			extras = append(extras, fmt.Sprintf("%s:%d", cfg.Minecraft.LocalHost, p))
		}
	}
	m.fwd.SetExtraListens(extras)
	return m
}

// AttachUI 绑定界面（main 在创建 ui 后调用）
func (m *Monitor) AttachUI(u *ui.UI) { m.ui = u }

// SetNotifier 绑定桌面通知（main 在创建托盘后调用；可空）
func (m *Monitor) SetNotifier(n Notifier) { m.notifier = n }

// notify 发送桌面通知（未绑定或失败时静默忽略）
func (m *Monitor) notify(title, body string) {
	if m.notifier == nil {
		return
	}
	logger.Debugf("桌面通知：%s —— %s", title, body)
	m.notifier.Notify(title, body)
}

// Cfg 返回最新配置快照（供 switcher/frpc 热加载后即时取新参数）
func (m *Monitor) Cfg() *config.Config {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cfg
}

// ---------- 对外读取（状态屏 / --status 用） ----------

// CurrentNode 当前节点
func (m *Monitor) CurrentNode() *config.Node {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.current
}

// Nodes 测速结果快照
func (m *Monitor) Nodes() map[string]*model.NodeRuntime {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]*model.NodeRuntime, len(m.nodes))
	for k, v := range m.nodes {
		out[k] = v
	}
	return out
}

// Mode 当前模式
func (m *Monitor) Mode() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.mode
}

// ---------- 主循环 ----------

// Run 启动主循环，直到 ctx 取消（Ctrl-C 或 q 命令）
func (m *Monitor) Run(parent context.Context) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	m.cancel = cancel
	m.ctx = ctx
	m.startTime = time.Now()

	// 注册多线路由回调：路径质量回报 + 路径断开定向重测
	mp.OnPathQuality = m.onPathQuality
	mp.OnPathDown = m.onPathDown
	mp.OnPathSpike = m.onPathSpike

	// 启动自检（不阻断）：本地 MC 端口、remote_port 一致性
	m.checkLocalMC()
	m.checkRemotePortConsistency()
	m.registerSecrets()

	// 首次拉取远程节点列表（EdgeOne 等）；失败回退本地节点
	m.rm.Refresh()

	// 记录配置初始 mtime（热加载基准）
	if fi, err := os.Stat(m.cfgPath); err == nil {
		m.lastCfgMod = fi.ModTime()
	}
	m.loadState()

	// 启动本地 TCP 转发器（玩家连 127.0.0.1:local_port -> 自动转发到最优节点隧道）
	if err := m.fwd.Start(); err != nil {
		logger.Errorf("本地转发器启动失败：%v", err)
	} else {
		logger.Infof("本地转发器已启动：玩家连接 %s -> 自动转发到当前最优节点隧道", m.fwdListen())
	}
	defer m.fwd.Stop()
	m.applyMultipath()

	// watch 状态屏先出画面：首轮测速较慢（含超时节点可达数十秒），
	// 若不先渲染会长时间黑屏，用户会误以为程序没反应。
	if m.ui != nil {
		m.ui.StartWatchLoop()
	}

	// 首轮全量测速 + 初始节点选择 + frpc 拉起
	logger.Infof("开始首轮全量测速……")
	m.fullRetest(ctx)
	m.pickInitial(ctx)

	// 无可用节点：进入全灭兜底循环（frpc 仍托管退避，进程不退出，F8）
	if m.bestEligibleNode("") == nil {
		m.allDownLoop(ctx)
	}

	m.resetTickers()
	defer m.stopTickers()
	// ③ 配置 mtime 轮询定时器（1 秒粒度，F1.4）
	cfgT := time.NewTicker(1 * time.Second)
	defer cfgT.Stop()
	// ④ 远程节点列表定时拉取（EdgeOne 等，拉取失败回退本地）
	remoteT := time.NewTicker(time.Duration(m.Cfg().Remote.Interval) * time.Minute)
	defer remoteT.Stop()
	// ⑤ 每小时尖峰统计（用于验收 ≤N/小时）
	hourT := time.NewTicker(time.Hour)
	defer hourT.Stop()

	logger.Infof("进入后台监控：当前节点=%s 模式=%s（Ctrl-C 优雅退出）", m.currentName(), m.Mode())

	for {
		select {
		case <-ctx.Done():
			m.gracefulShutdown()
			return nil

		case <-m.monitorT.C:
			// ① 轻量探测当前节点 + frpc 健康
			m.monitorTick(ctx)

		case <-m.retestT.C:
			// ② 全量重测 → 历史更新 → 通道 B 决策
			m.retestTick(ctx)

		case <-cfgT.C:
			// ③ 配置热加载轮询
			m.reloadIfChanged()

		case <-remoteT.C:
			// ④ 远程节点列表定时刷新（EdgeOne 等）
			m.rm.Refresh()

		case <-hourT.C:
			// ⑤ 每小时尖峰统计
			hc := m.Cfg()
			logger.Infof("【尖峰统计】近 1 小时端到端尖峰 %d 次（阈值 %.0fms，目标 ≤%d/小时）",
				m.spikesLastHour(), hc.Switching.SpikeMs, hc.Switching.SpikeTargetPerHour)

		case ev := <-m.eventCh():
			m.onFrpcEvent(ctx, ev)

		case c := <-m.cmdCh():
			m.onCommand(ctx, c)
		}
	}
}

// eventCh frpc 事件通道（未托管 frpc 时返回 nil，select 分支永不触发）
func (m *Monitor) eventCh() <-chan frpc.Event {
	if !m.Cfg().Frpc.IsEnabled() {
		return nil
	}
	return m.fm.Events
}

// cmdCh 命令通道（无 ui 时返回 nil，select 分支永不触发）
func (m *Monitor) cmdCh() <-chan rune {
	if m.ui == nil {
		return nil
	}
	return m.ui.Commands()
}

// resetTickers 按最新配置创建定时器
func (m *Monitor) resetTickers() {
	m.stopTickers()
	cfg := m.Cfg()
	m.monitorT = time.NewTicker(time.Duration(cfg.Switching.MonitorIntervalSec) * time.Second)
	m.retestT = time.NewTicker(time.Duration(cfg.Switching.RetestIntervalMin) * time.Minute)
}

func (m *Monitor) stopTickers() {
	if m.monitorT != nil {
		m.monitorT.Stop()
	}
	if m.retestT != nil {
		m.retestT.Stop()
	}
}

// ---------- 周期任务 ----------

// monitorTick ① 轻量探测当前节点一次（F10.1/F10.2）
func (m *Monitor) monitorTick(ctx context.Context) {
	node := m.CurrentNode()
	if node == nil {
		return
	}
	cfg := m.Cfg()
	useMC := cfg.Switching.UseMCPing()
	timeout := time.Duration(cfg.Probing.TimeoutMs) * time.Millisecond
	if useMC {
		timeout = time.Duration(cfg.Probing.MCCheck.TimeoutMs) * time.Millisecond
	}
	rtt, err := prober.ProbeOnce(ctx, node, useMC, timeout)
	if err != nil {
		// 抗抖复核：偶发丢包/瞬断不算故障，最多再试 2 次（每次间隔 300ms）
		for i := 0; i < 2 && err != nil; i++ {
			select {
			case <-ctx.Done():
			case <-time.After(300 * time.Millisecond):
				if r2, e2 := prober.ProbeOnce(ctx, node, useMC, timeout); e2 == nil {
					rtt, err = r2, nil
				}
			}
		}
	}
	m.recordRTT(rtt, err == nil)
	m.updateQualityEWMA(rtt, err == nil)
	m.adaptMultipath(ctx) // 自适应多线：按端到端探测质量决定是否启用/关闭多线
	if err == nil {
		m.checkDegradation(ctx, node, rtt)
	} else {
		m.mu.Lock()
		m.degradeTicks = 0
		m.mu.Unlock()
	}
	var healthy bool
	if cfg.Frpc.IsEnabled() {
		healthy = m.fm.Healthy()
	} else {
		healthy = true // 未托管 frpc 时只看节点探测
	}

	if err == nil && healthy {
		m.sw.ResetMonitorFailures() // 成功即清零连续失败计数
		return
	}

	// 组装失败原因（中文，F7.3）
	var reason string
	if err != nil {
		reason = "节点探测失败：" + err.Error()
	}
	if cfg.Frpc.IsEnabled() && !healthy {
		if fatal := m.fm.FatalErr(); fatal != "" {
			reason = joinReason(reason, "frpc 不健康："+fatal)
		} else {
			reason = joinReason(reason, "frpc 不健康（未见登录成功或进程已退出）")
		}
	}
	logger.Warnf("当前节点 %s 监控异常（%d/%d）：%s",
		node.Name, m.sw.FailCount()+1, cfg.Switching.FailThreshold, reason)

	if dead := m.sw.RecordMonitorFailure(reason); dead {
		// 连续失败达阈值 → 通道 A（高优先级，不受任何冷却/驻留/上限限制）
		m.channelA(ctx, reason)
	}
}

// adaptMultipath 自适应多线：平时单线；当端到端探测出现明显抖动或失败时启用多线冗余，
// 稳定一段时间后再关闭。仅在 multipath 启用、自动挑选且 adaptive=true 时生效。
func (m *Monitor) adaptMultipath(ctx context.Context) {
	mp := m.Cfg().Multipath
	if !mp.IsEnabled() || !mp.IsAuto() || !mp.IsAdaptive() {
		return
	}
	m.mu.RLock()
	manual := m.mpManual
	m.mu.RUnlock()
	if manual != 0 {
		return // 用户手动指定了开/关，自适应不介入
	}
	m.mu.RLock()
	win := append([]time.Duration(nil), m.rttWindow...)
	winSamples := len(m.rttWindow)
	jitEW := m.jitterEWMA
	lossEW := m.lossEWMA
	streak := m.degradeStreak
	engaged := m.mpEngaged
	paths := append([]string(nil), m.mpPaths...)
	m.mu.RUnlock()
	if len(paths) == 0 {
		return
	}
	// 劣化判定①：抖动/丢包 EWMA 超阈值，需连续 engage_ticks 次（滞回，降误触发）
	// 劣化判定②：单次端到端 RTT 超过 engage_spike_ms → 视为尖峰，立即触发（不等连续）
	need := mp.EngageTicks
	if need < 1 {
		need = 1
	}
	recentSpike := false
	for _, v := range win {
		if spikeMs := mp.EngageSpikeMs; spikeMs > 0 && float64(v.Milliseconds()) > spikeMs {
			recentSpike = true
			break
		}
	}
	degradedTick := recentSpike || (winSamples >= 2 && (jitEW > mp.EngageJitterMs || lossEW > mp.EngageLossPct))
	if degradedTick {
		m.mu.Lock()
		m.degradeStreak++
		streak = m.degradeStreak
		if recentSpike {
			streak = need // 尖峰立即满足连续次数要求
		}
		if engaged {
			m.mpStableSince = time.Time{}
		}
		m.mu.Unlock()
		if engaged {
			return
		}
		if streak < need {
			logger.Infof("端到端质量劣化 %d/%d 次（抖动%.0fms 丢包%.0f%%），未达阈值暂不启用多线",
				streak, need, jitEW, lossEW)
			return
		}
		// 即将启用多线：先全量重测（拿到最新实测数据），再决定用 2 路还是 3 路
		if recentSpike {
			logger.Warnf("检测到端到端尖峰（单次 RTT 超过 %.0fms），立即全量重测并启用多线……", mp.EngageSpikeMs)
		} else {
			logger.Warnf("检测到网络劣化（抖动/丢包升高，连续 %d 次），正在全量重测以决定多线冗余路数……", streak)
		}
		m.fullRetest(ctx)
		k, reason := m.chooseRedundancy()
		if k <= 0 {
			logger.Warnf("全量重测后无可用候选路径，暂不启用多线：%s", reason)
			m.mu.Lock()
			m.mpStableSince = time.Time{}
			m.mu.Unlock()
			return
		}
		m.mu.Lock()
		m.mpRedundancy = k
		m.mpEngaged = true
		m.mpStableSince = time.Time{}
		m.mpReason = reason
		m.degradeStreak = 0
		m.mu.Unlock()
		logger.Warnf("全量重测完成 → 启用 %d 路多线冗余：%s", k, reason)
		m.applyMultipath()
		m.notify(i18n.T("已开启多线冗余", "Multipath enabled"), fmt.Sprintf(i18n.T("%d 路", "%d paths"), k))
		return
	}
	// 未劣化：清零连续劣化计数
	m.mu.Lock()
	m.degradeStreak = 0
	m.mu.Unlock()
	if !engaged {
		m.mu.Lock()
		m.mpStableSince = time.Time{}
		m.mu.Unlock()
		return
	}
	m.mu.Lock()
	if m.mpStableSince.IsZero() {
		m.mpStableSince = time.Now()
	}
	stableFor := time.Since(m.mpStableSince)
	m.mu.Unlock()
	if stableFor >= time.Duration(mp.DisengageSec)*time.Second {
		m.mu.Lock()
		m.mpEngaged = false
		m.mpStableSince = time.Time{}
		m.mpRedundancy = 0
		m.mpReason = ""
		m.mu.Unlock()
		logger.Infof("网络已稳定 %.0f 秒，关闭多线，回到单节点模式", stableFor.Seconds())
		m.syncMultipath()
		m.notify(i18n.T("已关闭多线冗余", "Multipath disabled"), "")
	}
}

// chooseRedundancy 自适应即将启用多线时，基于「刚完成的全量测速」在
// redundancy ~ redundancy_max 之间选择合适路数，并给出中文原因。
// 原则：默认尽量少路（2 路开销更小）；仅当前两条本身仍不够稳、
// 且第 3 条质量与之接近（加进来才真正增强冗余）时才启用 3 路。
func (m *Monitor) chooseRedundancy() (int, string) {
	cfg := m.Cfg()
	minK := cfg.Multipath.Redundancy
	if minK < 2 {
		minK = 2
	}
	maxK := cfg.Multipath.RedundancyMax
	if maxK < minK {
		maxK = minK
	}
	// 用户自选线路：按自选条数，不做自动判定
	pinnedN := len(cfg.Multipath.Paths)
	m.mu.RLock()
	if len(m.mpPinned) > 0 {
		pinnedN = len(m.mpPinned)
	}
	m.mu.RUnlock()
	if pinnedN > 0 {
		return pinnedN, fmt.Sprintf("采用用户自选线路 %d 条", pinnedN)
	}
	cands := m.rankedMpCands()
	n := len(cands)
	if n == 0 {
		return 0, "无可用候选路径"
	}
	if minK > n {
		minK = n
	}
	if maxK > n {
		maxK = n
	}
	if maxK <= minK {
		return maxK, fmt.Sprintf("可用路径仅 %d 条，取 %d 路", n, maxK)
	}
	// 不稳定度：抖动/丢包/尖峰/端到端最大-平均比 归一化叠加（越小越稳）
	inst := func(name string) float64 {
		rt := m.rtOf(name)
		if rt == nil || rt.Stats == nil {
			return 1
		}
		j := clamp01(rt.Stats.JitterMs / 50.0)
		l := clamp01(rt.Stats.LossPct / 8.0)
		sp := clamp01(rt.Stats.SpikePct / 25.0)
		sr := 0.0
		if rt.MC != nil && rt.MC.AvgRTTMs > 0 {
			sr = clamp01((rt.MC.MaxRTTMs/rt.MC.AvgRTTMs - 1.0) / 1.5)
		}
		return 0.35*j + 0.2*l + 0.2*sp + 0.25*sr
	}
	// 第 3 条优先选「与上两条不同地区」的（去相关，避免同一骨干一起抽风）
	third := cands[2]
	for _, c := range cands[2:] {
		if c.region != "" && c.region != cands[0].region && c.region != cands[1].region {
			third = c
			break
		}
	}
	i1, i2 := inst(cands[0].name), inst(cands[1].name)
	i3 := inst(third.name)
	avgTop2 := (i1 + i2) / 2
	scoreRatio := third.score / max(cands[1].score, 1e-6)
	diverse := third.region != "" && third.region != cands[0].region && third.region != cands[1].region
	// 第 3 条是否值得加入：总分接近第 2 条，且不稳定度不明显更差
	comparable := scoreRatio >= 0.90 && i3 <= i2+0.20 && i3 <= avgTop2+0.25
	if avgTop2 > 0.35 && comparable && (diverse || scoreRatio >= 0.97) {
		extra := ""
		if diverse {
			extra = "（不同地区，去相关）"
		}
		return 3, fmt.Sprintf("前 2 路仍不够稳（不稳定度 %.2f/%.2f），第 3 路质量接近（%.1f 分，约为第 2 路 %.0f%%，不稳定度 %.2f）%s→ 3 路",
			i1, i2, third.score, scoreRatio*100, i3, extra)
	}
	why := "前 2 路已足够稳"
	if !comparable {
		why = fmt.Sprintf("第 3 路质量明显较差（%.1f 分，约为第 2 路 %.0f%%）", third.score, scoreRatio*100)
	}
	return 2, fmt.Sprintf("%s（不稳定度 %.2f/%.2f/%.2f）→ 2 路", why, i1, i2, i3)
}

// clamp01 把值限制到 [0,1]
func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// checkDegradation 当前节点延迟持续偏高时累计连续次数，达阈值触发质量劣化自动切换。
func (m *Monitor) checkDegradation(ctx context.Context, node *config.Node, rtt time.Duration) {
	// 多路模式下不做单点劣化切换（路径由多路负责）
	if m.fwd.MultipathEnabled() {
		return
	}
	// 用户已交互自选节点 → 不做劣化自动切换，避免当前节点乱跳
	if m.userPinned() {
		return
	}
	sw := m.Cfg().Switching
	if !sw.DegradeEnabled() {
		return
	}
	if rtt < time.Duration(sw.DegradationMs)*time.Millisecond {
		m.mu.Lock()
		m.degradeTicks = 0
		m.mu.Unlock()
		return
	}
	m.mu.Lock()
	m.degradeTicks++
	n := m.degradeTicks
	m.mu.Unlock()
	if n < sw.DegradationTicks {
		logger.Warnf("当前节点 %s 延迟偏高（%s），连续 %d/%d 次（达阈值将自动切换）",
			node.Name, rtt.Round(time.Millisecond), n, sw.DegradationTicks)
		return
	}
	m.mu.Lock()
	m.degradeTicks = 0
	m.mu.Unlock()
	m.degradationSwitch(ctx, node, rtt)
}

// degradationSwitch 质量劣化自动切换：切到最优的其他节点；按配置可断开现有玩家连接强制重连。
func (m *Monitor) degradationSwitch(ctx context.Context, cur *config.Node, rtt time.Duration) {
	best := m.bestEligibleNode(cur.Name)
	if best == nil {
		logger.Warnf("当前节点 %s 持续高延迟（%s），但没有更优节点可切，继续观察", cur.Name, rtt.Round(time.Millisecond))
		return
	}
	logger.Warnf("当前节点 %s 持续高延迟（%s ≥ 阈值），自动切换到更稳节点 %s", cur.Name, rtt.Round(time.Millisecond), best.Name)
	m.switchTo(ctx, best, switcher.Decision{
		Switch:  true,
		Channel: "A",
		Reason:  fmt.Sprintf("质量劣化自动切换：%s 持续高延迟（%s），切换到 %s", cur.Name, rtt.Round(time.Millisecond), best.Name),
	})
	if m.Cfg().Switching.DegradeDropPlayers() {
		if n := m.fwd.CloseAll(); n > 0 {
			logger.Warnf("已断开 %d 个玩家连接，强制重连到新节点 %s", n, best.Name)
		}
	}
}

// retestTick ② 全量重测 + 通道 B 更优切换评估
func (m *Monitor) retestTick(ctx context.Context) {
	m.retest(ctx, false)
}

// retest 全量重测。force=false 时，若有玩家在线则跳过：
// 避免测速的并发连接突发对正在进行的游戏造成额外抖动。
func (m *Monitor) retest(ctx context.Context, force bool) {
	if !force && m.Cfg().Switching.PauseWhenActive() {
		if n := m.fwd.ActiveConns(); n > 0 {
			logger.Infof("有 %d 个玩家在线，跳过本轮全量重测（避免打扰游戏），仅保留当前节点监控", n)
			return
		}
	}
	m.fullRetest(ctx)
	m.evaluateChannelB(ctx)
}

// evaluateChannelB 通道 B 决策（更优切换四道闸门，§7.4）
func (m *Monitor) evaluateChannelB(ctx context.Context) {
	// 多路模式下节点切换无意义（连接同时走多条路径），由路径自动挑选负责
	if m.fwd.MultipathEnabled() {
		return
	}
	// 用户已交互自选当前/备用节点 → 不做自动「更优切换」，避免当前节点乱跳
	if m.userPinned() {
		return
	}
	// 有玩家在线时暂停“更优切换”（通道B），避免打扰正在进行的游戏；
	// 故障切换（通道A）不在此限，仍会正常切。
	if m.Cfg().Switching.PauseWhenActive() {
		if n := m.fwd.ActiveConns(); n > 0 {
			logger.Infof("更优切换评估：当前有 %d 个玩家连接在线，按配置暂停更优切换（通道B），保持节点不变", n)
			return
		}
	}
	curNode := m.CurrentNode()
	best := m.bestEligibleNode("")
	if best == nil {
		return
	}
	if curNode != nil && curNode.Name == best.Name {
		return // 已是最优
	}
	total := 0.0
	if rt := m.rtOf(best.Name); rt != nil && rt.Score != nil {
		total = rt.Score.Total
	}
	logger.Infof("空闲时切到当前最优节点：%s（%.1f 分），原节点 %s", best.Name, total, nameOf(curNode))
	m.switchTo(ctx, best, switcher.Decision{
		Switch:  true,
		Channel: "B",
		Reason:  fmt.Sprintf("空闲时选择当前总分最高的节点 %s（%.1f 分）", best.Name, total),
	})
}

// onFrpcEvent 处理 frpc 管理器事件
func (m *Monitor) onFrpcEvent(ctx context.Context, ev frpc.Event) {
	switch ev.Kind {
	case frpc.EventRestartLimited:
		// 滑动窗口内重启超限 → 通道 A（F7.1）
		logger.Errorf("%s", ev.Msg)
		m.channelA(ctx, ev.Msg)
	case frpc.EventFatal:
		// 致命错误已由管理器记 ERROR 日志；此处仅驱动一次监控判定
		// （健康判定会因 fatal 标志失败，由 fail_threshold 兜底）
		logger.Debugf("frpc 致命错误事件：%s", ev.Msg)
	}
}

// onCommand 处理 watch 模式单字符命令（F13.2）
func (m *Monitor) onCommand(ctx context.Context, c rune) {
	// 数字键：[1-9] 选择第 1-9 个节点，[0] 选择第 10 个（用户显式选节点）；
	// 若处于 [b] 备用选择模式，则改为把该节点设为备用节点。
	if c >= '1' && c <= '9' || c == '0' {
		n := int(c - '0')
		if c == '0' {
			n = 10
		}
		m.mu.RLock()
		selBackup := m.selectingBackup
		m.mu.RUnlock()
		if selBackup {
			m.mu.Lock()
			m.selectingBackup = false
			m.mu.Unlock()
			logger.Infof("收到命令 [%c]：把第 %d 个节点设为备用节点", c, n)
			m.setBackupByIndex(ctx, n)
			return
		}
		logger.Infof("收到命令 [%c]：选择第 %d 个节点", c, n)
		m.selectByIndex(ctx, n)
		return
	}
	switch c {
	case 'r':
		logger.Infof("收到命令 [r]：重载配置")
		m.forceReload()
	case 't':
		logger.Infof("收到命令 [t]：立即重测")
		m.retest(ctx, true)
		m.resetTickers() // 重置重测定时器，避免立即又触发一轮
	case 's':
		logger.Infof("收到命令 [s]：切换到下一可用节点")
		m.manualNext(ctx)
	case 'a':
		logger.Infof("收到命令 [a]：切回自动模式并重新自动选择")
		m.autoReselect(ctx)
	case 'b':
		m.mu.Lock()
		m.selectingBackup = !m.selectingBackup
		on := m.selectingBackup
		m.mu.Unlock()
		if on {
			logger.Infof("收到命令 [b]：进入备用节点选择模式（按 [1-9]/[0] 指定备用节点，再按 [b] 取消）")
		} else {
			logger.Infof("收到命令 [b]：取消备用节点选择")
		}
	case 'm':
		m.mu.Lock()
		m.mpManual = (m.mpManual + 1) % 3
		mode := m.mpManual
		m.mpEngaged = false // 手动指定后重置自适应状态
		m.mpStableSince = time.Time{}
		m.mu.Unlock()
		logger.Infof("收到命令 [m]：多线模式 → %s", mpModeText(mode))
		m.syncMultipath()
	case 'q':
		logger.Infof("收到命令 [q]：退出")
		if m.cancel != nil {
			m.cancel()
		}
	}
}

// ---------- 切换通道 ----------

// channelA 通道 A（故障切换）：立即全量重测并切换，不受任何冷却/驻留/上限限制（F7.1）。
// 例外：当前节点是唯一可用节点 → 只退避重启 frpc，不切换。
func (m *Monitor) channelA(ctx context.Context, trigger string) {
	// 多路模式下不做节点故障切换（每条路径独立，断了会自动重连），仅清失败计数
	if m.fwd.MultipathEnabled() {
		m.sw.ResetMonitorFailures()
		return
	}
	logger.Warnf("触发故障切换（通道A）：%s", trigger)
	m.fullRetest(ctx)

	cur := m.CurrentNode()
	best := m.bestEligibleNode("")

	// 全灭 → 兜底循环（F8：退避重测 + 钩子 + 红色告警，进程不退出）
	if best == nil {
		m.allDownLoop(ctx)
		return
	}

	// 最优仍是当前节点：不切换（未托管 frpc 时无需重启，只清零失败计数）
	if cur != nil && best.Name == cur.Name {
		logger.Warnf("重测后当前节点 %s 仍是最优可用节点，保持不变", cur.Name)
		if m.Cfg().Frpc.IsEnabled() {
			m.restartCurrent(ctx)
		}
		m.sw.ResetMonitorFailures()
		return
	}

	// 唯一可用节点例外条款：只退避重启 frpc（frpc 托管时才有意义）
	enabledCount := 0
	for _, n := range m.rm.Nodes() {
		if n.IsEnabled() {
			enabledCount++
		}
	}
	if enabledCount == 1 && cur != nil {
		d := m.sw.EvaluateChannelA(cur.Name, true)
		logger.Infof("%s", d.Reason)
		if m.Cfg().Frpc.IsEnabled() {
			m.restartCurrent(ctx)
		}
		m.sw.ResetMonitorFailures()
		return
	}

	d := m.sw.EvaluateChannelA(nameOf(cur), false)
	logger.Infof("%s", d.Reason)
	if d.Switch {
		m.switchTo(ctx, best, d)
	}
}

// restartCurrent 退避重启当前节点的 frpc（Stop + Start，保留重启计数与退避状态）
func (m *Monitor) restartCurrent(ctx context.Context) {
	if !m.Cfg().Frpc.IsEnabled() {
		return // 未托管 frpc，无需重启
	}
	node := m.CurrentNode()
	if node == nil {
		return
	}
	m.fm.Stop()
	if err := m.fm.Start(node); err != nil {
		logger.Errorf("frpc 重启失败：%v（管理器将继续退避重试）", err)
		return
	}
	m.fm.WaitLoginOK(ctx, 15*time.Second)
	m.saveState()
}

// switchTo 执行切换：Stop → 渲染新配置 → Start → 等登录 → 登记 → 钩子 → 存档（§8.2）
func (m *Monitor) switchTo(ctx context.Context, node *config.Node, d switcher.Decision) {
	prev := m.CurrentNode()
	logger.Infof("执行切换：%s", d.Reason)
	if m.Cfg().Frpc.IsEnabled() {
		m.fm.Stop()
		if err := m.fm.Start(node); err != nil {
			logger.Errorf("切换到 %s 失败（frpc 启动异常）：%v（管理器将继续退避重试）", node.Name, err)
		} else if !m.fm.WaitLoginOK(ctx, 15*time.Second) {
			if fatal := m.fm.FatalErr(); fatal != "" {
				logger.Errorf("frpc 登录未成功：%s（将按退避策略重试；若持续失败请检查 token/端口）", fatal)
			} else {
				logger.Warnf("frpc 已拉起但 15 秒内未观察到“登录成功”，请关注后续日志")
			}
		}
	}
	m.setCurrent(node)
	m.clearRTT()
	// 仅「自动切换」通知，且极简（不影响游戏）；手动切换（数字/[s]/[a]）不通知
	if (d.Channel == "A" || d.Channel == "B") && (prev == nil || prev.Name != node.Name) {
		m.notify(i18n.T("已切换节点", "Node switched"), node.Name)
	}
	// 更新本地转发目标（新连接走新节点隧道，旧连接保持）
	target := fmt.Sprintf("%s:%d", node.ServerAddr, node.ServerPort)
	m.fwd.SetTarget(target)
	logger.Infof("本地转发目标已更新：%s -> %s（节点 %s 的隧道）", m.fwdListen(), target, node.Name)
	// 通道 A 的系统选择视为回到自动模式；手动命令 [s] 的模式由 manualNext 设置
	if d.Channel == "A" {
		m.setMode("自动")
	}
	m.sw.RegisterSwitch() // 刷新驻留起点、冷却截止、滑动窗口计数（F7.2）
	m.saveState()
	m.runHook(m.Cfg().Hooks.OnSwitch, node, "on_switch")
}

// manualNext 手动切换到下一个可用节点（[s] 命令；用户显式命令，不受四道闸门限制）
func (m *Monitor) manualNext(ctx context.Context) {
	nodes := m.rm.Nodes()
	// 收集启用节点
	var enabled []*config.Node
	for i := range nodes {
		if nodes[i].IsEnabled() {
			enabled = append(enabled, &nodes[i])
		}
	}
	if len(enabled) == 0 {
		logger.Warnf("没有启用节点，无法手动切换")
		return
	}
	cur := m.CurrentNode()
	idx := -1
	for i, n := range nodes {
		if cur != nil && n.Name == cur.Name {
			idx = i
		}
	}
	// 第一优先：下一个“可选举”的节点
	for step := 1; step <= len(enabled); step++ {
		n := enabled[(idx+step)%len(enabled)]
		if cur != nil && n.Name == cur.Name {
			continue
		}
		if rt := m.rtOf(n.Name); rt != nil && rt.Score != nil && rt.Score.Eligible {
			m.setMode("手动")
			m.switchTo(ctx, n, switcher.Decision{
				Switch: true, Channel: "手动",
				Reason: fmt.Sprintf("手动切换（用户命令 [s]）：%s → %s", nameOf(cur), n.Name),
			})
			m.pinCurrent(n.Name)
			m.applyMultipath()
			return
		}
	}
	// 没有 eligible 的其他节点：退而切到下一个启用节点（用户强制命令）
	n := enabled[(idx+1)%len(enabled)]
	if cur != nil && n.Name == cur.Name {
		logger.Warnf("没有其他启用节点可切换")
		return
	}
	m.setMode("手动")
	m.switchTo(ctx, n, switcher.Decision{
		Switch: true, Channel: "手动",
		Reason: fmt.Sprintf("手动切换（用户命令 [s]，无可选举节点，切换到下一个启用节点）：%s → %s", nameOf(cur), n.Name),
	})
	m.pinCurrent(n.Name)
	m.applyMultipath()
}

// selectByIndex 按状态屏编号选择节点（数字键 [1-9] / [0]=第10个；用户显式命令，不受四道闸门限制）。
// 编号顺序与状态屏 / rm.Nodes() 一致（1 基）。选中的节点会写入自选线路，锁定为多线主路径。
func (m *Monitor) selectByIndex(ctx context.Context, n int) {
	nodes := m.rm.Nodes()
	if n < 1 || n > len(nodes) {
		logger.Warnf("选择无效：节点编号 %d 超出范围（当前共 %d 个节点）", n, len(nodes))
		return
	}
	target := &nodes[n-1]
	if !target.IsEnabled() {
		logger.Warnf("节点 [%d] %s 已停用（enabled=false），无法选择", n, target.Name)
		return
	}
	cur := m.CurrentNode()
	if cur != nil && cur.Name == target.Name {
		m.pinCurrent(target.Name) // 已是当前节点，仍锁定为多线主路径
		m.applyMultipath()
		logger.Infof("节点 [%d] %s 已是当前节点，已锁定为多线主路径", n, target.Name)
		return
	}
	m.setMode("手动")
	m.switchTo(ctx, target, switcher.Decision{
		Switch:  true,
		Channel: "手动",
		Reason:  fmt.Sprintf("手动选择（用户命令 [%d]）：%s → %s", n, nameOf(cur), target.Name),
	})
	m.pinCurrent(target.Name)
	m.applyMultipath()
}

// setBackupByIndex 把第 n 个节点设为备用节点（[b] 后按数字键触发）。
// 备用节点写入自选线路第 2 位，多线冗余与状态屏「备用节点」随之同步。
func (m *Monitor) setBackupByIndex(ctx context.Context, n int) {
	nodes := m.rm.Nodes()
	if n < 1 || n > len(nodes) {
		logger.Warnf("选择备用节点无效：编号 %d 超出范围（当前共 %d 个节点）", n, len(nodes))
		return
	}
	target := &nodes[n-1]
	if !target.IsEnabled() {
		logger.Warnf("备用节点 [%d] %s 已停用（enabled=false），无法选择", n, target.Name)
		return
	}
	cur := m.CurrentNode()
	if cur == nil {
		logger.Warnf("当前没有节点，无法设置备用节点")
		return
	}
	if target.Name == cur.Name {
		logger.Warnf("备用节点不能与当前节点相同（%s）", target.Name)
		return
	}
	m.pinBackup(cur.Name, target.Name)
	m.applyMultipath()
	logger.Infof("备用节点已设为：%s（当前节点 %s）", target.Name, cur.Name)
}

// pinCurrent 把节点名锁定为自选线路的主路径，只保留「新当前 + 原备用」两条
// （避免每次换当前节点都把旧当前累积成第 3、4 条路径）。
func (m *Monitor) pinCurrent(name string) {
	m.mu.Lock()
	backup := ""
	if len(m.mpPinned) >= 2 && m.mpPinned[1] != name {
		backup = m.mpPinned[1]
	}
	pinned := []string{name}
	if backup != "" {
		pinned = append(pinned, backup)
	}
	m.mpPinned = pinned
	m.mu.Unlock()
}

// pinBackup 锁定「当前节点 + 备用节点」的自选线路（主路径 + 第 2 路）。
func (m *Monitor) pinBackup(current, backup string) {
	m.mu.Lock()
	m.mpPinned = []string{current, backup}
	m.mu.Unlock()
}

// autoReselect 切回自动模式，并按最新一轮测速结果立即重新选择最优节点（用户命令 [a]）。
// 与通道 B 的四道闸门无关：这是用户显式要求“回到自动并重新选优”，立即生效。
func (m *Monitor) autoReselect(ctx context.Context) {
	m.mu.Lock()
	m.mpPinned = nil // 回到自动：解除交互自选线路锁定
	m.mu.Unlock()
	m.setMode("自动")
	best := m.bestEligibleNode("")
	if best == nil {
		logger.Warnf("切回自动模式：当前没有可用节点（全部测速失败或被一票否决），保持当前节点并等待下轮重测")
		return
	}
	cur := m.CurrentNode()
	if cur != nil && cur.Name == best.Name {
		logger.Infof("切回自动模式：当前节点 %s 已是最优（%.1f 分），保持不变", best.Name, m.rtOf(best.Name).Score.Total)
		m.sw.ResetMonitorFailures()
		return
	}
	score := m.rtOf(best.Name).Score.Total
	logger.Infof("切回自动模式：重新选择最优节点 %s（%.1f 分）", best.Name, score)
	m.switchTo(ctx, best, switcher.Decision{
		Switch:  true,
		Channel: "自动",
		Reason:  fmt.Sprintf("切回自动模式（用户命令 [a]）并重新选优：%s → %s（%.1f 分）", nameOf(cur), best.Name, score),
	})
}

// ---------- 全灭兜底（F8） ----------

// allDownLoop 全部节点不可用：等待→重测循环；等待逐次翻倍封顶 10 分钟；
// 每轮执行 on_all_nodes_down 钩子；红色高亮 + ERROR 日志；进程不退出、不 panic。
func (m *Monitor) allDownLoop(ctx context.Context) {
	cfg := m.Cfg()
	wait := time.Duration(cfg.General.AllNodesDownWaitSec) * time.Second
	logger.Errorf("\x1b[31m全部节点不可用！进入退避重测循环（初始 %s，逐次翻倍，封顶 600s），进程保持运行\x1b[0m", wait)
	m.notify(i18n.T("全部节点不可用", "All nodes unavailable"), i18n.T("已进入退避重试，恢复后自动切回", "backing off; will switch back when recovered"))

	for {
		m.runHook(cfg.Hooks.OnAllNodesDown, nil, "on_all_nodes_down")

		// 等待（可被 t=立即重试 / r=重载配置 / 退出 打断）
		timer := time.NewTimer(wait)
	waitLoop:
		for {
			select {
			case <-ctx.Done():
				timer.Stop()
				m.gracefulShutdown()
				return
			case <-timer.C:
				break waitLoop
			case c := <-m.cmdCh():
				if c == 't' {
					timer.Stop()
					logger.Infof("收到命令 [t]：立即重测")
					break waitLoop
				}
				if c == 'r' {
					m.forceReload()
				}
				// 数字键：用户在全灭期间手动指定节点，立即尝试并跳出等待
				if c >= '1' && c <= '9' {
					timer.Stop()
					m.selectByIndex(ctx, int(c-'0'))
					break waitLoop
				}
				if c == '0' {
					timer.Stop()
					m.selectByIndex(ctx, 10)
					break waitLoop
				}
				// q 命令：cancel 后下一轮 select 命中 ctx.Done
			}
		}

		// 翻倍封顶 600 秒
		wait *= 2
		if wait > 600*time.Second {
			wait = 600 * time.Second
		}

		m.fullRetest(ctx)
		best := m.bestEligibleNode("")
		if best == nil {
			logger.Errorf("\x1b[31m重测后仍全部节点不可用，%.0f 秒后继续重试\x1b[0m", wait.Seconds())
			continue
		}

		// 恢复：当前节点仍可用则维持，否则直接切到最优（通道 A 语义）
		cur := m.CurrentNode()
		if cur == nil || best.Name != cur.Name {
			m.switchTo(ctx, best, switcher.Decision{
				Switch: true, Channel: "A",
				Reason: fmt.Sprintf("从全部节点不可用中恢复，切换到当前最优节点 %s（%.1f 分）", best.Name, m.rtOf(best.Name).Score.Total),
			})
		} else {
			logger.Infof("节点 %s 已恢复可用，继续托管", cur.Name)
			m.sw.ResetMonitorFailures()
		}
		return
	}
}

// ---------- 全量重测 ----------

// fullRetest 一轮完整测速：两阶段探测 → 状态更新 → 历史 EWMA → 结果日志
func (m *Monitor) fullRetest(ctx context.Context) {
	cfg := m.Cfg()
	nodes := m.rm.Nodes() // 远程+本地合并后的生效节点（拉取失败已回退本地）
	p := prober.New(cfg, m.hist, nodes)
	prev := m.Nodes()
	res := p.RunAll(ctx, prev)
	m.mu.Lock()
	m.nodes = res
	m.mu.Unlock()

	// 历史更新（F5.1/F5.2）：每轮全量测速后对每节点记录 EWMA 平滑分；
	// 本轮离线/超时/一票否决 → x=0（离线自然拉低历史分）
	for i := range nodes {
		n := &nodes[i]
		if !n.IsEnabled() {
			continue
		}
		rt, ok := res[n.Name]
		offline := !ok || rt.Score == nil || !rt.Score.Eligible
		x := 0.0
		if !offline {
			x = rt.Score.Total
		}
		m.hist.Update(n.Name, x, offline)
	}
	if err := m.hist.Save(); err != nil {
		logger.Warnf("保存历史数据失败（不影响运行）：%v", err)
	}

	// 结果日志（含各分项分数与中文原因，F4.3）
	for i := range nodes {
		n := &nodes[i]
		rt, ok := res[n.Name]
		if !ok {
			continue
		}
		if rt.Status == model.StatusDisabled {
			logger.Infof("节点 %s：DISABLED（已停用）", n.Name)
			continue
		}
		if rt.Score != nil {
			logger.Infof("节点 %s：[%s] 总分 %.1f —— %s", n.Name, rt.Status, rt.Score.Total, rt.Score.Reason)
		} else {
			logger.Infof("节点 %s：[%s] %s", n.Name, rt.Status, rt.StatusReason)
		}
	}

	// 每轮测速后按稳定性重新挑选多路路径（新连接生效，不影响进行中的连接）
	m.applyMultipath()
}

// ---------- 初始选择 ----------

// pickInitial 启动时选择初始节点：
//   - 手动模式（--node）：直接用指定节点；
//   - 自动模式：优先恢复 state.json 中的上次节点（避免重启后无谓切换），
//     该节点不可用则选当前最优；全灭时仍托管一个节点退避重试（F8/验收#1）。
func (m *Monitor) pickInitial(ctx context.Context) {
	nodes := m.rm.Nodes()

	// 用户已交互自选当前节点（mpPinned[0]）→ 尊重自选，不被自动最优覆盖
	m.mu.RLock()
	pinnedCur := ""
	if len(m.mpPinned) > 0 {
		pinnedCur = m.mpPinned[0]
	}
	m.mu.RUnlock()
	if pinnedCur != "" {
		for i := range nodes {
			if nodes[i].Name == pinnedCur && nodes[i].IsEnabled() {
				logger.Infof("启动：采用用户自选当前节点 %s", pinnedCur)
				m.setCurrent(&nodes[i])
				m.startCurrent(ctx)
				return
			}
		}
	}

	// 多路模式：节点显示与选路由 applyMultipath 负责（取总分最高的路径），不恢复旧节点、不单点选择
	if m.fwd.MultipathEnabled() {
		m.startCurrent(ctx)
		return
	}

	// 手动模式
	if m.manualInitial != "" {
		var n *config.Node
		for i := range nodes {
			if nodes[i].Name == m.manualInitial {
				n = &nodes[i]
				break
			}
		}
		if n == nil {
			logger.Errorf("手动指定的节点 %s 不存在（远程+本地均无此节点），回退自动选择", m.manualInitial)
		} else if !n.IsEnabled() {
			logger.Warnf("手动指定的节点 %s 已停用（enabled=false），回退自动选择", m.manualInitial)
		} else {
			m.setMode("手动")
			m.setCurrent(n)
			m.startCurrent(ctx)
			return
		}
	}

	// 启动直接选当前最优节点（总分最高），使「当前节点」与屏幕上的总分一致
	if best := m.bestEligibleNode(""); best != nil {
		rt := m.rtOf(best.Name)
		logger.Infof("初始选择最优节点：%s（%.1f 分）—— %s", best.Name, rt.Score.Total, rt.Score.Reason)
		m.setCurrent(best)
		m.startCurrent(ctx)
		return
	}

	// 全灭：仍托管一个节点（savedNode 或第一个启用节点）让 frpc 退避重试
	logger.Errorf("当前没有可用的最优节点（全部测速失败或被一票否决）")
	var pick *config.Node
	if m.savedNode != "" {
		for i := range nodes {
			if nodes[i].Name == m.savedNode && nodes[i].IsEnabled() {
				pick = &nodes[i]
				break
			}
		}
	}
	if pick == nil {
		for i := range nodes {
			if nodes[i].IsEnabled() {
				pick = &nodes[i]
				break
			}
		}
	}
	if pick != nil {
		if m.Cfg().Frpc.IsEnabled() {
			logger.Warnf("frpc 先托管节点 %s 并退避重试，等待其恢复（进程不退出）", pick.Name)
		} else {
			logger.Warnf("先保留节点 %s 为当前节点，等待下轮重测恢复（进程不退出）", pick.Name)
		}
		m.setCurrent(pick)
		m.startCurrent(ctx)
	}
}

// startCurrent 拉起当前节点的 frpc 并等待登录结果（未托管 frpc 时跳过）
// 无论是否托管 frpc，都更新本地转发器的目标（玩家连 127.0.0.1:local_port -> 当前节点隧道）
func (m *Monitor) startCurrent(ctx context.Context) {
	m.clearRTT()
	node := m.CurrentNode()
	if node != nil {
		target := fmt.Sprintf("%s:%d", node.ServerAddr, node.ServerPort)
		m.fwd.SetTarget(target)
		logger.Infof("本地转发目标：%s -> %s（节点 %s 的隧道）", m.fwdListen(), target, node.Name)
	}
	if !m.Cfg().Frpc.IsEnabled() {
		// 未托管 frpc：仅设置转发目标，不拉起 frpc 进程
		m.saveState()
		return
	}
	node = m.CurrentNode()
	if node == nil {
		return
	}
	if err := m.fm.Start(node); err != nil {
		logger.Errorf("frpc 启动失败：%v（管理器将继续退避重试）", err)
		return
	}
	if !m.fm.WaitLoginOK(ctx, 15*time.Second) {
		if fatal := m.fm.FatalErr(); fatal != "" {
			logger.Errorf("frpc 登录未成功：%s", fatal)
		} else {
			logger.Warnf("frpc 已拉起但 15 秒内未观察到“登录成功”，请关注后续日志")
		}
	}
	m.saveState()
}

// ---------- 配置热加载（F1.4） ----------

// reloadIfChanged 轮询配置 mtime，变化后重新加载校验；
// 校验失败保留旧配置并打 ERROR 日志（含中文原因），不影响运行。
func (m *Monitor) reloadIfChanged() {
	fi, err := os.Stat(m.cfgPath)
	if err != nil {
		return
	}
	mt := fi.ModTime()
	if !mt.After(m.lastCfgMod) {
		return
	}
	m.lastCfgMod = mt
	m.doReload()
}

// forceReload 强制重载（[r] 命令）
func (m *Monitor) forceReload() {
	if fi, err := os.Stat(m.cfgPath); err == nil {
		m.lastCfgMod = fi.ModTime()
	}
	m.doReload()
}

// doReload 实际执行加载与生效
func (m *Monitor) doReload() {
	cfg, err := config.Load(m.cfgPath)
	if err != nil {
		// 校验失败：保留旧配置，仅 ERROR 日志（F1.4）
		logger.Errorf("配置热加载失败，保留旧配置继续运行：%v", err)
		return
	}
	m.mu.Lock()
	m.cfg = cfg
	m.mu.Unlock()

	// 应用热生效项
	logger.SetLevel(logger.ParseLevel(cfg.General.LogLevel))
	m.hist.SetParams(cfg.Scoring.History.UpdateAlpha, cfg.Scoring.History.NeutralScore)
	// 重新拉取远程节点列表（远程 URL 可能已改）
	m.rm.Refresh()
	m.applyMultipath()
	m.registerSecrets()
	m.checkRemotePortConsistency()
	m.resetTickers() // 监控/重测间隔可能已变化
	logger.Infof("配置已热加载生效（本地 %d 个节点，远程源 %s）",
		len(cfg.Nodes), remoteDesc(cfg.Remote.URL))

	// 当前节点被删除或停用 → 立即重测切换
	cur := m.CurrentNode()
	if cur != nil {
		found := false
		for _, n := range m.rm.Nodes() {
			if n.Name == cur.Name {
				found = true
				if !n.IsEnabled() {
					found = false
				}
				break
			}
		}
		if !found {
			logger.Warnf("当前节点 %s 已被移除或停用，立即重测并切换", cur.Name)
			go func() {
				m.fullRetest(m.ctx)
				if best := m.bestEligibleNode(""); best != nil {
					m.switchTo(m.ctx, best, switcher.Decision{
						Switch: true, Channel: "A",
						Reason: fmt.Sprintf("当前节点 %s 被移除/停用，切换到最优节点 %s", cur.Name, best.Name),
					})
				} else {
					m.allDownLoop(m.ctx)
				}
			}()
		}
	}
}

// ---------- 启动自检 ----------

// checkLocalMC 检测本地 MC 服务端是否监听（F10.3）。
// 仅在托管 frpc 时检测（frpc 需要转发到本地服务端）；
// 隧道由服务端建立（未托管 frpc）时跳过——本地没有 MC 服务端也正常。
func (m *Monitor) checkLocalMC() {
	cfg := m.Cfg()
	if !cfg.Frpc.IsEnabled() {
		return
	}
	addr := net.JoinHostPort(cfg.Minecraft.LocalHost, fmt.Sprintf("%d", cfg.Minecraft.LocalPort))
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		logger.Warnf("本地 MC 服务端未监听 %s 端口，请先启动服务端（不阻断启动；服务端就绪前玩家无法连接）", addr)
		return
	}
	_ = conn.Close()
	logger.Infof("本地 MC 服务端检测通过：%s", addr)
}

// checkRemotePortConsistency 各启用节点 remote_port 不一致时告警（F1.5，不阻断）
func (m *Monitor) checkRemotePortConsistency() {
	seen := make(map[int]bool)
	for _, n := range m.rm.Nodes() {
		if !n.IsEnabled() {
			continue
		}
		seen[n.RemotePort] = true
	}
	if len(seen) > 1 {
		logger.Warnf("各启用节点的 remote_port 不一致（%v）：建议统一端口，切换后玩家无需改连接地址", keysOf(seen))
	}
}

// registerSecrets 把全部 token 登记进日志脱敏表（F12.3）
func (m *Monitor) registerSecrets() {
	logger.ResetSecrets()
	for _, n := range m.rm.Nodes() {
		logger.RegisterSecret(n.Token)
	}
}

// mpCand 多线候选路径（节点名 + 隧道端点 + 总分）
type mpCand struct {
	name   string
	ep     string
	score  float64
	region string
}

// rankedMpCands 按总分从高到低给出可选举的多线候选路径。
func (m *Monitor) rankedMpCands() []mpCand {
	nodes := m.rm.Nodes()
	cands := make([]mpCand, 0, len(nodes))
	for i := range nodes {
		n := &nodes[i]
		if !n.IsEnabled() {
			continue
		}
		rt := m.rtOf(n.Name)
		if rt == nil || rt.Score == nil || !rt.Score.Eligible {
			continue
		}
		cands = append(cands, mpCand{
			name:   n.Name,
			ep:     fmt.Sprintf("%s:%d", n.ServerAddr, n.ServerPort),
			score:  rt.Score.Total,
			region: n.Region,
		})
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].score > cands[j].score })
	return cands
}

// pickDiverse 从已按总分排序的候选中挑 k 条，优先覆盖**不同地区**（避免同一骨干同时抽风）。
// 第一条始终是总分最高者；不同地区不够时再按总分补齐。
func pickDiverse(cands []mpCand, k int) []mpCand {
	if k <= 0 {
		return nil
	}
	if k >= len(cands) {
		return cands
	}
	chosen := make([]mpCand, 0, k)
	used := make(map[string]bool, k)
	for _, c := range cands {
		if len(chosen) >= k {
			break
		}
		if c.region == "" || !used[c.region] {
			chosen = append(chosen, c)
			if c.region != "" {
				used[c.region] = true
			}
		}
	}
	if len(chosen) < k {
		for _, c := range cands {
			if len(chosen) >= k {
				break
			}
			dup := false
			for _, x := range chosen {
				if x.name == c.name {
					dup = true
					break
				}
			}
			if !dup {
				chosen = append(chosen, c)
			}
		}
	}
	return chosen
}

// applyMultipath 按配置启用/关闭多路冗余转发。线路优先级：
// 用户交互自选(mpPinned) > 配置自选(paths) > 自动按总分挑选。
func (m *Monitor) applyMultipath() {
	cfg := m.Cfg()
	mp.SetLargeFramePaths(cfg.Multipath.LargePaths)
	mp.SetFailoverOnly(cfg.Multipath.FailoverOnlyOn())
	mp.SetAutoRedundancy(cfg.Multipath.AutoRedundancyOn())
	mp.SetWeakThresholds(cfg.Multipath.QueueRiseMs, cfg.Multipath.WeakJitterMs)
	mp.SetSpikeThresholdMs(cfg.Switching.SpikeMs)
	if !cfg.Multipath.IsEnabled() {
		m.mu.Lock()
		m.mpPaths = nil
		m.mpPathNames = nil
		m.mu.Unlock()
		m.fwd.SetMultipath(nil)
		return
	}
	// 用户自选线路优先（交互锁定 > 配置 paths）
	m.mu.RLock()
	refs := append([]string(nil), m.mpPinned...)
	m.mu.RUnlock()
	if len(refs) == 0 {
		refs = cfg.Multipath.Paths
	}
	if len(refs) > 0 {
		m.applySelectedPaths(refs)
		return
	}
	if !cfg.Multipath.IsAuto() {
		// 未自选且关闭自动挑选：不启用多路（保持单目标）
		m.mu.Lock()
		m.mpPaths = nil
		m.mpPathNames = nil
		m.mu.Unlock()
		m.fwd.SetMultipath(nil)
		return
	}
	cands := m.rankedMpCands()
	if len(cands) == 0 {
		m.fwd.SetMultipath(nil) // 还没有测速结果：先用单目标，等测速后再切多路
		return
	}
	k := cfg.Multipath.Redundancy
	m.mu.RLock()
	if m.mpRedundancy > 0 {
		k = m.mpRedundancy // 自适应进入多线时按实测选定的路数
	}
	m.mu.RUnlock()
	if maxK := cfg.Multipath.RedundancyMax; maxK > 0 && k > maxK {
		k = maxK
	}
	if k < 1 {
		k = 1
	}
	if k > len(cands) {
		k = len(cands)
	}
	picked := pickDiverse(cands, k)
	paths := make([]string, 0, len(picked))
	names := make([]string, 0, len(picked))
	for _, c := range picked {
		paths = append(paths, c.ep)
		names = append(names, c.name)
	}
	m.mu.Lock()
	m.mpPaths = paths
	m.mpPathNames = names
	m.mu.Unlock()
	m.syncMultipath()
	// 让状态屏「当前节点」与主路径（总分最高的那条）一致
	nodes := m.rm.Nodes()
	for i := range nodes {
		if nodes[i].Name == cands[0].name {
			m.setCurrent(&nodes[i])
			break
		}
	}
	logger.Infof("多路候选 %d 条（按总分+去相关）：%v（多线模式=%s，当前=%s）",
		len(paths), names, mpModeText(m.mpManual), mpOnText(m.mpOn()))
}

// applySelectedPaths 应用用户自选线路（交互 mpPinned 或配置 paths）：
// 严格按用户给定的顺序与数量使用，不做自动补齐（避免备用节点随评分乱跳）。
func (m *Monitor) applySelectedPaths(refs []string) {
	names, eps := m.resolvePaths(refs)
	if len(names) == 0 {
		m.fwd.SetMultipath(nil)
		return
	}
	m.mu.Lock()
	m.mpPaths = eps
	m.mpPathNames = names
	m.mu.Unlock()
	// 主路径：优先第 1 条，若其暂不可用则用第一条可用路径（故障时不至于把流量压到坏节点）
	primary, primaryEp := -1, ""
	for i, nm := range names {
		if rt := m.rtOf(nm); rt != nil && rt.Score != nil && rt.Score.Eligible {
			primary, primaryEp = i, eps[i]
			break
		}
	}
	if primary < 0 {
		primary, primaryEp = 0, eps[0]
	}
	m.setCurrentByName(names[primary])
	m.fwd.SetTarget(primaryEp)
	m.syncMultipath()
	logger.Infof("多线采用自选线路 %d 条：%v（模式=%s，当前=%s）",
		len(names), names, mpModeText(m.mpManual), mpOnText(m.mpOn()))
}

// userPinned 用户是否已交互自选线路（锁定当前/备用，禁止自动切换节点）
func (m *Monitor) userPinned() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.mpPinned) > 0
}

// resolvePaths 把线路引用（节点名或 host:port）解析为节点名与端点列表；
// 能匹配到节点则用其当前端点，未匹配的原样保留（视为直接端点）。
func (m *Monitor) resolvePaths(refs []string) (names, eps []string) {
	nodes := m.rm.Nodes()
	seen := make(map[string]bool, len(refs))
	add := func(name, ep string) {
		if seen[name] {
			return
		}
		seen[name] = true
		names = append(names, name)
		eps = append(eps, ep)
	}
	for _, ref := range refs {
		ref = strings.TrimSpace(ref)
		if ref == "" {
			continue
		}
		matched := false
		for j := range nodes {
			ep := fmt.Sprintf("%s:%d", nodes[j].ServerAddr, nodes[j].ServerPort)
			if nodes[j].Name == ref || ep == ref {
				if !nodes[j].IsEnabled() {
					logger.Warnf("自选线路 %s 已停用（enabled=false），已跳过", nodes[j].Name)
					seen[nodes[j].Name] = true
					matched = true
					break
				}
				add(nodes[j].Name, ep)
				matched = true
				break
			}
		}
		if !matched {
			add(ref, ref)
		}
	}
	return
}

// ---------- 多线路径质量与路径断开处理 ----------

// mpPathQuality 单条多线路径的实时质量
type mpPathQuality struct {
	RTTMs    float64
	JitterMs float64
	MaxMs    float64
	LossPct  float64
	Queue    int
	At       time.Time
}

// onPathQuality 接收 mp 层周期回报的路径质量（供状态屏展示）
func (m *Monitor) onPathQuality(addr string, rttMs, jitterMs, maxMs, lossPct float64, queue int) {
	m.mu.Lock()
	if m.mpPathQ == nil {
		m.mpPathQ = make(map[string]mpPathQuality)
	}
	m.mpPathQ[addr] = mpPathQuality{RTTMs: rttMs, JitterMs: jitterMs, MaxMs: maxMs, LossPct: lossPct, Queue: queue, At: time.Now()}
	m.mu.Unlock()
}

// onPathSpike 隧道往返尖峰：按真实会话样本计入「近 1 小时尖峰」并告警。
// 与 10 秒一次的监控探测不同，这是会话中每 500ms 一次的真实样本，能抓到瞬时尖峰。
func (m *Monitor) onPathSpike(addr string, rttMs float64) {
	m.mu.Lock()
	m.spikeTimes = append(m.spikeTimes, time.Now())
	cut := time.Now().Add(-time.Hour)
	kept := m.spikeTimes[:0]
	for _, t := range m.spikeTimes {
		if t.After(cut) {
			kept = append(kept, t)
		}
	}
	m.spikeTimes = kept
	cnt := len(m.spikeTimes)
	m.mu.Unlock()
	logger.Warnf("隧道尖峰：%s %.0fms（近 1 小时第 %d 次）", addr, rttMs, cnt)
}

// onPathDown 多线某条路径断开：短暂去重后对该节点做定向重测，确认不可用则移出候选并重排路径。
func (m *Monitor) onPathDown(addr string) {
	m.mu.Lock()
	if m.pathDownAt == nil {
		m.pathDownAt = make(map[string]time.Time)
	}
	if t, ok := m.pathDownAt[addr]; ok && time.Since(t) < 15*time.Second {
		m.mu.Unlock()
		return
	}
	m.pathDownAt[addr] = time.Now()
	m.mu.Unlock()
	name := m.nodeNameByEndpoint(addr)
	logger.Warnf("多线路径断开：%s（节点 %s），触发定向重测", addr, name)
	go m.recheckPath(name)
}

// recheckPath 定向重测单个节点：连续失败则标记不可用并重排多线路径。
func (m *Monitor) recheckPath(name string) {
	if name == "" {
		return
	}
	n := m.findNode(name)
	if n == nil {
		return
	}
	ctx := m.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	cfg := m.Cfg()
	useMC := cfg.Switching.UseMCPing()
	timeout := time.Duration(cfg.Probing.TimeoutMs) * time.Millisecond
	if useMC {
		timeout = time.Duration(cfg.Probing.MCCheck.TimeoutMs) * time.Millisecond
	}
	fail := 0
	for i := 0; i < 3; i++ {
		if _, err := prober.ProbeOnce(ctx, n, useMC, timeout); err != nil {
			fail++
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(300 * time.Millisecond):
		}
	}
	if fail >= 2 {
		m.mu.Lock()
		if rt := m.nodes[name]; rt != nil {
			rt.Status = model.StatusOffline
			rt.StatusReason = "多线路径断开，定向重测确认不可用"
			if rt.Score != nil {
				rt.Score.Eligible = false
				rt.Score.Total = 0
			}
		}
		m.mu.Unlock()
		logger.Warnf("路径节点 %s 定向重测确认不可用，已移出多线候选", name)
	} else {
		logger.Infof("路径节点 %s 定向重测仍可用，保留候选（断开的可能只是单条隧道）", name)
	}
	m.applyMultipath()
}

// findNode 按名称取生效节点（远程+本地合并后）
func (m *Monitor) findNode(name string) *config.Node {
	nodes := m.rm.Nodes()
	for i := range nodes {
		if nodes[i].Name == name {
			return &nodes[i]
		}
	}
	return nil
}

// nodeNameByEndpoint 把隧道端点 host:port 映射回节点名（找不到返回端点本身）
func (m *Monitor) nodeNameByEndpoint(addr string) string {
	nodes := m.rm.Nodes()
	for i := range nodes {
		if fmt.Sprintf("%s:%d", nodes[i].ServerAddr, nodes[i].ServerPort) == addr {
			return nodes[i].Name
		}
	}
	return addr
}

// pathQualitySnapshot 组装当前多线路径的实时质量（与 mpPathNames 对齐）
func (m *Monitor) pathQualitySnapshot() []mpPathQuality {
	m.mu.RLock()
	paths := append([]string(nil), m.mpPaths...)
	names := append([]string(nil), m.mpPathNames...)
	q := make(map[string]mpPathQuality, len(m.mpPathQ))
	for k, v := range m.mpPathQ {
		q[k] = v
	}
	m.mu.RUnlock()
	out := make([]mpPathQuality, 0, len(names))
	for i := range names {
		pq := mpPathQuality{}
		if i < len(paths) {
			if v, ok := q[paths[i]]; ok {
				pq = v
			}
		}
		out = append(out, pq)
	}
	return out
}

// mpModeText 手动模式文案
func mpModeText(manual int) string {
	switch manual {
	case 1:
		return "强制多线"
	case 2:
		return "强制单线"
	default:
		return "自适应"
	}
}

// mpOnText 多线开关文案
func mpOnText(on bool) string {
	if on {
		return "多线"
	}
	return "单线"
}

// mpOn 当前多线是否实际启用
func (m *Monitor) mpOn() bool {
	cfg := m.Cfg()
	if !cfg.Multipath.IsEnabled() {
		return false
	}
	m.mu.RLock()
	manual := m.mpManual
	engaged := m.mpEngaged
	m.mu.RUnlock()
	switch manual {
	case 1:
		return true
	case 2:
		return false
	default:
		return !cfg.Multipath.IsAdaptive() || engaged
	}
}

// setCurrentByName 按名称设置当前节点
func (m *Monitor) setCurrentByName(name string) {
	nodes := m.rm.Nodes()
	for i := range nodes {
		if nodes[i].Name == name {
			m.setCurrent(&nodes[i])
			return
		}
	}
}

// warmOn 关键小包常开冗余是否生效（单线也保留第 2 条路径；强制单线时不生效）
func (m *Monitor) warmOn() bool {
	cfg := m.Cfg()
	if !cfg.Multipath.IsEnabled() || !cfg.Multipath.CriticalAlwaysOn() {
		return false
	}
	m.mu.RLock()
	manual := m.mpManual
	m.mu.RUnlock()
	return manual != 2
}

// syncMultipath 按当前模式把多线开关同步到转发器
func (m *Monitor) syncMultipath() {
	cfg := m.Cfg()
	if !cfg.Multipath.IsEnabled() {
		m.fwd.SetMultipath(nil)
		return
	}
	m.mu.RLock()
	paths := append([]string(nil), m.mpPaths...)
	m.mu.RUnlock()
	warm := m.warmOn()
	if len(paths) > 0 && (m.mpOn() || warm) {
		m.fwd.SetMultipath(paths)
	} else {
		m.fwd.SetMultipath(nil)
	}
}

// ---------- 钩子（§8.3） ----------

// runHook 执行外部钩子命令：cmd /c <命令>，注入节点环境变量；
// 失败仅 WARN，不影响主流程。
func (m *Monitor) runHook(cmdLine string, node *config.Node, what string) {
	if strings.TrimSpace(cmdLine) == "" {
		return
	}
	logger.Infof("执行 %s 钩子……", what)
	cmd := exec.Command("cmd", "/c", cmdLine)
	if node != nil {
		cmd.Env = append(os.Environ(),
			"MC_NODE_NAME="+node.Name,
			"MC_NODE_ADDR="+node.ServerAddr,
			fmt.Sprintf("MC_NODE_PORT=%d", node.ServerPort),
			fmt.Sprintf("MC_REMOTE_PORT=%d", node.RemotePort),
		)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		logger.Warnf("%s 钩子执行失败（不影响主流程）：%v %s", what, err, tailOf(string(out)))
		return
	}
	logger.Infof("%s 钩子执行成功 %s", what, tailOf(string(out)))
}

// ---------- 优雅退出 ----------

// gracefulShutdown 优雅退出：停 frpc（若托管）→ 保存 state → 保存历史
func (m *Monitor) gracefulShutdown() {
	logger.Infof("正在优雅退出……")
	m.fwd.Stop()
	if m.Cfg().Frpc.IsEnabled() {
		m.fm.Stop()
	}
	m.saveState()
	if err := m.hist.Save(); err != nil {
		logger.Warnf("退出时保存历史数据失败：%v", err)
	}
	if m.Cfg().Frpc.IsEnabled() {
		logger.Infof("退出完成（frpc 已停止，无孤儿进程；state 已保存）")
	} else {
		logger.Infof("退出完成（state 已保存）")
	}
}

// fwdListen 返回本地转发监听地址
func (m *Monitor) fwdListen() string {
	cfg := m.Cfg()
	return fmt.Sprintf("%s:%d", cfg.Minecraft.LocalHost, cfg.Minecraft.LocalPort)
}

// ---------- 内部工具 ----------

func (m *Monitor) setCurrent(n *config.Node) {
	m.mu.Lock()
	m.current = n
	m.mu.Unlock()
}

// qualityEWMAAlpha 端到端质量 EWMA 平滑系数
const qualityEWMAAlpha = 0.3

// updateQualityEWMA 用本轮轻量探测更新「抖动/丢包」EWMA（供自适应劣化判定）。
// 抖动 = 相邻两次 RTT 差的绝对值；丢包以失败记 100%。
func (m *Monitor) updateQualityEWMA(rtt time.Duration, ok bool) {
	a := qualityEWMAAlpha
	m.mu.Lock()
	if !ok {
		m.lossEWMA = m.lossEWMA*(1-a) + 100*a
		m.mu.Unlock()
		return
	}
	ms := float64(rtt.Microseconds()) / 1000.0
	if m.rttEWMA == 0 {
		m.rttEWMA = ms
	} else {
		dev := ms - m.rttEWMA
		if dev < 0 {
			dev = -dev
		}
		m.jitterEWMA = m.jitterEWMA*(1-a) + dev*a
		m.rttEWMA = m.rttEWMA*(1-a) + ms*a
	}
	m.lossEWMA = m.lossEWMA * (1 - a) // 成功则丢包 EWMA 衰减
	m.mu.Unlock()
}

// recordRTT 记录当前节点本轮轻量探测的延迟（状态屏展示用，保留最近若干次成功值），
// 并把超过尖峰阈值的探测记为「尖峰」（近 1 小时滑动统计，用于验收 ≤N/小时）。
func (m *Monitor) recordRTT(rtt time.Duration, ok bool) {
	ms := float64(rtt.Microseconds()) / 1000.0
	var spike bool
	var cnt int
	var thr float64
	var pathInfo string
	m.mu.Lock()
	m.lastRTTOK = ok
	m.lastRTTAt = time.Now()
	if ok {
		m.rttWindow = append(m.rttWindow, rtt)
		if len(m.rttWindow) > rttWindowSize {
			m.rttWindow = m.rttWindow[len(m.rttWindow)-rttWindowSize:]
		}
		thr = m.cfg.Switching.SpikeMs
		if thr > 0 && ms > thr {
			m.spikeTimes = append(m.spikeTimes, time.Now())
			spike = true
		}
		cut := time.Now().Add(-time.Hour)
		kept := m.spikeTimes[:0]
		for _, t := range m.spikeTimes {
			if t.After(cut) {
				kept = append(kept, t)
			}
		}
		m.spikeTimes = kept
		cnt = len(m.spikeTimes)
		if spike {
			// 采集同时刻多线路径 RTT，用于区分「网络段」还是「服务端段」
			mn, mx, jm, np := 0.0, 0.0, 0.0, 0
			for _, q := range m.mpPathQ {
				if q.At.IsZero() {
					continue
				}
				r := q.RTTMs
				if np == 0 || r < mn {
					mn = r
				}
				if np == 0 || r > mx {
					mx = r
				}
				if q.JitterMs > jm {
					jm = q.JitterMs
				}
				np++
			}
			switch {
			case np == 0:
				pathInfo = "；当前单线（无多线路径对照）"
			case mx < ms/2:
				pathInfo = fmt.Sprintf("；同时刻多线路径 RTT %.0f~%.0fms/抖动≤%.0fms → 路径正常，尖峰来自【服务端 / agent→MC】", mn, mx, jm)
			default:
				pathInfo = fmt.Sprintf("；同时刻多线路径 RTT %.0f~%.0fms/抖动≤%.0fms → 路径也高，尖峰来自【网络 / 隧道】", mn, mx, jm)
			}
		}
	}
	m.mu.Unlock()
	if spike {
		logger.Warnf("端到端尖峰：%.0fms > 阈值 %.0fms（近 1 小时第 %d 次）%s", ms, thr, cnt, pathInfo)
	}
}

// spikesLastHour 近 1 小时尖峰次数
func (m *Monitor) spikesLastHour() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.spikeTimes)
}

// clearRTT 切换节点后清空延迟，避免展示旧节点的数据（显示为“测量中”）
func (m *Monitor) clearRTT() {
	m.mu.Lock()
	m.rttWindow = nil
	m.lastRTTOK = false
	m.lastRTTAt = time.Time{}
	m.mu.Unlock()
}

// rttWindowSize 实时延迟展示的滑动窗口大小（取中位数，抗单次尖峰）
const rttWindowSize = 5

func (m *Monitor) setMode(mode string) {
	m.mu.Lock()
	m.mode = mode
	m.mu.Unlock()
}

func (m *Monitor) currentName() string {
	if n := m.CurrentNode(); n != nil {
		return n.Name
	}
	return "（无）"
}

// rtOf 取节点最新测速结果（可能为 nil）
func (m *Monitor) rtOf(name string) *model.NodeRuntime {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.nodes[name]
}

// bestEligibleNode 最优可选举节点（excludeName 之外的最高分；空串表示不排除）
func (m *Monitor) bestEligibleNode(excludeName string) *config.Node {
	nodes := m.rm.Nodes()
	var best *config.Node
	bestScore := -1.0
	for i := range nodes {
		n := &nodes[i]
		if !n.IsEnabled() {
			continue
		}
		if n.Name == excludeName {
			continue
		}
		rt := m.rtOf(n.Name)
		if rt == nil || rt.Score == nil || !rt.Score.Eligible {
			continue
		}
		if rt.Score.Total > bestScore {
			bestScore = rt.Score.Total
			best = n
		}
	}
	return best
}

// joinReason 拼接多段失败原因
func joinReason(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	return a + "；" + b
}

// nameOf 节点名（nil 安全）
func nameOf(n *config.Node) string {
	if n == nil {
		return "（无）"
	}
	return n.Name
}

// remoteDesc 远程源描述（日志用）
func remoteDesc(url string) string {
	if url == "" {
		return "未配置（仅用本地节点）"
	}
	return url
}

// excludeName 返回排除名（nil 节点 → 空串）
func excludeName(n *config.Node) string {
	if n == nil {
		return ""
	}
	return n.Name
}

// keysOf map 键列表（告警展示用）
func keysOf(m map[int]bool) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// tailOf 截取钩子输出尾部（避免日志过长）
func tailOf(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 300 {
		return "……" + s[len(s)-300:]
	}
	return s
}
