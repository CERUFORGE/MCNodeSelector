// Package main 是 MCNodeSelector 的命令行入口（§11）。
// 顶层 recover 兜底：任何未捕获 panic → 中文提示后退出码 1（绝不闪退，F15）。
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"mcnode/internal/config"
	"mcnode/internal/diagnose"
	"mcnode/internal/history"
	"mcnode/internal/i18n"
	"mcnode/internal/lock"
	"mcnode/internal/logger"
	"mcnode/internal/model"
	"mcnode/internal/monitor"
	"mcnode/internal/mp"
	"mcnode/internal/prober"
	"mcnode/internal/remotenodes"
	"mcnode/internal/tray"
	"mcnode/internal/ui"
)

// 版本号（含 frp 适配版本说明，§11 --version）
const (
	version   = "v1.0"
	buildTime = "2026-11-15"

	// appWindowTitle 控制台窗口标题：第二个实例据此找到并唤醒已运行的实例
	appWindowTitle = "MCNodeSelector (MCNS) - 节点选择器"
)

// logPanic 把崩溃信息追加写入程序目录下的 panic.log（便于双击闪退后排查）
func logPanic(r any) {
	path := filepath.Join(config.ExeDir(), "panic.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "\n===== %s =====\npanic: %v\n%s\n",
		time.Now().Format("2006-01-02 15:04:05"), r, debug.Stack())
}

func main() {
	// 顶层 recover：兜底所有未捕获 panic，写 panic.log + 弹窗提示后退出码 1（绝不静默闪退）
	defer func() {
		if r := recover(); r != nil {
			logPanic(r)
			ui.MessageBox("MCNodeSelector 崩溃",
				fmt.Sprintf("程序发生内部错误：%v\n\n详细信息已写入程序目录的 panic.log，请把它发给开发者。", r))
			fmt.Fprintf(os.Stderr, "\x1b[31m程序发生内部错误：%v\x1b[0m\n", r)
			os.Exit(1)
		}
	}()

	// Windows 控制台：先切 UTF-8 代码页，再启用 VT 处理，让中文与颜色正常显示
	ui.SetUTF8Console()
	_ = ui.EnableVTOutput()

	args := os.Args[1:]
	// 无参数（双击）：进入多服务器启动菜单（添加/选择服务器）
	if len(args) == 0 {
		runLauncher()
		return
	}

	// 确定配置文件路径：优先 --config，否则程序目录下的 config.jsonc
	cfgPath := flagValue(args, "--config")
	if cfgPath == "" {
		cfgPath = filepath.Join(config.ExeDir(), "config.jsonc")
	}

	// --version / --help 不需要配置（--help 会尝试读取语言设置）
	if hasFlag(args, "--version") {
		fmt.Printf("MCNodeSelector (MCNS) %s\n", version)
		fmt.Println("可选托管 frpc 需 frp ≥ 0.52.0（TOML 配置）；普通玩家无需 frpc")
		fmt.Printf("构建日期：%s\n", buildTime)
		return
	}
	if hasFlag(args, "--help") || hasFlag(args, "-h") {
		if _, statErr := os.Stat(cfgPath); statErr == nil {
			if c, err := config.Load(cfgPath); err == nil {
				i18n.Set(c.General.Lang())
			}
		}
		printHelp()
		return
	}

	// --diagnose / --list / --status / --history 需要配置但不需要单实例锁
	switch {
	case hasFlag(args, "--diagnose"):
		if hadFail := diagnose.Run(cfgPath); hadFail {
			os.Exit(1)
		}
		return
	case hasFlag(args, "--list"):
		runList(cfgPath)
		return
	case hasFlag(args, "--status"):
		runStatus(cfgPath)
		return
	case hasFlag(args, "--history"):
		runHistory(cfgPath)
		return
	case hasFlag(args, "--setup"):
		runSetup(cfgPath)
		return
	case hasFlag(args, "--servers"):
		runServersList()
		return
	case hasFlag(args, "--play"):
		runPlay(flagValue(args, "--play"))
		return
	case hasFlag(args, "--add-server"):
		runAddServer(args)
		return
	case hasFlag(args, "--remove-server"):
		runRemoveServer(flagValue(args, "--remove-server"))
		return
	case hasFlag(args, "--import"):
		runImport(cfgPath, flagValue(args, "--import"))
		return
	case hasFlag(args, "--make-player-config"):
		runMakePlayerConfig(cfgPath, flagValue(args, "--make-player-config"))
		return
	case hasFlag(args, "--export"):
		runExport(cfgPath, flagValue(args, "--export"))
		return
	case hasFlag(args, "--check-update"):
		runCheckUpdate(cfgPath)
		return
	case hasFlag(args, "--install-autostart"):
		runAutostart(cfgPath, true)
		return
	case hasFlag(args, "--uninstall-autostart"):
		runAutostart(cfgPath, false)
		return
	}

	// --agent 服务端多路冗余 agent（配合客户端 multipath 使用）
	if hasFlag(args, "--agent") {
		runAgent(cfgPath)
		return
	}

	// --test 全量测速报告（需要锁，写 history）
	if hasFlag(args, "--test") {
		runTest(cfgPath)
		return
	}

	// --auto / --node：常驻模式，需要单实例锁
	auto := hasFlag(args, "--auto")
	nodeName := flagValue(args, "--node")
	watch := hasFlag(args, "--watch")
	if !auto && nodeName == "" {
		fmt.Fprintln(os.Stderr, "参数无法识别。使用 mcnode --help 查看完整用法。")
		os.Exit(1)
	}
	// 玩家首次双击：没有 config.jsonc 也没有 nodes.json → 自动进入极简向导
	if _, err := os.Stat(cfgPath); os.IsNotExist(err) {
		nj := filepath.Join(filepath.Dir(cfgPath), "nodes.json")
		if _, e := os.Stat(nj); e != nil {
			if fi, e2 := os.Stdin.Stat(); e2 == nil && (fi.Mode()&os.ModeCharDevice) != 0 {
				runSetup(cfgPath)
			}
		}
	}
	runEngine(cfgPath, nodeName, watch)
}

// hasFlag 是否包含某 flag
func hasFlag(args []string, f string) bool {
	for _, a := range args {
		if a == f {
			return true
		}
	}
	return false
}

// flagValue 取 --flag value 形式的值
func flagValue(args []string, f string) string {
	for i, a := range args {
		if a == f && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// loadCfgOrExit 加载配置；首次运行生成模板时提示后退出 0
func loadCfgOrExit(cfgPath string) *config.Config {
	// 玩家零配置：没有 config.jsonc 但同目录有 nodes.json（管理员提供）→ 自动生成配置
	if _, err := os.Stat(cfgPath); os.IsNotExist(err) {
		nj := filepath.Join(filepath.Dir(cfgPath), "nodes.json")
		if _, e := os.Stat(nj); e == nil {
			if abs, e2 := filepath.Abs(nj); e2 == nil {
				if werr := writePlayerConfig(cfgPath, abs, 25565, "zh"); werr == nil {
					fmt.Println("已检测到 nodes.json，自动完成配置（无需手动设置）。")
				}
			}
		}
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		if errors.Is(err, config.ErrFirstRun) {
			// 首次运行：给出最简单的两条路，不要求玩家编辑 JSON
			fmt.Println("═══════════════════════════════════════════════════════════")
			fmt.Println("首次运行，还差一步（二选一即可）：")
			fmt.Println("  ① 管理员给了 nodes.json → 把它放到本程序同目录，重新打开即可（自动配置）")
			fmt.Println("  ② 运行  mcnode.exe --setup  按提示填「订阅地址 + 端口」即可")
			fmt.Printf("配置文件位置：%s\n", cfgPath)
			fmt.Println("═══════════════════════════════════════════════════════════")
			os.Exit(0)
		}
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}
	i18n.Set(cfg.General.Lang())
	return cfg
}

// ---------- --auto / --node ----------

func runEngine(cfgPath, manualNode string, watch bool) {
	cfg := loadCfgOrExit(cfgPath)
	mp.SetClientToken(cfg.Multipath.AgentToken)

	// 手动模式校验节点名存在
	if manualNode != "" {
		n := cfg.FindNode(manualNode)
		if n == nil {
			fmt.Fprintf(os.Stderr, "错误：指定的节点 “%s” 不存在。可用节点：\n", manualNode)
			for i := range cfg.Nodes {
				x := &cfg.Nodes[i]
				fmt.Fprintf(os.Stderr, "  - %s（%s:%d，remote_port=%d）\n", x.Name, x.ServerAddr, x.ServerPort, x.RemotePort)
			}
			os.Exit(1)
		}
		if !n.IsEnabled() {
			fmt.Fprintf(os.Stderr, "错误：节点 “%s” 已停用（enabled=false）。请先在配置中启用它。\n", manualNode)
			os.Exit(1)
		}
	}

	// 单实例锁（F15）
	var release func()
	if cfg.General.IsSingleInstance() {
		rel, err := lock.Acquire(cfg.LockPath())
		if err != nil {
			fmt.Fprintln(os.Stderr, err.Error())
			if ui.FocusExistingWindow(appWindowTitle) {
				os.Exit(0) // 已有实例在跑：把它拉到前台，本实例静默退出
			}
			ui.MessageBox("MCNodeSelector 已在运行", err.Error()+"\n\n如果看不到它的窗口，请在系统托盘（右下角）找到图标，双击显示或右键退出。")
			os.Exit(1)
		}
		release = rel
	}
	// 唯一实例负责设置窗口标题（第二个实例据此找到并唤醒本实例）
	ui.SetConsoleTitle(appWindowTitle)
	defer func() {
		if release != nil {
			release()
		}
	}()

	// 初始化日志（按配置）
	logger.Init(logger.ParseLevel(cfg.General.LogLevel), cfg.LogDir(), cfg.General.LogKeepDays, !watch)
	defer logger.Close()
	mp.Debugf = logger.Debugf
	mp.Warnf = logger.Warnf
	for i := range cfg.Nodes {
		logger.RegisterSecret(cfg.Nodes[i].Token)
	}

	logger.Infof("MCNodeSelector %s 启动（模式=%s，watch=%v）", version, modeText(manualNode), watch)
	if len(cfg.Nodes) == 0 && strings.TrimSpace(cfg.Remote.URL) == "" {
		logger.Warnf("配置中没有节点（nodes 为空且 remote.url 为空）：将进入等待，请用 --setup 或 --import 添加节点")
	}

	// 启动时静默检查更新（不阻塞、不影响运行）
	if cfg.General.UpdateCheckEnabled() && cfg.General.UpdateRepo != "" {
		go silentUpdateCheck(cfg.General.UpdateRepo)
	}

	// 信号：Ctrl-C 优雅退出
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	m := monitor.New(cfgPath, cfg, manualNode, watch)
	u := ui.New(watch, m.Snapshot)
	m.AttachUI(u)
	revert, err := u.Init()
	if err != nil {
		logger.Warnf("界面初始化告警（不影响运行）：%v", err)
	}
	defer revert()

	// 系统托盘（仅 watch 模式）：可隐藏窗口到托盘 + 桌面通知；失败不影响主流程。
	// onExit 绑定到信号 ctx 的 stop：托盘菜单“退出”等价于 Ctrl-C 优雅退出。
	if watch {
		if tr, terr := tray.Start(i18n.T("MCNodeSelector (MCNS) 运行中（双击显示/隐藏窗口，右键菜单）", "MCNodeSelector (MCNS) running (double-click to show/hide, right-click menu)"), stop); terr == nil {
			tr.SetCommandHandler(func(cmd string) {
				if cmd == "logs" {
					openPath(cfg.LogDir())
					return
				}
				if cmd != "" {
					u.Push(rune(cmd[0]))
				}
			})
			defer tr.Stop()
			m.SetNotifier(tr)
			logger.Infof("系统托盘已就绪：双击托盘图标显示/隐藏窗口，右键菜单可操作")
		} else {
			logger.Warnf("系统托盘不可用（不影响运行）：%v", terr)
		}
	}

	if err := m.Run(ctx); err != nil {
		logger.Errorf("主循环异常退出：%v", err)
		os.Exit(1)
	}
}

func modeText(manualNode string) string {
	if manualNode != "" {
		return "手动（" + manualNode + "）"
	}
	return "自动"
}

// ---------- --agent（服务端多路冗余 agent） ----------

// runAgent 运行服务端 agent：监听多个客户端路径连接，去重按序后转发到本地 MC 服务端。
func runAgent(cfgPath string) {
	cfg := loadCfgOrExit(cfgPath)
	logger.Init(logger.ParseLevel(cfg.General.LogLevel), cfg.LogDir(), cfg.General.LogKeepDays, true)
	defer logger.Close()
	mp.Debugf = logger.Debugf
	mp.Warnf = logger.Warnf

	listen := cfg.Multipath.AgentListen
	if listen == "" {
		listen = "127.0.0.1:7001"
	}
	mc := cfg.Multipath.AgentMC
	if mc == "" {
		mc = "127.0.0.1:25565" // MC 服务端标准端口（服务端本地）
	}
	mp.SetAgentToken(cfg.Multipath.AgentToken)
	mp.SetAgentPassthrough(cfg.Multipath.AgentPassthroughOn())
	if cfg.Multipath.AgentToken == "" {
		logger.Warnf("agent 未设置 multipath.agent_token：公网部署时任何人都能连入并转发到你的 MC，建议立即设置")
	}
	if cfg.Multipath.AgentPassthroughOn() {
		logger.Infof("agent 允许原始透传（MC 状态 ping 可直接连 agent 端口）；如需仅多线，设置 multipath.agent_passthrough=false")
	} else {
		logger.Infof("agent 已关闭原始透传：只接受带 token 的多线会话（请把 mc_check 指向 MC 公网端口）")
	}
	logger.Infof("多路冗余 agent 启动：监听 %s → 本地 MC %s", listen, mc)
	logger.Infof("提示：请在 frpc 里把该监听端口映射到各节点，并在客户端 multipath.paths 填写这些节点地址")
	if err := mp.RunAgent(listen, mc); err != nil {
		fmt.Fprintln(os.Stderr, "agent 启动失败："+err.Error())
		os.Exit(1)
	}
}

// ---------- --test ----------

func runTest(cfgPath string) {
	cfg := loadCfgOrExit(cfgPath)
	// --test 也写 history，需单实例锁
	if cfg.General.IsSingleInstance() {
		rel, err := lock.Acquire(cfg.LockPath())
		if err != nil {
			fmt.Fprintln(os.Stderr, err.Error())
			if ui.FocusExistingWindow(appWindowTitle) {
				os.Exit(0)
			}
			ui.MessageBox("MCNodeSelector 已在运行", err.Error())
			os.Exit(1)
		}
		defer rel()
	}
	logger.Init(logger.ParseLevel(cfg.General.LogLevel), cfg.LogDir(), cfg.General.LogKeepDays, true)
	defer logger.Close()
	for i := range cfg.Nodes {
		logger.RegisterSecret(cfg.Nodes[i].Token)
	}

	logger.Infof("--test 全量测速开始（%d 个本地节点）", len(cfg.EnabledNodes()))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	rm := remotenodes.New(func() *config.Config { return cfg })
	nodes := rm.Refresh()

	hist := history.New(cfg.HistoryPath(), cfg.Scoring.History.UpdateAlpha, cfg.Scoring.History.NeutralScore)
	p := prober.New(cfg, hist, nodes)
	res := p.RunAll(ctx, nil)

	// 历史更新
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
		hist.Update(n.Name, x, offline)
	}
	if err := hist.Save(); err != nil {
		logger.Warnf("保存历史数据失败：%v", err)
	}

	printTestReport(cfg, nodes, res)
}

// printTestReport 打印测速报告表格（含各分项分数与中文原因，F4.3）
func printTestReport(cfg *config.Config, nodes []config.Node, res map[string]*model.NodeRuntime) {
	fmt.Println(strings.Repeat("═", 100))
	fmt.Println("MCNodeSelector (MCNS) 全量测速报告")
	fmt.Println(strings.Repeat("─", 100))
	fmt.Printf("%-16s %-10s %-12s %-10s %-8s %-8s %-8s %-8s %s\n",
		"节点", "状态", "avg/p95ms", "抖动", "丢包", "MC", "历史", "总分", "原因")
	fmt.Println(strings.Repeat("─", 100))
	hist := history.New(cfg.HistoryPath(), cfg.Scoring.History.UpdateAlpha, cfg.Scoring.History.NeutralScore)
	for i := range nodes {
		n := &nodes[i]
		rt, ok := res[n.Name]
		if !ok {
			continue
		}
		lat := "-/-"
		if rt.Stats != nil && rt.Stats.Received > 0 {
			lat = fmt.Sprintf("%.0f/%.0f", rt.Stats.AvgMs, rt.Stats.P95Ms)
		}
		jit := "-"
		if rt.Stats != nil && rt.Stats.Received > 0 {
			jit = fmt.Sprintf("%.0f", rt.Stats.JitterMs)
		}
		loss := "-"
		if rt.Stats != nil {
			loss = fmt.Sprintf("%.0f%%", rt.Stats.LossPct)
		}
		mc := "-"
		if rt.MC != nil {
			if rt.MC.OK {
				mc = fmt.Sprintf("%.0fms", rt.MC.AvgRTTMs)
			} else {
				mc = "失败"
			}
		} else if !cfg.Probing.MCCheck.IsEnabled() {
			mc = "关"
		}
		histScore := hist.Get(n.Name)
		total := "-"
		reason := rt.StatusReason
		if rt.Score != nil {
			total = fmt.Sprintf("%.1f", rt.Score.Total)
			reason = rt.Score.Reason
		}
		fmt.Printf("%-16s %-10s %-12s %-10s %-8s %-8s %-8.0f %-8s %s\n",
			n.Name, rt.Status, lat, jit, loss, mc, histScore, total, reason)
	}
	fmt.Println(strings.Repeat("═", 100))
}

// ---------- --list ----------

func runList(cfgPath string) {
	cfg := loadCfgOrExit(cfgPath)
	// --list 也尝试拉取远程节点（合并展示）
	rm := remotenodes.New(func() *config.Config { return cfg })
	nodes := rm.Refresh()
	fmt.Println(strings.Repeat("═", 80))
	fmt.Println("节点列表（远程+本地合并）")
	fmt.Println(strings.Repeat("─", 80))
	fmt.Printf("%-16s %-10s %-22s %-10s %-10s %-8s %s\n",
		"名称", "状态", "地址", "端口", "远程端口", "权重", "区域/标签")
	fmt.Println(strings.Repeat("─", 80))
	for i := range nodes {
		n := &nodes[i]
		status := "启用"
		if !n.IsEnabled() {
			status = "停用"
		}
		tags := n.Region
		if len(n.Tags) > 0 {
			if tags != "" {
				tags += "/"
			}
			tags += strings.Join(n.Tags, ",")
		}
		fmt.Printf("%-16s %-10s %-22s %-10d %-10d %-8.1f %s\n",
			n.Name, status, n.ServerAddr, n.ServerPort, n.RemotePort, n.Weight, tags)
	}
	fmt.Println(strings.Repeat("═", 80))
}

// ---------- --status ----------

func runStatus(cfgPath string) {
	cfg := loadCfgOrExit(cfgPath)
	fmt.Println(strings.Repeat("═", 60))
	fmt.Println("MCNodeSelector (MCNS) 运行状态")
	fmt.Println(strings.Repeat("─", 60))
	pid, alive := lock.Check(cfg.LockPath())
	if alive {
		fmt.Printf("进程状态：运行中（PID %d）\n", pid)
	} else if pid != 0 {
		fmt.Printf("进程状态：未运行（存在陈旧锁，PID %d 已退出）\n", pid)
	} else {
		fmt.Println("进程状态：未运行")
	}
	if data, err := os.ReadFile(cfg.StatePath()); err == nil {
		fmt.Printf("上次状态：%s\n", strings.TrimSpace(string(data)))
	} else {
		fmt.Println("（无 state.json，可能从未运行）")
	}
	if data, err := os.ReadFile(cfg.HistoryPath()); err == nil {
		fmt.Println(strings.Repeat("─", 60))
		fmt.Println("历史数据：")
		fmt.Println(string(data))
	}
	fmt.Println(strings.Repeat("═", 60))
}

// ---------- --history ----------

func runHistory(cfgPath string) {
	cfg := loadCfgOrExit(cfgPath)
	hist := history.New(cfg.HistoryPath(), cfg.Scoring.History.UpdateAlpha, cfg.Scoring.History.NeutralScore)
	rm := remotenodes.New(func() *config.Config { return cfg })
	nodes := rm.Refresh()
	fmt.Println(strings.Repeat("═", 60))
	fmt.Println("节点历史评分")
	fmt.Println(strings.Repeat("─", 60))
	fmt.Printf("%-16s %-10s %-10s %s\n", "节点", "历史分", "记录轮数", "最近更新")
	fmt.Println(strings.Repeat("─", 60))
	snap := hist.Snapshot()
	for i := range nodes {
		n := &nodes[i]
		e, ok := snap[n.Name]
		if !ok {
			fmt.Printf("%-16s %-10s %-10s %s\n", n.Name, "-", "0", "（无记录）")
			continue
		}
		fmt.Printf("%-16s %-10.1f %-10d %s\n", n.Name, e.Score, e.Rounds, e.UpdatedAt.Format("2006-01-02 15:04:05"))
	}
	fmt.Println(strings.Repeat("═", 60))
}

// ---------- --help ----------

func printHelp() {
	fmt.Println(i18n.T(`MCNodeSelector (MCNS) —— Minecraft FRP 节点自动选择器

用法：
  mcnode                       等价 --auto --watch（双击默认入口，可用数字键手动选节点）
  mcnode --auto                自动模式：测速→选优→frpc→监控→自动切换
  mcnode --auto --watch        自动模式 + 实时状态屏
  mcnode --node <节点名>        手动指定节点启动（仍受监控与故障切换保护）
  mcnode --setup               交互式配置向导（首次使用推荐）
  mcnode --servers             列出所有服务器档案
  mcnode --add-server <名称> <地址|订阅> <端口>   添加一个服务器
  mcnode --play <名称>         启动指定服务器
  mcnode --remove-server <名称> 删除服务器档案
  mcnode --import <文件|URL>    导入/校验节点列表（本地文件或订阅 URL）
  mcnode --make-player-config <URL|文件>  生成「玩家零配置」版 config.jsonc（管理员用）
  mcnode --test                全量测速并打印报告表格
  mcnode --list                列出全部节点
  mcnode --status              当前节点、frpc 健康、运行时长、切换统计
  mcnode --history             各节点历史分与记录轮数
  mcnode --export <文件.csv>    导出历史评分为 CSV
  mcnode --check-update        检查是否有新版本
  mcnode --install-autostart   安装开机自启
  mcnode --uninstall-autostart 取消开机自启
  mcnode --diagnose            一键诊断
  mcnode --agent               服务端多路冗余 agent（配合客户端 multipath 使用）
  mcnode --config <文件>       指定配置文件（默认 exe 同目录的 config.jsonc）
  mcnode --version             版本号
  mcnode --help                本帮助

watch 状态屏单字符命令：
  [1-9] 选择第 1-9 个节点  [0] 选择第 10 个节点
  [s] 下一个节点   [a] 切回自动并重新选优   [t] 立即重测   [r] 重载配置   [q] 退出
  [m] 多线开关（自适应/强制多线/强制单线）  [b] 选择备用节点

评分算法：分项（延迟/P95/抖动/丢包/尖峰/MC/历史）线性归一化后加权求和，再乘节点权重。
切换防抖：双通道（故障切换 A / 更优切换 B），B 通道需过四道闸门
（最小驻留、冷却、连续优势、每小时上限）。详见 README。`, `MCNodeSelector (MCNS) — automatic Minecraft FRP node selector

Usage:
  mcnode                       same as --auto --watch (double-click default; number keys select nodes)
  mcnode --auto                auto mode: probe -> pick best -> frpc -> monitor -> auto switch
  mcnode --auto --watch        auto mode + live status screen
  mcnode --node <name>         start with a fixed node (still monitored / failover protected)
  mcnode --setup               interactive setup wizard (recommended for first run)
  mcnode --servers             list server profiles
  mcnode --add-server <name> <addr|subscription> <port>   add a server
  mcnode --play <name>         start a server profile
  mcnode --remove-server <name> remove a server profile
  mcnode --import <file|URL>   import/validate a node list (local file or subscription URL)
  mcnode --make-player-config <URL|file>  generate a zero-setup config.jsonc for players (admin)
  mcnode --test                run a full speed test and print a report table
  mcnode --list                list all nodes
  mcnode --status              current node, frpc health, uptime, switch stats
  mcnode --history             per-node history score and rounds
  mcnode --export <file.csv>   export history scores as CSV
  mcnode --check-update        check for a newer release
  mcnode --install-autostart   install start-on-boot
  mcnode --uninstall-autostart remove start-on-boot
  mcnode --diagnose            one-click diagnostics
  mcnode --agent               server-side multipath agent (used with client multipath)
  mcnode --config <file>       use a specific config file (default: config.jsonc next to the exe)
  mcnode --version             version
  mcnode --help                this help

watch-mode single-key commands:
  [1-9] select node 1-9   [0] select 10th
  [s] next node   [a] back to auto   [t] retest now   [r] reload config   [q] quit
  [m] multipath mode (auto / force multi / force single)   [b] choose backup node

Scoring: each factor (latency/P95/jitter/loss/spike/MC/history) is linearly normalized,
weighted-summed, then multiplied by node weight.
Anti-flap: channel A (failure) / channel B (better), B needs four gates
(min dwell, cooldown, consecutive wins, hourly cap). See README.`))
}
