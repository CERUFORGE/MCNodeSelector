// Package diagnose 实现一键诊断（§13 / --diagnose）。
// 检查清单 10 项，输出 ✅/⚠️/❌ 三级 + 逐条修复建议。
// 诊断过程不获取单实例锁，可在主程序运行时执行（临时 frpc 登录验证会跳过当前节点以免冲突）。
package diagnose

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"mcnode/internal/config"
	"mcnode/internal/frpc"
	"mcnode/internal/lock"
	"mcnode/internal/logger"
	"mcnode/internal/mcping"
	"mcnode/internal/neterr"
)

// 三级状态图标
type icon string

const (
	pass icon = "✅"
	warn icon = "⚠️"
	fail icon = "❌"
)

// check 单条诊断结果
type check struct {
	id     int
	title  string
	icon   icon
	msg    string // 检查结果描述
	advice string // 失败时的修复建议
}

// Run 执行全部检查并打印汇总报告；返回是否有 ❌ 项（main 据此决定退出码）
func Run(cfgPath string) bool {
	fmt.Println("MCNodeSelector (MCNS) 一键诊断开始……")
	fmt.Println(strings.Repeat("─", 60))

	var report []check
	addLine := func(c check) { report = append(report, c) }

	// 检查 1：config.jsonc
	cfg, c := checkConfig(cfgPath)
	addLine(c)
	if c.icon == fail {
		// 配置不可用，后续检查无法进行
		printReport(report)
		return true
	}

	// 检查 2：frpc.exe 存在且版本 ≥0.52
	addLine(checkFrpc(cfg))

	// 检查 3：本地 MC 服务端监听
	addLine(checkLocalMC(cfg))

	// 检查 4：出网连通性（TCP 223.5.5.5:53）
	addLine(checkOutbound())

	// 检查 5：逐节点 DNS 解析
	dnsResults := map[string]string{}
	for i := range cfg.Nodes {
		n := &cfg.Nodes[i]
		if !n.IsEnabled() {
			addLine(check{id: 5, title: n.Name + "：已停用（enabled=false），跳过 DNS 检查", icon: pass})
			continue
		}
		c := checkDNS(n)
		c.title = n.Name + " DNS 解析"
		addLine(c)
		if c.icon == fail {
			dnsResults[n.Name] = c.msg
		}
	}

	// 检查 6：逐节点 TCP 探测（区分拒绝/超时）
	for i := range cfg.Nodes {
		n := &cfg.Nodes[i]
		if !n.IsEnabled() {
			addLine(check{id: 6, title: n.Name + "：已停用，跳过 TCP 探测", icon: pass})
			continue
		}
		if dnsResults[n.Name] != "" {
			addLine(check{id: 6, title: n.Name + " TCP 探测：DNS 已失败，跳过", icon: warn, advice: "先修复 DNS"})
			continue
		}
		addLine(checkTCP(n))
	}

	// 检查 9：单实例锁（提前做，供 7/8 判定是否跳过当前节点）
	pid, alive := lock.Check(cfg.LockPath())
	lockRunning := alive && pid != 0
	currentNode := ""
	if lockRunning {
		// 读取 state.json 的当前节点，避免临时 frpc 与之冲突
		if data, err := os.ReadFile(cfg.StatePath()); err == nil {
			if s := strings.Index(string(data), `"current_node":`); s >= 0 {
				s += len(`"current_node":`) + 1
				e := strings.IndexByte(string(data)[s:], '"')
				if e >= 0 {
					currentNode = string(data)[s : s+e]
				}
			}
		}
		addLine(check{id: 9, title: "单实例锁", icon: pass,
			msg: fmt.Sprintf("已有实例在运行（PID %d），当前节点 %s；临时 frpc 登录验证将跳过该节点以免端口冲突",
				pid, currentNode)})
	} else {
		addLine(check{id: 9, title: "单实例锁", icon: pass, msg: "无运行中的实例，锁可用"})
	}

	// 检查 7：逐节点临时 frpc 登录验证（跳过运行中实例的当前节点）
	for i := range cfg.Nodes {
		n := &cfg.Nodes[i]
		if !n.IsEnabled() {
			addLine(check{id: 7, title: n.Name + "：已停用，跳过 frpc 登录验证", icon: pass})
			continue
		}
		if lockRunning && n.Name == currentNode && currentNode != "" {
			addLine(check{id: 7, title: n.Name + " frpc 登录验证：运行中实例正占用该节点，跳过以免冲突", icon: warn})
			continue
		}
		addLine(checkFrpcLogin(cfg, n, currentNode))
	}

	// 检查 8：当前/候选节点公网映射 MC ping
	for i := range cfg.Nodes {
		n := &cfg.Nodes[i]
		if !n.IsEnabled() {
			continue
		}
		if lockRunning && n.Name == currentNode && currentNode != "" {
			addLine(check{id: 8, title: n.Name + " MC ping：运行中实例占用该节点端口，跳过", icon: warn})
			continue
		}
		addLine(checkMCPing(cfg, n, currentNode))
	}

	// 检查 10：汇总报告
	hasFail := printReport(report)
	return hasFail
}

// ---------- 检查 1：config.jsonc ----------
func checkConfig(cfgPath string) (*config.Config, check) {
	if _, err := os.Stat(cfgPath); err != nil {
		return nil, check{id: 1, title: "配置文件", icon: fail,
			msg:    "未找到 config.jsonc，建议首次运行程序自动生成模板后再编辑",
			advice: "直接运行 mcnode.exe（不加参数）会自动生成模板"}
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, check{id: 1, title: "配置文件", icon: fail, msg: err.Error(), advice: "按提示修改后重试"}
	}
	return cfg, check{id: 1, title: "配置文件", icon: pass,
		msg: fmt.Sprintf("config.jsonc 正常，共 %d 个节点（启用 %d 个）", len(cfg.Nodes), len(cfg.EnabledNodes()))}
}

// ---------- 检查 2：frpc.exe ----------
var versionReg = regexp.MustCompile(`(\d+)\.(\d+)\.?(\d*)`)

func checkFrpc(cfg *config.Config) check {
	bin := cfg.FrpcBinary()
	if _, err := os.Stat(bin); err != nil {
		return check{id: 2, title: "frpc 可执行文件", icon: fail,
			msg:    fmt.Sprintf("找不到 %s", bin),
			advice: "到 frp 官方 GitHub Releases 下载 ≥0.52.0 的 Windows amd64 版，放入程序目录或在配置 frpc.binary 指定路径"}
	}
	// 尝试解析版本
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "-v").CombinedOutput()
	if err != nil {
		return check{id: 2, title: "frpc 可执行文件", icon: warn,
			msg:    fmt.Sprintf("找到 %s，但无法获取版本号：%v", bin, err),
			advice: "建议确认版本 ≥0.52.0"}
	}
	m := versionReg.FindStringSubmatch(strings.TrimSpace(string(out)))
	if m == nil {
		return check{id: 2, title: "frpc 可执行文件", icon: warn,
			msg:    fmt.Sprintf("找到 %s，版本输出无法识别：%s", bin, strings.TrimSpace(string(out))),
			advice: "建议确认版本 ≥0.52.0"}
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	if major < 0 || (major == 0 && minor < 52) {
		return check{id: 2, title: "frpc 可执行文件", icon: fail,
			msg:    fmt.Sprintf("frpc 版本 %s.%s 过低，要求 ≥0.52.0", m[1], m[2]),
			advice: "请到 GitHub Releases 下载 ≥0.52.0 版本（frpc 使用 TOML 配置格式）"}
	}
	return check{id: 2, title: "frpc 可执行文件", icon: pass,
		msg: fmt.Sprintf("%s 版本 %s.%s.%s（≥0.52.0 ✓）", filepath.Base(bin), m[1], m[2], m[3])}
}

// ---------- 检查 3：本地 MC 服务端 ----------
func checkLocalMC(cfg *config.Config) check {
	addr := net.JoinHostPort(cfg.Minecraft.LocalHost, fmt.Sprintf("%d", cfg.Minecraft.LocalPort))
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return check{id: 3, title: "本地 MC 服务端", icon: fail,
			msg:    fmt.Sprintf("未监听 %s：%v", addr, err),
			advice: "请先启动 Minecraft 服务端（本地 127.0.0.1:25565），再运行本工具"}
	}
	conn.Close()
	// 进一步用 SLP 验证是否真的是 MC
	res, err := mcping.Ping(cfg.Minecraft.LocalHost, cfg.Minecraft.LocalPort, 3*time.Second)
	if err != nil || res == nil {
		return check{id: 3, title: "本地 MC 服务端", icon: warn,
			msg:    fmt.Sprintf("端口 %s 有服务监听，但 SLP 握手失败：%v（可能仍在启动中）", addr, err),
			advice: "等待服务端完全启动"}
	}
	return check{id: 3, title: "本地 MC 服务端", icon: pass,
		msg: fmt.Sprintf("%s 是 Minecraft 服务端（版本 %s，在线 %d/%d）", addr, res.VersionName, res.PlayersOnline, res.PlayersMax)}
}

// ---------- 检查 4：出网连通性 ----------
func checkOutbound() check {
	conn, err := net.DialTimeout("tcp", "223.5.5.5:53", 3*time.Second)
	if err != nil {
		return check{id: 4, title: "出网连通性", icon: fail,
			msg:    fmt.Sprintf("TCP 223.5.5.5:53 连接失败：%v", err),
			advice: "本机可能断网，或防火墙拦截了出站 TCP"}
	}
	conn.Close()
	return check{id: 4, title: "出网连通性", icon: pass, msg: "TCP 223.5.5.5:53 可连接"}
}

// ---------- 检查 5：DNS ----------
func checkDNS(n *config.Node) check {
	addrs, err := net.LookupHost(n.ServerAddr)
	if err != nil {
		return check{id: 5, icon: fail, msg: fmt.Sprintf("DNS 解析失败：%v", err),
			advice: "检查域名拼写，或本机 DNS 设置"}
	}
	if len(addrs) == 0 {
		return check{id: 5, icon: fail, msg: "DNS 解析返回空", advice: "检查域名拼写"}
	}
	return check{id: 5, icon: pass, msg: fmt.Sprintf("解析到 %s", strings.Join(addrs, ", "))}
}

// ---------- 检查 6：TCP 探测 ----------
func checkTCP(n *config.Node) check {
	host, port := n.ProbeAddr()
	addr := net.JoinHostPort(host, fmt.Sprintf("%d", port))
	start := time.Now()
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	rtt := time.Since(start)
	if err != nil {
		class, msg := neterr.Classify(err)
		advice := ""
		switch class {
		case neterr.ClassRefused:
			advice = "frps 可能未运行；云安全组/防火墙未放行 bindPort"
		case neterr.ClassTimeout:
			advice = "线路不通或丢包严重"
		default:
			advice = msg
		}
		return check{id: 6, title: n.Name + " TCP 探测", icon: fail, msg: msg, advice: advice}
	}
	conn.Close()
	return check{id: 6, title: n.Name + " TCP 探测", icon: pass,
		msg: fmt.Sprintf("%s 连接成功（RTT %.0fms）", addr, float64(rtt.Microseconds())/1000)}
}

// ---------- 检查 7：临时 frpc 登录验证 ----------
func checkFrpcLogin(cfg *config.Config, n *config.Node, currentNode string) check {
	if _, err := os.Stat(cfg.FrpcBinary()); err != nil {
		return check{id: 7, title: n.Name + " frpc 登录验证", icon: warn,
			msg: "frpc.exe 不存在，跳过", advice: "先安装 frpc ≥0.52"}
	}
	// 用临时代理名 + 临时工作目录，避免与主实例冲突
	tmpDir := filepath.Join(cfg.FrpcWorkDir(), "diag-"+n.Name)
	proxyName := "mcnode-diag"
	tomlPath := filepath.Join(tmpDir, "frpc.toml")
	content := frpc.RenderTOML(n, cfg.Minecraft, proxyName)
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return check{id: 7, title: n.Name + " frpc 登录验证", icon: fail, msg: err.Error()}
	}
	if err := os.WriteFile(tomlPath, []byte(content), 0o644); err != nil {
		return check{id: 7, title: n.Name + " frpc 登录验证", icon: fail, msg: err.Error()}
	}
	defer os.RemoveAll(tmpDir)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, cfg.FrpcBinary(), "-c", tomlPath)
	cmd.Dir = tmpDir
	cmd.SysProcAttr = nil
	pr, pw, err := os.Pipe()
	if err != nil {
		return check{id: 7, title: n.Name + " frpc 登录验证", icon: fail, msg: err.Error()}
	}
	cmd.Stdout = pw
	cmd.Stderr = pw
	if err := cmd.Start(); err != nil {
		pw.Close()
		pr.Close()
		return check{id: 7, title: n.Name + " frpc 登录验证", icon: fail,
			msg: fmt.Sprintf("启动 frpc 失败：%v", err)}
	}
	// 逐行扫描，见到 login success / login failed 即判定
	result := make(chan string, 1)
	go func() {
		buf := make([]byte, 4096)
		seen := ""
		for {
			nr, err := pr.Read(buf)
			if nr > 0 {
				seen += string(buf[:nr])
				logger.Debugf("diag frpc | %s", string(buf[:nr]))
				if strings.Contains(seen, "login to server success") {
					result <- "ok"
					return
				}
				if strings.Contains(seen, "login to server failed") ||
					strings.Contains(seen, "login to server error") {
					result <- "fail:frps 登录失败（token 或端口错误）"
					return
				}
				if strings.Contains(seen, "port already used") {
					result <- "fail:remote_port 已被占用"
					return
				}
				if strings.Contains(seen, "i/o timeout") || strings.Contains(seen, "connection refused") {
					result <- "fail:连接 frps 超时或拒绝"
					return
				}
			}
			if err != nil {
				result <- "fail:frpc 提前退出（未观察到登录结果）"
				return
			}
		}
	}()

	var status string
	select {
	case s := <-result:
		status = s
	case <-time.After(15 * time.Second):
		status = "fail:15 秒内未观察到登录结果（可能 frps 不可达或 token 错误）"
	}
	cmd.Process.Kill()
	cmd.Wait()
	pw.Close()
	pr.Close()

	if strings.HasPrefix(status, "ok") {
		return check{id: 7, title: n.Name + " frpc 登录验证", icon: pass, msg: "frps 登录成功（token/端口正确）"}
	}
	msg := strings.TrimPrefix(status, "fail:")
	return check{id: 7, title: n.Name + " frpc 登录验证", icon: fail, msg: msg, advice: "检查 token、server_port、frps 状态"}
}

// ---------- 检查 8：MC ping ----------
func checkMCPing(cfg *config.Config, n *config.Node, currentNode string) check {
	host, port := n.MCAddr()
	res, err := mcping.Ping(host, port, 5*time.Second)
	if err != nil || res == nil {
		_, msg := neterr.Classify(err)
		if msg == "" {
			msg = fmt.Sprintf("%v", err)
		}
		return check{id: 8, title: n.Name + " MC ping", icon: fail, msg: msg,
			advice: "若未启动 frpc 隧道则正常（remote_port 上无隧道）；启动主程序后隧道建立即可 ping 通"}
	}
	return check{id: 8, title: n.Name + " MC ping", icon: pass,
		msg: fmt.Sprintf("版本 %s，在线 %d/%d（RTT %.0fms）", res.VersionName, res.PlayersOnline, res.PlayersMax, res.RTTMs)}
}

// ---------- 检查 10：汇总报告 ----------
func printReport(report []check) bool {
	fmt.Println(strings.Repeat("─", 60))
	for _, c := range report {
		fmt.Printf("%s [%d] %s\n", c.icon, c.id, c.title)
		if c.msg != "" {
			fmt.Printf("     %s\n", c.msg)
		}
		if c.advice != "" {
			fmt.Printf("     修复建议：%s\n", c.advice)
		}
		fmt.Println()
	}
	// 统计
	p, w, f := 0, 0, 0
	for _, c := range report {
		switch c.icon {
		case pass:
			p++
		case warn:
			w++
		case fail:
			f++
		}
	}
	fmt.Println(strings.Repeat("─", 60))
	fmt.Printf("汇总：%d 通过  %d 警告  %d 失败\n", p, w, f)
	if f > 0 {
		fmt.Println("存在失败项，请按上述修复建议逐一处理后重试。")
	} else if w > 0 {
		fmt.Println("存在警告项，可先尝试启动；若异常请回到诊断排查。")
	} else {
		fmt.Println("全部通过，可以启动 mcnode --auto。")
	}
	return f > 0
}
