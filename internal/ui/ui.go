// Package ui 提供两种界面（F13）：
//   - 事件流模式（默认）：滚动日志输出；
//   - watch 模式（--watch）：2 秒重绘的实时状态屏（§15），支持单字符命令
//     r=重载配置 t=立即重测 s=下一节点 a=切回自动 m=多线开关 b=选备用节点 q=退出，
//     以及数字键 1-9 直接选择对应编号的节点（0 选第 10 个）。
//
// 约束：watch 模式下日志只写文件不刷屏，屏幕只画状态（F13.3）；
// 状态屏行数随节点数动态变化（零硬编码，§15）。
package ui

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"mcnode/internal/i18n"
	"mcnode/internal/logger"
	"mcnode/internal/model"
)

// Row 状态屏表格行（由 monitor.Snapshot 填充）
type Row struct {
	Name            string // 节点名
	Status          model.NodeStatus
	AvgMs           float64 // Phase1 平均延迟
	P95Ms           float64 // Phase1 P95
	HasStats        bool    // 是否有测速数据（黑名单/禁用节点无）
	JitterMs        float64 // 抖动
	LossPct         float64 // 丢包率
	SpikePct        float64 // 尖峰率（稳健去尖峰后统计）
	MCState         string  // "OK" / "失败" / "-" / "关"
	MCRTTMs         float64 // 端到端 MC RTT 均值
	MCMaxMs         float64 // 端到端 MC RTT 最大
	HasMCRTT        bool    // 是否有端到端 MC 实测
	Score           float64 // 总分
	HasScore        bool
	Star            bool          // 当前节点打 ★
	Backup          bool          // 通道B 更优切换候选（备用节点）打 ☆
	IsPath          bool          // 多线正在使用的路径节点
	BlacklistRemain time.Duration // 黑名单剩余（>0 时展示）
}

// ScreenData 状态屏数据快照（§15 版面）
type ScreenData struct {
	CurrentName    string
	CurrentStatus  model.NodeStatus
	FrpcDesc       string // frpc 健康描述，如 "正常(隧道OK)"
	Mode           string // "自动" / "手动"
	Uptime         time.Duration
	SwitchesThisHr int
	CooldownRemain time.Duration
	Rows           []Row
	HasCurrent     bool // 是否有当前节点（全灭时 false）

	// 通道B（更优切换）备用候选展示
	BackupName   string  // 备用节点（最优的其他可选举节点）
	BackupScore  float64 // 备用节点总分
	HasBackup    bool    // 是否存在备用候选
	BackupPaused bool    // 是否因有玩家在线而暂停通道B

	// 实时状态
	OnlineConns      int       // 当前活跃玩家连接数
	HasRTT           bool      // 是否已有探测结果
	RTTOK            bool      // 探测是否成功
	RTTMs            float64   // 延迟（最近若干次成功探测的中位数，毫秒）
	RTTLastFail      bool      // 最近一次探测是否失败（可能只是瞬断）
	RTTSpark         []float64 // 当前节点最近几次 RTT（时间顺序，用于走势图）
	CooldownFrac     float64   // 冷却进度（剩余/总时长，0~1，用于进度条）
	SpikesLastHour   int       // 近 1 小时尖峰次数（端到端 RTT 超阈值）
	SpikeTargetPerHr int       // 目标尖峰数/小时
	SpikeMs          float64   // 尖峰阈值（ms）

	// 多线冗余状态
	MPEnabled bool      // 是否启用了多线功能
	MPOn      bool      // 当前是否实际在用多线
	MPManual  int       // 0=自适应 1=强制多线 2=强制单线
	MPNames   []string  // 多线使用的节点名
	MPReason  string    // 自适应进入多线时的路数判定原因
	MPPathQ   []MPPathQ // 多线各路径实时质量（与 MPNames 对齐）
	MPWarm    bool      // 关键小包常开冗余模式
	MPEngaged bool      // 自适应是否已因劣化启用（区别于常开）

	SelectingBackup bool // 是否处于「选择备用节点」模式（[b] 后按数字键）
}

// MPPathQ 多线单条路径的实时质量（状态屏展示）
type MPPathQ struct {
	Name     string
	RTTMs    float64
	JitterMs float64
	MaxMs    float64
	LossPct  float64
	Queue    int
	Has      bool
}

// UI 界面管理器
type UI struct {
	mu           sync.Mutex
	watch        bool
	dataFn       func() ScreenData
	cmdCh        chan rune
	stopCh       chan struct{}
	keyDone      chan struct{}
	restoreInput func()
	vtOK         bool
}

// New 创建界面。watch=true 进入状态屏模式；dataFn 由 monitor 提供（并发安全）。
func New(watch bool, dataFn func() ScreenData) *UI {
	return &UI{
		watch:   watch,
		dataFn:  dataFn,
		cmdCh:   make(chan rune, 16),
		stopCh:  make(chan struct{}),
		keyDone: make(chan struct{}),
	}
}

// Init 初始化控制台：启用 VT、watch 模式下关闭日志刷屏并切单字符输入。
// 返回的 revert 用于程序退出时恢复控制台状态。
func (u *UI) Init() (revert func(), err error) {
	u.vtOK = EnableVTOutput() == nil
	if !u.vtOK {
		// 降级：无颜色/无光标控制，但仍可运行（普通终端通常都支持）
		logger.Warnf("未能启用控制台 ANSI 支持，状态屏可能有残影但不影响功能")
	}
	if !u.watch {
		return func() {}, nil
	}
	// watch 模式：屏幕只画状态，日志只写文件（F13.3）
	logger.SetConsole(false)
	restore, rerr := MakeRawInput()
	u.restoreInput = restore
	if rerr != nil {
		logger.Warnf("%v，单字符命令将需要按回车提交", rerr)
	}
	// 键盘读取协程：单字符命令 → cmdCh
	go u.readKeys()
	fmt.Print("\x1b[?25l") // 隐藏光标，避免重绘闪烁
	return func() {
		u.Close()
	}, nil
}

// readKeys 循环读取单字符命令（r/t/s/a/q 与数字键 1-9/0，其余忽略）
func (u *UI) readKeys() {
	defer close(u.keyDone)
	buf := make([]byte, 1)
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil {
			return // 退出时 stdin 关闭
		}
		if n == 0 {
			continue
		}
		c := rune(buf[0])
		// 数字键 1-9 选择第 1-9 个节点，0 选择第 10 个；字母为功能命令
		isDigit := c >= '1' && c <= '9' || c == '0'
		switch c {
		case 'r', 't', 's', 'a', 'm', 'b', 'q':
			select {
			case u.cmdCh <- c:
			default: // 命令队列满则丢弃（防止连按刷屏）
			}
		default:
			if isDigit {
				select {
				case u.cmdCh <- c:
				default:
				}
			}
		}
	}
}

// Commands 返回命令通道（monitor 消费）
func (u *UI) Commands() <-chan rune { return u.cmdCh }

// Push 从外部（如托盘菜单）注入一个单字符命令（非阻塞）
func (u *UI) Push(c rune) {
	select {
	case u.cmdCh <- c:
	default:
	}
}

// StartWatchLoop 启动状态屏渲染循环（2 秒重绘，§15）
func (u *UI) StartWatchLoop() {
	if !u.watch {
		return
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logger.Errorf("状态屏渲染异常（已忽略，不影响运行）：%v", r)
			}
		}()
		u.render()                                  // 立即画一次
		t := time.NewTicker(110 * time.Millisecond) // 动画帧率（~9fps）
		defer t.Stop()
		for {
			select {
			case <-t.C:
				u.render()
			case <-u.stopCh:
				return
			}
		}
	}()
}

// Close 恢复控制台：显示光标、恢复输入模式、恢复日志控制台输出
func (u *UI) Close() {
	u.mu.Lock()
	defer u.mu.Unlock()
	select {
	case <-u.stopCh:
		return // 已关闭，幂等
	default:
		close(u.stopCh)
	}
	if u.watch {
		fmt.Print("\x1b[?25h") // 显示光标
		logger.SetConsole(true)
	}
	if u.restoreInput != nil {
		u.restoreInput()
	}
}

// ---------- 动画与图形助手 ----------

var (
	spinFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	flowFrames = []string{"●○○", "●●○", "○●●", "○○●"}
	rttBarRune = []rune("▁▂▃▄▅▆▇█")
)

// spinAt 取当前动画相位的旋转指示符
func spinAt(phase int) string {
	n := len(spinFrames)
	return spinFrames[((phase%n)+n)%n]
}

// flowAt 三点流动指示符（表示“正在运行/测速中”）
func flowAt(phase int) string {
	n := len(flowFrames)
	return flowFrames[((phase/2)%n+n)%n]
}

// pulseDot 状态圆点：正常时【常亮不闪】，异常时才闪烁（明暗交替）
func pulseDot(phase int, good bool) string {
	if good {
		return "\x1b[1;32m●\x1b[0m" // 健康：稳定绿，不闪
	}
	if (phase/4)%2 == 0 {
		return "\x1b[1;31m●\x1b[0m"
	}
	return "\x1b[2;31m●\x1b[0m"
}

// sparkline 把一组数值画成 ▁▂▃… 走势（按区间归一化，越高越慢）
func sparkline(vals []float64) string {
	if len(vals) == 0 {
		return ""
	}
	mn, mx := vals[0], vals[0]
	for _, v := range vals {
		if v < mn {
			mn = v
		}
		if v > mx {
			mx = v
		}
	}
	var sb strings.Builder
	for _, v := range vals {
		i := len(rttBarRune) / 2
		if mx > mn {
			i = int((v - mn) / (mx - mn) * float64(len(rttBarRune)-1))
		}
		if i < 0 {
			i = 0
		}
		if i >= len(rttBarRune) {
			i = len(rttBarRune) - 1
		}
		sb.WriteRune(rttBarRune[i])
	}
	return sb.String()
}

// qualityBar 3 格信号条：延迟越低越满
func qualityBar(ms float64) string {
	lv := 0
	switch {
	case ms < 60:
		lv = 3
	case ms < 90:
		lv = 2
	case ms < 140:
		lv = 1
	}
	return strings.Repeat("▇", lv) + strings.Repeat("▁", 3-lv)
}

// fracBar 进度条（frac 0~1）
func fracBar(frac float64, w int, fill, empty string) string {
	if frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	n := int(frac*float64(w) + 0.5)
	if n > w {
		n = w
	}
	return strings.Repeat(fill, n) + strings.Repeat(empty, w-n)
}

// lossColor 丢包配色：0 绿 / <10% 黄 / 否则红
func lossColor(p float64) string {
	switch {
	case p <= 0:
		return "\x1b[32m"
	case p < 10:
		return "\x1b[33m"
	default:
		return "\x1b[31m"
	}
}

// statusIcon 状态小图标（测速中为旋转动画）
func statusIcon(s model.NodeStatus, phase int) string {
	switch s {
	case model.StatusOnline:
		return "\x1b[1;32m●\x1b[0m"
	case model.StatusUnstable:
		return "\x1b[1;33m◐\x1b[0m"
	case model.StatusBlacklisted:
		return "\x1b[1;31m⊘\x1b[0m"
	case model.StatusOffline, model.StatusTimeout, model.StatusInternalErr:
		return "\x1b[1;31m○\x1b[0m"
	case model.StatusTesting:
		return "\x1b[36m" + spinAt(phase) + "\x1b[0m"
	default:
		return "\x1b[90m·\x1b[0m"
	}
}

// healthBadge 顶部健康徽标（结合当前节点状态）
func healthBadge(d ScreenData, phase int) string {
	if !d.HasCurrent {
		return "\x1b[1;33m" + spinAt(phase) + " " + i18n.T("启动中", "starting") + "\x1b[0m"
	}
	switch d.CurrentStatus {
	case model.StatusOnline:
		return "\x1b[1;32m● " + i18n.T("就绪", "ready") + "\x1b[0m"
	case model.StatusUnstable:
		return "\x1b[1;33m◐ " + i18n.T("波动", "unstable") + "\x1b[0m"
	default:
		return "\x1b[1;31m○ " + i18n.T("异常", "error") + "\x1b[0m"
	}
}

// render 绘制一帧状态屏：光标回原点后逐行覆盖，不清整屏（避免闪烁/滚动），并按状态配色。
func (u *UI) render() {
	d := u.dataFn()
	var b strings.Builder
	b.WriteString("\x1b[H") // 光标回原点，后续逐行覆盖
	line := func(s string) {
		b.WriteString(s)
		b.WriteString("\x1b[K\n") // 清除本行行尾残留
	}
	const dim = "\x1b[90m"
	const reset = "\x1b[0m"
	const bold = "\x1b[1m"
	const cyan = "\x1b[1;36m"
	sep := dim + strings.Repeat("─", 72) + reset
	phase := int(time.Now().UnixMilli() / 110)

	// 标题：品牌 + 健康徽标 + 运行脉动
	line(" " + cyan + "◆ MCNodeSelector (MCNS) v1.0" + reset +
		dim + " · " + i18n.T("Minecraft 节点选择器", "Minecraft node selector") + "   " + reset +
		healthBadge(d, phase) + "  \x1b[2;36m" + flowAt(phase) + reset)

	// 当前节点（呼吸灯 + 加粗）
	if d.HasCurrent {
		sc := statusColor(d.CurrentStatus)
		good := d.CurrentStatus == model.StatusOnline
		line(" " + dim + i18n.T("当前节点", "Current node") + " " + reset + pulseDot(phase, good) + " " + bold + d.CurrentName + reset +
			" " + sc + "[" + string(d.CurrentStatus) + "]" + reset +
			dim + "   " + i18n.T("运行", "Uptime") + " " + fmtUptime(d.Uptime) + "   frpc " + d.FrpcDesc + reset)
	} else {
		line(" \x1b[33m" + spinAt(phase) + " " + i18n.T("当前节点：正在启动，首轮测速中……", "Current node: starting, first probe in progress...") + reset)
	}

	// 状态 + 冷却进度 + 在线连接 + 端到端延迟走势
	rttTxt := spinAt(phase) + " " + i18n.T("测量中", "measuring")
	rttCol := dim
	spark := ""
	if d.HasRTT {
		if d.RTTOK {
			rttTxt = fmt.Sprintf("%.0fms", d.RTTMs)
			rttCol = rttColor(d.RTTMs)
		} else {
			rttTxt = i18n.T("探测失败", "probe failed")
			rttCol = "\x1b[31m"
		}
		spark = "\x1b[36m" + sparkline(d.RTTSpark) + reset
	}
	cd := ""
	if d.CooldownRemain > 0 {
		cd = "   " + dim + i18n.T("冷却", "cooldown") + " " + reset + "\x1b[33m[" + fracBar(1-d.CooldownFrac, 8, "█", "░") + "]" + reset + " " + fmtDur(d.CooldownRemain)
	}
	onDot := "\x1b[90m○\x1b[0m" // 无连接：灰色空心，不闪
	onCol := dim
	if d.OnlineConns > 0 {
		onDot = "\x1b[1;32m●\x1b[0m" // 有连接：常亮绿，不闪
		onCol = "\x1b[32m"
	}
	failNote := ""
	if d.RTTLastFail {
		failNote = dim + " " + i18n.T("(刚失败1次)", "(1 recent fail)") + reset
	}
	spikeCol := "\x1b[32m"
	if d.SpikesLastHour > d.SpikeTargetPerHr {
		spikeCol = "\x1b[1;31m"
	}
	spikeSeg := "   " + dim + i18n.T("近1h尖峰", "spikes 1h") + " " + reset + spikeCol + fmt.Sprintf("%d", d.SpikesLastHour) + reset +
		dim + fmt.Sprintf("/≤%d", d.SpikeTargetPerHr) + reset
	peakSeg := ""
	if len(d.MPPathQ) > 0 && d.MPPathQ[0].Has && d.SpikeMs > 0 && d.MPPathQ[0].MaxMs > d.SpikeMs {
		peakSeg = " " + dim + i18n.T("峰值", "peak") + " " + reset + "\x1b[1;31m" + fmt.Sprintf("%.0fms", d.MPPathQ[0].MaxMs) + reset
	}
	line(" " + dim + i18n.T("模式", "Mode") + " " + reset + d.Mode + "   " + dim + i18n.T("本小时切换", "switches 1h") + " " + reset + fmt.Sprintf("%d", d.SwitchesThisHr) + cd +
		"   " + dim + i18n.T("在线连接", "online") + " " + reset + onDot + onCol + fmt.Sprintf(" %d", d.OnlineConns) + reset +
		"   " + dim + i18n.T("节点延迟", "latency") + " " + reset + rttCol + rttTxt + reset + " " + spark + failNote + peakSeg + spikeSeg)

	// 备用节点
	if d.HasBackup {
		paused := ""
		if d.BackupPaused {
			paused = dim + i18n.T("（有玩家在线，已暂停更优切换）", " (player online, better-switch paused)") + reset
		}
		line(" " + dim + i18n.T("备用节点", "Backup") + " " + reset + "\x1b[36m☆ " + d.BackupName + reset +
			dim + fmt.Sprintf("  (%.1f)", d.BackupScore) + reset + paused)
	} else {
		line(" " + dim + i18n.T("备用节点 无（[b] 可选择）", "Backup none ([b] to choose)") + reset)
	}
	// [b] 备用节点选择提示
	if d.SelectingBackup {
		line(" \x1b[1;33m▶ " + i18n.T("备用节点选择中：按 [1-9]/[0] 指定备用节点，再按 [b] 取消", "Selecting backup: press [1-9]/[0], [b] to cancel") + "\x1b[0m")
	}

	// 多线冗余状态
	if d.MPEnabled {
		modeTxt := i18n.T("自适应", "Auto")
		switch d.MPManual {
		case 1:
			modeTxt = i18n.T("强制多线", "Force multi")
		case 2:
			modeTxt = i18n.T("强制单线", "Force single")
		}
		if d.MPOn {
			mpHealthy := true
			for _, q := range d.MPPathQ {
				if q.Has && (q.LossPct > 0 || q.RTTMs > 150) {
					mpHealthy = false
				}
			}
			mpDot := "\x1b[1;32m●\x1b[0m" // 健康：常亮绿，不闪
			if !mpHealthy {
				mpDot = pulseDot(phase, false) // 有路径异常才闪
			}
			label := i18n.T("已启用", "ON")
			if !d.MPEngaged && d.MPWarm {
				label = i18n.T("关键包常开冗余", "critical always-on")
			}
			line(" " + dim + i18n.T("多线冗余", "Multipath") + " " + reset + mpDot + " \x1b[1;32m" + label + reset +
				dim + fmt.Sprintf(i18n.T("   %d 路：", "   %d paths: "), len(d.MPNames)) + reset +
				"\x1b[35m↔ " + strings.Join(d.MPNames, "  ↔ ") + reset)
			if d.MPReason != "" {
				line(" " + dim + i18n.T("自适应判定（全量重测后）：", "adaptive decision (after full retest): ") + d.MPReason + reset)
			}
			if len(d.MPPathQ) > 0 {
				var sb strings.Builder
				sb.WriteString(" " + dim + i18n.T("路径质量：", "path quality: "))
				for i, q := range d.MPPathQ {
					if i > 0 {
						sb.WriteString("   ")
					}
					if !q.Has {
						sb.WriteString(q.Name + " " + spinAt(phase))
					} else {
						sb.WriteString(fmt.Sprintf("%s %s%s%s %.0fms(±%.0f %s%.0f)/%s%.0f%%%s",
							q.Name, rttColor(q.RTTMs), qualityBar(q.RTTMs), reset+dim,
							q.RTTMs, q.JitterMs, i18n.T("峰", "pk"), q.MaxMs, lossColor(q.LossPct), q.LossPct, reset))
					}
				}
				sb.WriteString(reset)
				line(sb.String())
			}
		} else {
			line(" " + dim + i18n.T("多线冗余 ○ 未启用（", "Multipath ○ OFF (") + modeTxt + i18n.T("，网络正常）", ", network normal)") + reset)
		}
	}

	line(sep)
	line(" " + dim + pad(i18n.T("序号", "#"), 4) + pad(i18n.T("节点", "Node"), 14) + reset +
		"\x1b[1;36m" + pad(i18n.T("状态", "Status"), 11) + pad(i18n.T("延迟端到端", "Latency"), 11) + pad(i18n.T("抖动", "Jitter"), 7) + pad(i18n.T("丢包", "Loss"), 6) + pad(i18n.T("尖峰", "Spike"), 6) + pad("MC", 4) + i18n.T("总分", "Score") + reset)
	line(sep)

	for i, r := range d.Rows {
		latency := "-/-"
		used := 0.0
		hasLat := false
		if r.HasMCRTT {
			latency = fmt.Sprintf("%.0f/%.0fms", r.MCRTTMs, r.MCMaxMs) // 端到端（含服务器段）
			used, hasLat = r.MCRTTMs, true
		} else if r.HasStats {
			latency = fmt.Sprintf("%.0f/%.0fms", r.AvgMs, r.P95Ms)
			used, hasLat = r.AvgMs, true
		}
		latCol := ""
		if hasLat {
			latCol = rttColor(used)
		}
		jit := "-"
		if r.HasStats {
			jit = fmt.Sprintf("%.0fms", r.JitterMs)
		}
		loss := "-"
		if r.HasStats {
			loss = fmt.Sprintf("%.0f%%", r.LossPct)
		}
		spike := "-"
		if r.HasStats {
			spike = fmt.Sprintf("%.0f%%", r.SpikePct)
		}
		score := "-"
		scoreCol := ""
		if r.HasScore {
			score = fmt.Sprintf("%.1f", r.Score)
			scoreCol = scoreColor(r.Score)
		}
		statusTxt := string(r.Status)
		if r.BlacklistRemain > 0 {
			statusTxt = fmtDur(r.BlacklistRemain)
		}
		icon := statusIcon(r.Status, phase)
		mark := ""
		add := func(s string) {
			if mark != "" {
				mark += " "
			}
			mark += s
		}
		if r.Star {
			add("\x1b[1;32m★\x1b[0m")
		}
		if r.Backup {
			add("\x1b[36m☆\x1b[0m")
		}
		if r.IsPath {
			add("\x1b[35m↔\x1b[0m")
		}
		sc := statusColor(r.Status)
		// 当前节点整行加粗高亮
		rowBold := ""
		rowBoldEnd := ""
		if r.Star {
			rowBold = "\x1b[1m"
			rowBoldEnd = "\x1b[0m"
		}
		line(" " + rowBold + dim + pad(fmt.Sprintf("[%d]", i+1), 4) + reset +
			rowBold + pad(r.Name, 14) + reset +
			icon + " " + sc + pad(statusTxt, 9) + reset +
			latCol + pad(latency, 11) + reset +
			pad(jit, 7) + pad(loss, 6) + pad(spike, 6) +
			pad(r.MCState, 4) + scoreCol + pad(score, 5) + reset + " " + mark + rowBoldEnd)
	}

	line(sep)
	line(" " + dim + i18n.T("[1-9]选节点 [0]第10 [b]选备用 [a]自动 [s]下一 [m]多线 [t]重测 [r]重载 [q]退出",
		"[1-9]node [0]10th [b]backup [a]auto [s]next [m]multi [t]retest [r]reload [q]quit") + reset)
	line(" " + dim + i18n.T("★=当前节点   ☆=备用节点   ↔=多线路径   ", "★=current   ☆=backup   ↔=multipath   ") + reset + rttColor(55) + "▇" + reset + dim + i18n.T("=信号", "=signal") + reset)

	b.WriteString("\x1b[J") // 清除下方残留
	fmt.Print(b.String())
}

// statusColor 状态配色：在线绿 / 不稳黄 / 异常红 / 其它灰
func statusColor(s model.NodeStatus) string {
	switch s {
	case model.StatusOnline:
		return "\x1b[32m"
	case model.StatusUnstable:
		return "\x1b[33m"
	case model.StatusBlacklisted, model.StatusOffline, model.StatusTimeout, model.StatusInternalErr:
		return "\x1b[31m"
	default:
		return "\x1b[90m"
	}
}

// scoreColor 总分配色：≥95 绿 / ≥85 黄 / 否则红
func scoreColor(v float64) string {
	switch {
	case v >= 95:
		return "\x1b[32m"
	case v >= 85:
		return "\x1b[33m"
	default:
		return "\x1b[31m"
	}
}

// rttColor 延迟配色：<80 绿 / <150 黄 / 否则红
func rttColor(ms float64) string {
	switch {
	case ms < 80:
		return "\x1b[32m"
	case ms < 150:
		return "\x1b[33m"
	default:
		return "\x1b[31m"
	}
}

// fmtUptime 运行时长格式化（2h13m / 5m12s / 45s）
func fmtUptime(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
}

// fmtDur 时长格式化（m 分 s 秒）
func fmtDur(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
}
