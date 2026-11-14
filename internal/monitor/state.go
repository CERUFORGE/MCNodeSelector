// state.json 持久化与状态屏数据快照（§10 / §15）。
// state.json 记录当前节点名、上次切换时间、模式；程序启动时据此恢复；
// 损坏自愈策略与 history.json 相同：备份 *.corrupt.<ts> 后重建，仅 WARN。
package monitor

import (
	"encoding/json"
	"os"
	"sort"
	"time"

	"mcnode/internal/fsutil"
	"mcnode/internal/i18n"
	"mcnode/internal/logger"
	"mcnode/internal/model"
	"mcnode/internal/ui"
)

// stateFile state.json 结构
type stateFile struct {
	CurrentNode   string    `json:"current_node"`   // 当前节点名
	Mode          string    `json:"mode"`           // 自动 / 手动
	LastSwitch    time.Time `json:"last_switch"`    // 上次切换时间
	TotalSwitches int       `json:"total_switches"` // 累计切换次数
	SavedAt       time.Time `json:"saved_at"`       // 保存时间
}

// saveState 保存运行状态（原子写，切换与退出时调用）
func (m *Monitor) saveState() {
	cfg := m.Cfg()
	d := stateFile{
		CurrentNode:   m.currentName(),
		Mode:          modeText(m.Mode()),
		LastSwitch:    time.Now(),
		TotalSwitches: m.sw.TotalSwitches(),
		SavedAt:       time.Now(),
	}
	data, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		logger.Warnf("序列化运行状态失败（不影响运行）：%v", err)
		return
	}
	if err := fsutil.WriteAtomic(cfg.StatePath(), data, 0o644); err != nil {
		logger.Warnf("保存运行状态失败（不影响运行）：%v", err)
	}
}

// loadState 启动时恢复上次运行状态（损坏自动备份重建，F10/§10）
func (m *Monitor) loadState() {
	cfg := m.Cfg()
	raw, err := os.ReadFile(cfg.StatePath())
	if err != nil {
		if !os.IsNotExist(err) {
			logger.Warnf("读取运行状态失败（%s）：%v，忽略旧状态", cfg.StatePath(), err)
		}
		return
	}
	var d stateFile
	if err := json.Unmarshal(raw, &d); err != nil {
		if backup, berr := fsutil.BackupCorrupt(cfg.StatePath()); berr == nil {
			logger.Warnf("运行状态文件损坏，已自动备份为 %s 并忽略（不影响运行）", backup)
		} else {
			logger.Warnf("运行状态文件损坏且备份失败（%v），已忽略", berr)
		}
		return
	}
	m.savedNode = d.CurrentNode
	if d.Mode == "手动" {
		// 手动模式不跨重启恢复（重启后默认自动，除非 --node 指定）
		logger.Infof("上次运行为手动模式（节点 %s），本次默认自动模式", d.CurrentNode)
	}
	if d.CurrentNode != "" {
		logger.Infof("已读取上次运行状态：节点 %s（%s 保存）", d.CurrentNode, d.SavedAt.Format("2006-01-02 15:04:05"))
	}
}

// Snapshot 生成状态屏数据快照（ui 每 2 秒调用一次，§15）
func (m *Monitor) Snapshot() ui.ScreenData {
	cfg := m.Cfg()
	cur := m.CurrentNode()
	nodes := m.rm.Nodes()

	m.mu.RLock()
	nodesCopy := make(map[string]*model.NodeRuntime, len(m.nodes))
	for k, v := range m.nodes {
		nodesCopy[k] = v
	}
	m.mu.RUnlock()

	d := ui.ScreenData{
		CurrentName:    m.currentName(),
		CurrentStatus:  model.StatusTesting,
		FrpcDesc:       "未知",
		Mode:           modeText(m.Mode()),
		Uptime:         time.Since(m.startTime),
		SwitchesThisHr: m.sw.SwitchesLastHour(),
		CooldownRemain: m.sw.CooldownRemain(),
		HasCurrent:     cur != nil,
	}

	// frpc 健康描述
	if !cfg.Frpc.IsEnabled() {
		d.FrpcDesc = "未托管"
	} else {
		switch {
		case m.fm.Healthy() && m.fm.ProxyOK():
			d.FrpcDesc = "正常(隧道OK)"
		case m.fm.Healthy():
			d.FrpcDesc = "正常(登录OK)"
		case m.fm.FatalErr() != "":
			d.FrpcDesc = "异常(" + shortStr(m.fm.FatalErr(), 18) + ")"
		default:
			d.FrpcDesc = "启动中/重试中"
		}
	}
	if cur == nil {
		d.FrpcDesc = "未运行"
	}

	if rt, ok := nodesCopy[m.currentName()]; ok && cur != nil {
		d.CurrentStatus = rt.Status
	}

	// 节点行（按配置顺序，动态行数）
	mcEnabled := cfg.Probing.MCCheck.IsEnabled()
	// 多线冗余状态
	d.MPEnabled = cfg.Multipath.IsEnabled()
	d.MPOn = m.mpOn() || m.warmOn()
	d.MPWarm = m.warmOn()
	m.mu.RLock()
	d.MPManual = m.mpManual
	d.SelectingBackup = m.selectingBackup
	d.MPReason = m.mpReason
	d.MPEngaged = m.mpEngaged
	mpNames := append([]string(nil), m.mpPathNames...)
	m.mu.RUnlock()
	d.MPNames = mpNames
	// 多线各路径实时质量（与 mpNames 对齐，仅在实际启用多线时采集）
	if d.MPEnabled && d.MPOn && len(mpNames) > 0 {
		pq := m.pathQualitySnapshot()
		d.MPPathQ = make([]ui.MPPathQ, len(mpNames))
		for i, nm := range mpNames {
			item := ui.MPPathQ{Name: nm}
			if i < len(pq) {
				item.RTTMs = pq[i].RTTMs
				item.JitterMs = pq[i].JitterMs
				item.MaxMs = pq[i].MaxMs
				item.LossPct = pq[i].LossPct
				item.Queue = pq[i].Queue
				item.Has = !pq[i].At.IsZero()
			}
			d.MPPathQ[i] = item
		}
	}
	// 备用节点：多线启用时取多线第 2 条路径（与多线、自选同步）。
	// 若处于自选模式（交互锁定或配置 paths）且只选了一条，则不显示备用（不自动挑，避免乱跳）。
	selected := len(cfg.Multipath.Paths) > 0 || m.userPinned()
	switch {
	case cfg.Multipath.IsEnabled() && len(mpNames) >= 2:
		d.HasBackup = true
		d.BackupName = mpNames[1]
		if rt := m.rtOf(mpNames[1]); rt != nil && rt.Score != nil {
			d.BackupScore = rt.Score.Total
		}
	case cfg.Multipath.IsEnabled() && selected:
		// 自选模式且未选备用：保持空
	case cur != nil:
		if b := m.bestEligibleNode(cur.Name); b != nil {
			d.HasBackup = true
			d.BackupName = b.Name
			if rt := m.rtOf(b.Name); rt != nil && rt.Score != nil {
				d.BackupScore = rt.Score.Total
			}
		}
	}
	// 有玩家在线且配置开启时，通道B 会被暂停，状态屏标注
	d.BackupPaused = cfg.Switching.PauseWhenActive() && m.fwd.ActiveConns() > 0
	// 实时状态：在线连接数 + 当前节点延迟（取最近成功探测的中位数，抗抖）
	d.OnlineConns = m.fwd.ActiveConns()
	m.mu.RLock()
	at := m.lastRTTAt
	okLast := m.lastRTTOK
	win := append([]time.Duration(nil), m.rttWindow...)
	m.mu.RUnlock()
	if len(win) > 0 {
		d.HasRTT = true
		d.RTTOK = true
		d.RTTMs = float64(medianDuration(win).Microseconds()) / 1000.0
	} else if !at.IsZero() {
		d.HasRTT = true
		d.RTTOK = okLast
	}
	d.RTTLastFail = !at.IsZero() && !okLast
	d.SpikesLastHour = m.spikesLastHour()
	d.SpikeTargetPerHr = cfg.Switching.SpikeTargetPerHour
	d.SpikeMs = cfg.Switching.SpikeMs
	if len(win) > 0 {
		d.RTTSpark = make([]float64, len(win))
		for i, v := range win {
			d.RTTSpark[i] = float64(v.Microseconds()) / 1000.0
		}
	}
	if d.CooldownRemain > 0 && cfg.Switching.CooldownMin > 0 {
		total := time.Duration(cfg.Switching.CooldownMin) * time.Minute
		d.CooldownFrac = float64(d.CooldownRemain) / float64(total)
	}
	for i := range nodes {
		n := &nodes[i]
		row := ui.Row{Name: n.Name, Status: model.StatusTesting, MCState: "-"}
		if !n.IsEnabled() {
			row.Status = model.StatusDisabled
			row.MCState = "-"
		}
		if rt, ok := nodesCopy[n.Name]; ok {
			row.Status = rt.Status
			if rt.Stats != nil && rt.Stats.Received > 0 {
				row.HasStats = true
				row.AvgMs = rt.Stats.AvgMs
				row.P95Ms = rt.Stats.P95Ms
				row.JitterMs = rt.Stats.JitterMs
				row.LossPct = rt.Stats.LossPct
				row.SpikePct = rt.Stats.SpikePct
			}
			if rt.Score != nil {
				row.HasScore = true
				row.Score = rt.Score.Total
			}
			if rt.MC != nil {
				if rt.MC.OK {
					row.MCState = "OK"
					row.HasMCRTT = true
					row.MCRTTMs = rt.MC.AvgRTTMs
					row.MCMaxMs = rt.MC.MaxRTTMs
				} else {
					row.MCState = "失败"
				}
			} else if !mcEnabled {
				row.MCState = "关"
			}
			row.BlacklistRemain = rt.BlacklistRemain()
		}
		if cur != nil && n.Name == cur.Name {
			row.Star = true
			d.HasCurrent = true
		}
		if d.HasBackup && n.Name == d.BackupName {
			row.Backup = true
		}
		if d.MPOn {
			for _, pn := range mpNames {
				if pn == n.Name {
					row.IsPath = true
					break
				}
			}
		}
		d.Rows = append(d.Rows, row)
	}
	return d
}

// shortStr 截断过长字符串（状态屏列宽有限）
func shortStr(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// modeText 把内部模式（自动/手动）翻译为当前语言
func modeText(m string) string {
	if m == "手动" {
		return i18n.T("手动", "Manual")
	}
	return i18n.T("自动", "Auto")
}

// medianDuration 返回时长切片的中位数（副本排序，不改动入参）
func medianDuration(ds []time.Duration) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	cp := append([]time.Duration(nil), ds...)
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
	n := len(cp)
	if n%2 == 1 {
		return cp[n/2]
	}
	return (cp[n/2-1] + cp[n/2]) / 2
}
