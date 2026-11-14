// Package frpc 负责 frpc 进程的全生命周期管理（F9 / §8）：
//   - 配置自动渲染：切换节点时按节点渲染 TOML 并原子写入（先 .tmp 再 rename）；
//   - 进程托管：后台启动、stdout/stderr 合并接管、逐行扫描关键词（frp ≥0.52 措辞）；
//   - 健康判定：进程存活 && 已见“登录成功” &&（无致命错误 || 已见“隧道成功”）；
//     进程活着 ≠ 隧道活着，必须基于输出关键词判定（§2.7）；
//   - 异常退出：指数退避重启 1s→2s→4s→…→上限；滑动窗口内重启数超限 → 上报切换；
//   - 退出清理：kill 并等待，绝不留孤儿 frpc 进程。
package frpc

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"mcnode/internal/config"
	"mcnode/internal/fsutil"
	"mcnode/internal/logger"
)

// EventKind frpc 事件类型
const (
	EventLoginOK        = "login_ok"        // frps 登录成功
	EventProxyOK        = "proxy_ok"        // 隧道启动成功
	EventFatal          = "fatal"           // 致命错误（认证失败/端口占用/名字冲突/超时/拒绝）
	EventRestartLimited = "restart_limited" // 滑动窗口内重启超限 → 触发通道 A
	EventExited         = "exited"          // 进程退出（含被停止）
)

// Event frpc 输出解析出的事件（经 Events 通道发给 monitor）
type Event struct {
	Kind string
	Msg  string
}

// keywordRule 输出关键词 → 事件映射（§8.2 表格；集中定义便于 frp 升级时维护）
type keywordRule struct {
	re    *regexp.Regexp
	kind  string // 事件类型
	msgCN string // 中文消息
}

// keywordRules 关键词正则表（适配 frp 0.52~0.71+ 措辞，大小写不敏感）
var keywordRules = []keywordRule{
	{regexp.MustCompile(`(?i)login to (the )?server success`), EventLoginOK, "frps 登录成功"},
	{regexp.MustCompile(`(?i)start proxy success|proxy .* (started|running)`), EventProxyOK, "隧道启动成功"},
	{regexp.MustCompile(`(?i)login to (the )?server failed|login to server error|connect to server error`), EventFatal, "frps 登录失败（token/端口/代理名错误，或 frps 拒绝连接）"},
	{regexp.MustCompile(`(?i)session shutdown`), EventFatal, "frps 关闭了会话（session shutdown：可能是 token 错误、代理名冲突或 remote_port 被占用）"},
	{regexp.MustCompile(`(?i)port already used`), EventFatal, "remote_port 已被占用（请更换端口或检查是否有残留 frpc）"},
	{regexp.MustCompile(`(?i)proxy name .+ already working|proxy .* already exists`), EventFatal, "代理名冲突（疑似 frpc 双开或同名代理已存在）"},
	{regexp.MustCompile(`(?i)i/o timeout|dial tcp.*timeout`), EventFatal, "连接 frps 超时（链路不通或丢包严重）"},
	{regexp.MustCompile(`(?i)connect: connection refused|connection refused`), EventFatal, "frps 端口拒绝连接（请检查 frps 是否运行、安全组是否放行）"},
}

// Manager frpc 进程管理器（同一时刻只托管一个节点）
type Manager struct {
	mu    sync.Mutex
	cfgFn func() *config.Config // 取最新配置（热加载）

	node *config.Node // 当前节点
	cmd  *exec.Cmd

	sawLoginOK atomic.Bool  // 已观察到“登录成功”
	sawProxyOK atomic.Bool  // 已观察到“隧道启动成功”
	fatalMsg   atomic.Value // string：最近一次致命错误（中文）
	stopping   atomic.Bool  // 正在主动停止（区分主动 Stop 与异常退出）
	startGen   atomic.Int64 // 启动代数：防止旧扫描协程写新会话状态

	backoff         time.Duration // 当前退避时长
	restarts        []time.Time   // 重启时间滑动记录
	limitedNotified bool          // 本轮超限是否已上报（防止事件刷屏）

	supDone chan struct{} // supervise 协程退出信号
	Events  chan Event    // 供 monitor 消费的事件流
}

// New 创建管理器
func New(cfgFn func() *config.Config) *Manager {
	return &Manager{
		cfgFn:  cfgFn,
		Events: make(chan Event, 64),
	}
}

// Node 当前节点（未启动时为 nil）
func (m *Manager) Node() *config.Node {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.node
}

// escapeTOML TOML 基本字符串转义（反斜杠与双引号）
func escapeTOML(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return s
}

// RenderTOML 按节点渲染 frpc 配置（§8.1 格式）
func RenderTOML(n *config.Node, mc config.Minecraft, proxyName string) string {
	var b strings.Builder
	b.WriteString("# 本文件由 MCNodeSelector 自动生成，请勿手动修改\n")
	fmt.Fprintf(&b, "serverAddr = \"%s\"\n", escapeTOML(n.ServerAddr))
	fmt.Fprintf(&b, "serverPort = %d\n", n.ServerPort)
	if strings.TrimSpace(n.User) != "" {
		fmt.Fprintf(&b, "user = \"%s\"\n", escapeTOML(n.User))
	}
	if strings.TrimSpace(n.Token) != "" {
		fmt.Fprintf(&b, "auth.token = \"%s\"\n", escapeTOML(n.Token))
	}
	b.WriteString("\n[[proxies]]\n")
	fmt.Fprintf(&b, "name = \"%s\"\n", escapeTOML(proxyName))
	b.WriteString("type = \"tcp\"\n")
	fmt.Fprintf(&b, "localIP = \"%s\"\n", escapeTOML(mc.LocalHost))
	fmt.Fprintf(&b, "localPort = %d\n", mc.LocalPort)
	fmt.Fprintf(&b, "remotePort = %d\n", n.RemotePort)
	return b.String()
}

// Start 为节点启动 frpc：渲染配置 → 原子写入 → 拉起进程 → 后台扫描输出。
// 若已有进程在跑会先停止（正常切换流程由 monitor 调 Stop+Start）。
func (m *Manager) Start(n *config.Node) error {
	cfg := m.cfgFn()
	m.mu.Lock()
	nodeChanged := m.node == nil || m.node.Name != n.Name
	m.node = n
	m.mu.Unlock()

	// 确保旧进程已完全停止
	m.Stop()

	if nodeChanged {
		// 换节点：重启计数与退避清零（新节点新账本）
		m.mu.Lock()
		m.restarts = nil
		m.backoff = 0
		m.limitedNotified = false
		m.mu.Unlock()
	}

	// 渲染并原子写入 frpc.toml（§8.1）
	workDir := cfg.FrpcWorkDir()
	tomlPath := filepath.Join(workDir, "frpc.toml")
	if err := fsutil.WriteAtomic(tomlPath, []byte(RenderTOML(n, cfg.Minecraft, cfg.Frpc.ProxyName)), 0o644); err != nil {
		return fmt.Errorf("生成 frpc 配置失败：%w", err)
	}

	// 重置健康标志（新一轮会话）
	m.sawLoginOK.Store(false)
	m.sawProxyOK.Store(false)
	m.fatalMsg.Store("")
	m.stopping.Store(false)
	gen := m.startGen.Add(1)

	done := make(chan struct{})
	m.mu.Lock()
	m.supDone = done
	m.mu.Unlock()

	go m.supervise(gen, done)
	logger.Infof("frpc 已启动：节点 %s（%s:%d，remote_port=%d）",
		n.Name, n.ServerAddr, n.ServerPort, n.RemotePort)
	return nil
}

// supervise 托管循环：启动进程 → 等待退出 → 异常退出则退避重启（§8.2）。
// gen 启动代数：Start 被再次调用后，旧循环自动退出，避免双进程。
func (m *Manager) supervise(gen int64, done chan struct{}) {
	defer close(done)
	for {
		if m.stopping.Load() || gen != m.startGen.Load() {
			return // 被主动停止或已被新一轮 Start 取代
		}
		if err := m.launchOnce(); err != nil {
			// 启动失败（如 frpc.exe 缺失）：按异常退出处理，走退避重试，绝不退出程序
			logger.Errorf("frpc 启动失败：%v", err)
			m.emit(Event{Kind: EventFatal, Msg: fmt.Sprintf("frpc 启动失败：%v", err)})
		}
		// launchOnce 返回 = 进程已退出（或启动失败）
		if m.stopping.Load() || gen != m.startGen.Load() {
			return // 主动停止，不算异常
		}
		// ---- 异常退出：记录 + 判定限流 + 指数退避重启（1→2→4→…→上限）----
		m.recordRestart()
		cfg := m.cfgFn()
		m.mu.Lock()
		if m.backoff <= 0 {
			m.backoff = time.Second
		} else {
			m.backoff *= 2
		}
		if maxB := time.Duration(cfg.Frpc.RestartBackoffMaxSec) * time.Second; maxB > 0 && m.backoff > maxB {
			m.backoff = maxB
		}
		wait := m.backoff
		limited := m.limitedNow()
		m.mu.Unlock()

		if limited {
			m.emit(Event{Kind: EventRestartLimited, Msg: fmt.Sprintf(
				"frpc 在 %d 分钟滑动窗口内重启超过 %d 次，判定该节点 frp 链路异常，触发故障切换（通道A）",
				cfg.Frpc.RestartWindowMin, cfg.Frpc.MaxRestarts)})
		}
		logger.Warnf("frpc 异常退出，%.0f 秒后重启（指数退避，当前窗口已重启 %d 次）",
			wait.Seconds(), m.RestartCount())

		// 退避等待（可被 Stop 打断，保证退出不留孤儿进程）
		if !m.sleepInterruptible(gen, wait) {
			return
		}
	}
}

// sleepInterruptible 可被停止/换代打断的等待；返回 false 表示应退出托管循环
func (m *Manager) sleepInterruptible(gen int64, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	for {
		select {
		case <-timer.C:
			return true
		case <-time.After(100 * time.Millisecond):
			if m.stopping.Load() || gen != m.startGen.Load() {
				return false
			}
		}
	}
}

// launchOnce 启动一次 frpc 进程并阻塞等待其退出。
// stdout/stderr 合并接管，逐行扫描关键词 → 事件（§8.2）。
func (m *Manager) launchOnce() error {
	cfg := m.cfgFn()
	n := m.Node()
	if n == nil {
		return fmt.Errorf("未设置节点")
	}
	binary := cfg.FrpcBinary()
	if _, err := os.Stat(binary); err != nil {
		return fmt.Errorf("找不到 frpc 可执行文件（%s）：请到 frp 官方 GitHub Releases 下载 ≥0.52 版本并放入程序目录，或在配置中修改 frpc.binary 路径", binary)
	}

	workDir := cfg.FrpcWorkDir()
	cmd := exec.Command(binary, "-c", filepath.Join(workDir, "frpc.toml"))
	cmd.Dir = workDir
	cmd.SysProcAttr = sysProcAttr() // Windows 隐藏窗口

	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw

	if err := cmd.Start(); err != nil {
		pw.Close()
		pr.Close()
		return fmt.Errorf("拉起 frpc 进程失败：%w", err)
	}

	m.mu.Lock()
	m.cmd = cmd
	m.mu.Unlock()

	// 后台逐行扫描输出（日志 DEBUG 级记录原始输出，排查神器 F12.4）
	go func() {
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			line := sc.Text()
			if line == "" {
				continue
			}
			logger.Debugf("frpc | %s", line)
			for _, r := range keywordRules {
				if r.re.MatchString(line) {
					m.onKeyword(r.kind, r.msgCN)
					break // 一行只触发一个事件
				}
			}
		}
	}()

	err := cmd.Wait()
	_ = pw.Close()
	_ = pr.Close()
	m.emit(Event{Kind: EventExited, Msg: fmt.Sprintf("frpc 进程退出：%v", err)})
	return nil
}

// onKeyword 处理关键词事件：更新健康标志 + 发事件 + 分级日志
func (m *Manager) onKeyword(kind, msg string) {
	switch kind {
	case EventLoginOK:
		m.sawLoginOK.Store(true)
		logger.Infof("frpc：%s", msg)
	case EventProxyOK:
		m.sawProxyOK.Store(true)
		logger.Infof("frpc：%s", msg)
	case EventFatal:
		m.fatalMsg.Store(msg)
		logger.Errorf("frpc：%s", msg)
	}
	m.emit(Event{Kind: kind, Msg: msg})
}

// emit 非阻塞发送事件（满了就丢弃并记日志，绝不阻塞 frpc 扫描协程）
func (m *Manager) emit(e Event) {
	select {
	case m.Events <- e:
	default:
		logger.Warnf("frpc 事件队列已满，丢弃事件：%s", e.Msg)
	}
}

// recordRestart 记录一次异常重启到滑动窗口，并裁剪过期记录（§8.2）
func (m *Manager) recordRestart() {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	window := time.Duration(m.cfgFn().Frpc.RestartWindowMin) * time.Minute
	m.restarts = append(m.restarts, now)
	kept := m.restarts[:0]
	for _, t := range m.restarts {
		if now.Sub(t) <= window {
			kept = append(kept, t)
		}
	}
	m.restarts = kept
}

// limitedNow 窗口内重启数是否已达上限（调用方须持锁）
func (m *Manager) limitedNow() bool {
	maxRestarts := m.cfgFn().Frpc.MaxRestarts
	limited := len(m.restarts) >= maxRestarts
	if limited && !m.limitedNotified {
		m.limitedNotified = true
		return true // 首次超限，上报一次
	}
	if !limited {
		m.limitedNotified = false // 窗口滑出后允许再次上报
	}
	return false
}

// RestartCount 当前滑动窗口内重启次数
func (m *Manager) RestartCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	window := time.Duration(m.cfgFn().Frpc.RestartWindowMin) * time.Minute
	n := 0
	for _, t := range m.restarts {
		if now.Sub(t) <= window {
			n++
		}
	}
	return n
}

// Healthy 健康判定（§8.2）：进程存活 && 已见“登录成功” &&（无致命错误 || 已见“隧道成功”）。
// 进程活着 ≠ 隧道活着（§2.7）——token 错/端口被占时进程仍在跑但隧道早已失效。
func (m *Manager) Healthy() bool {
	m.mu.Lock()
	cmd := m.cmd
	m.mu.Unlock()
	alive := cmd != nil && cmd.Process != nil && cmd.ProcessState == nil
	if !alive {
		return false
	}
	if !m.sawLoginOK.Load() {
		return false
	}
	fatal, _ := m.fatalMsg.Load().(string)
	if fatal != "" && !m.sawProxyOK.Load() {
		return false
	}
	return true
}

// LoginOK 是否已观察到登录成功
func (m *Manager) LoginOK() bool { return m.sawLoginOK.Load() }

// ProxyOK 是否已观察到隧道启动成功
func (m *Manager) ProxyOK() bool { return m.sawProxyOK.Load() }

// FatalErr 最近一次致命错误（无则空串）
func (m *Manager) FatalErr() string {
	s, _ := m.fatalMsg.Load().(string)
	return s
}

// WaitLoginOK 等待观察到“登录成功”（切换流程用），超时返回 false。
func (m *Manager) WaitLoginOK(ctx context.Context, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if m.sawLoginOK.Load() {
			return true
		}
		select {
		case <-ctx.Done():
			return m.sawLoginOK.Load()
		case <-time.After(200 * time.Millisecond):
		}
	}
	return m.sawLoginOK.Load()
}

// Stop 停止 frpc：cancel + Kill + 等待退出，绝不留孤儿进程（§8.2 / F15）。
// 幂等：未运行时调用无副作用。
func (m *Manager) Stop() {
	m.mu.Lock()
	cmd := m.cmd
	done := m.supDone
	m.mu.Unlock()
	if cmd == nil && done == nil {
		return
	}
	m.stopping.Store(true)

	if cmd != nil && cmd.Process != nil {
		// 先尝试优雅 Kill（frpc 无控制台交互，直接 Kill 即可）
		if err := cmd.Process.Kill(); err != nil {
			// 进程可能已退出，忽略错误
			_ = err
		}
	}
	// 等待 supervise 协程退出（含扫描协程收尾）
	if done != nil {
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			logger.Warnf("等待 frpc 退出超时（10 秒），继续执行（可能需要手动检查任务管理器）")
		}
	}
	m.mu.Lock()
	m.cmd = nil
	m.supDone = nil
	m.mu.Unlock()
	// 递增代数，令可能残留的旧 supervise 循环立即失效
	m.startGen.Add(1)
}
