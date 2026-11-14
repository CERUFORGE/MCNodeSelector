package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"mcnode/internal/config"
	"mcnode/internal/i18n"
)

// ---------- 多服务器档案 ----------
//
// 每个服务器一个独立档案目录：<exe>/servers/<名称>/{config.jsonc,data,logs}
// 一次运行一个；玩家可在启动菜单里添加/删除/选择服务器。
// 档案的 data/log 用绝对路径，互不干扰。

type serverProfile struct {
	name    string
	dir     string
	cfgPath string
	port    int
}

func profilesDir() string { return filepath.Join(config.ExeDir(), "servers") }

// ---------- 启动器设置（语言） ----------

type appSettings struct {
	Language string `json:"language"`
}

func settingsPath() string { return filepath.Join(config.ExeDir(), "settings.json") }

func loadSettings() appSettings {
	var s appSettings
	if b, err := os.ReadFile(settingsPath()); err == nil {
		_ = json.Unmarshal(b, &s)
	}
	return s
}

func saveSettings(s appSettings) {
	b, _ := json.MarshalIndent(s, "", "  ")
	_ = os.WriteFile(settingsPath(), b, 0o644)
}

// askLanguage 首次启动先选语言（中英并列，不偏向任何一方）
func askLanguage(r *bufio.Reader) string {
	fmt.Println("Language / 语言:")
	fmt.Println("  1) English")
	fmt.Println("  2) 中文")
	fmt.Print("Choose / 选择 [2]: ")
	line, _ := r.ReadString('\n')
	if strings.TrimSpace(line) == "1" {
		return "en"
	}
	return "zh"
}

// classifyTarget 判断用户输入是 URL / 本地文件 / 直连地址，并做必要规范化。
// 返回 kind = "url" | "file" | "direct"。
func classifyTarget(t string) (kind, value string) {
	t = strings.TrimSpace(t)
	if t == "" {
		return "", ""
	}
	low := strings.ToLower(t)
	switch {
	case strings.HasPrefix(low, "http://"), strings.HasPrefix(low, "https://"):
		return "url", t
	case strings.HasPrefix(low, "file://"):
		return "file", strings.TrimPrefix(t, "file://")
	}
	// 已存在的文件
	if st, err := os.Stat(t); err == nil && !st.IsDir() {
		if abs, e := filepath.Abs(t); e == nil {
			return "file", abs
		}
		return "file", t
	}
	// Windows 盘符路径（如 C:\...）
	if len(t) >= 3 && t[1] == ':' && (t[2] == '\\' || t[2] == '/') {
		if abs, e := filepath.Abs(t); e == nil {
			return "file", abs
		}
		return "file", t
	}
	// host:port（端口为数字）→ 直连
	if h, p, err := net.SplitHostPort(t); err == nil && h != "" {
		if _, e := strconv.Atoi(p); e == nil {
			return "direct", t
		}
	}
	// 含反斜杠 → Windows 路径
	if strings.Contains(t, "\\") {
		if abs, e := filepath.Abs(t); e == nil {
			return "file", abs
		}
		return "file", t
	}
	// 含正斜杠：域名/URL 路径 或 相对文件路径
	if strings.Contains(t, "/") {
		first := t
		if i := strings.IndexByte(t, '/'); i >= 0 {
			first = t[:i]
		}
		if strings.Contains(first, ".") {
			return "url", "http://" + t
		}
		if abs, e := filepath.Abs(t); e == nil {
			return "file", abs
		}
		return "file", t
	}
	// 看起来像域名（含点、无空格）→ 订阅 URL，补 http://
	if strings.Contains(t, ".") && !strings.ContainsAny(t, " \t") {
		return "url", "http://" + t
	}
	return "direct", t
}

// safeName 把服务器名变成安全的目录名（保留中英文/数字，替换 Windows 非法字符）
func safeName(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		s = "server"
	}
	repl := func(r rune) rune {
		switch r {
		case '<', '>', ':', '"', '/', '\\', '|', '?', '*':
			return '_'
		}
		if r < 0x20 {
			return '_'
		}
		return r
	}
	return strings.Map(repl, s)
}

func profilePort(cfgPath string) int {
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		return 0
	}
	var m struct {
		Minecraft struct {
			LocalPort int `json:"local_port"`
		} `json:"minecraft"`
	}
	_ = json.Unmarshal(config.StripComments(data), &m)
	return m.Minecraft.LocalPort
}

// listProfiles 扫描档案目录，并把 exe 目录下的 config.jsonc 视为「默认」档案。
func listProfiles() []serverProfile {
	var out []serverProfile
	base := profilesDir()
	if ents, err := os.ReadDir(base); err == nil {
		for _, e := range ents {
			if !e.IsDir() {
				continue
			}
			cp := filepath.Join(base, e.Name(), "config.jsonc")
			if _, err := os.Stat(cp); err != nil {
				continue
			}
			out = append(out, serverProfile{name: e.Name(), dir: filepath.Join(base, e.Name()), cfgPath: cp, port: profilePort(cp)})
		}
	}
	root := filepath.Join(config.ExeDir(), "config.jsonc")
	if _, err := os.Stat(root); err == nil {
		out = append(out, serverProfile{name: "默认", dir: config.ExeDir(), cfgPath: root, port: profilePort(root)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// createProfile 新建/覆盖一个服务器档案。target 可为 host:port（直连）或订阅 URL/文件。
func createProfile(name, target string, port int) (string, error) {
	if port <= 0 || port > 65535 {
		port = 25565
	}
	if strings.TrimSpace(name) == "" {
		// 名字留空时用「主机-端口」避免同名覆盖
		if h, p, err := net.SplitHostPort(target); err == nil {
			name = h + "-" + p
		} else {
			name = target
		}
	}
	dir := filepath.Join(profilesDir(), safeName(name))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	cp := filepath.Join(dir, "config.jsonc")

	nodes := []any{}
	remote := ""
	kind, val := classifyTarget(target)
	switch kind {
	case "url", "file":
		remote = val
	case "direct":
		host, p, err := net.SplitHostPort(val)
		if err != nil {
			return "", fmt.Errorf("地址格式无法识别（应为 host:port 或 URL/文件）：%s", target)
		}
		pn, e := strconv.Atoi(p)
		if e != nil {
			return "", fmt.Errorf("端口无效：%s", p)
		}
		nodes = append(nodes, map[string]any{
			"name": "direct", "server_addr": host, "server_port": pn,
			"remote_port": pn, "enabled": true,
		})
	default:
		return "", fmt.Errorf("请输入 服务器地址(host:port) 或 订阅 URL/文件")
	}

	m := map[string]any{
		"general": map[string]any{
			"language": i18n.Lang(), "log_level": "INFO",
			"log_dir":       filepath.Join(dir, "logs"),
			"data_dir":      filepath.Join(dir, "data"),
			"log_keep_days": 7, "single_instance": true, "all_nodes_down_wait_sec": 30,
			"auto_update_check": true, "update_repo": "",
		},
		"minecraft": map[string]any{"local_host": "127.0.0.1", "local_port": port},
		"remote":    map[string]any{"url": remote, "timeout_ms": 5000, "interval_min": 10},
		"multipath": map[string]any{"enabled": false},
		"nodes":     nodes,
	}
	out, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(cp, out, 0o644); err != nil {
		return "", err
	}
	return cp, nil
}

func findProfile(name string) *serverProfile {
	ps := listProfiles()
	for i := range ps {
		if ps[i].name == name {
			return &ps[i]
		}
	}
	return nil
}

// promptLine 读一行输入
func promptLine(r *bufio.Reader, prompt, def string) string {
	if def != "" {
		fmt.Printf("%s [%s]: ", prompt, def)
	} else {
		fmt.Printf("%s: ", prompt)
	}
	line, _ := r.ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" {
		return def
	}
	return line
}

// promptAddServer 交互式添加服务器，返回新档案配置路径。r 由调用方复用，避免缓冲丢输入。
func promptAddServer(r *bufio.Reader) string {
	fmt.Println(i18n.T("=== 添加服务器 ===", "=== Add server ==="))
	name := promptLine(r, i18n.T("服务器名称（随便起，如 朋友服）", "Server name (any, e.g. MyServer)"), "")
	target := promptLine(r, i18n.T("服务器地址 host:port 或 节点订阅URL/文件", "Server address host:port or node subscription URL/file"), "")
	if strings.TrimSpace(target) == "" {
		fmt.Println(i18n.T("未填写地址，已取消。", "No address given, cancelled."))
		return ""
	}
	portStr := promptLine(r, i18n.T("本地端口（游戏连这个端口）", "Local port (game connects here)"), "25565")
	port, _ := strconv.Atoi(portStr)
	cp, err := createProfile(name, target, port)
	if err != nil {
		fmt.Fprintln(os.Stderr, i18n.T("添加失败：", "Add failed: ")+err.Error())
		return ""
	}
	fmt.Println(i18n.T("已添加服务器：", "Server added: ") + name)
	fmt.Println(i18n.T("游戏连接 127.0.0.1:", "Connect your game to 127.0.0.1:") + strconv.Itoa(port))
	return cp
}

// promptRemoveServer 交互式删除
func promptRemoveServer(ps []serverProfile, r *bufio.Reader) {
	fmt.Println(i18n.T("要删除哪个服务器？（输入编号，回车取消）", "Remove which server? (number, Enter to cancel)"))
	for i, p := range ps {
		fmt.Printf("  %d) %s\n", i+1, p.name)
	}
	line := promptLine(r, i18n.T("编号", "number"), "")
	n, err := strconv.Atoi(line)
	if err != nil || n < 1 || n > len(ps) {
		return
	}
	p := ps[n-1]
	if p.cfgPath == filepath.Join(config.ExeDir(), "config.jsonc") {
		fmt.Println(i18n.T("「默认」档案是根目录配置，请手动删除。", "The default profile is the root config; delete manually."))
		return
	}
	if err := os.RemoveAll(p.dir); err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		return
	}
	fmt.Println(i18n.T("已删除：", "Removed: ") + p.name)
}

// runLauncher 双击入口：先选语言；0 个档案→添加；多个→菜单选择/添加/删除。
func runLauncher() {
	r := bufio.NewReader(os.Stdin)
	st := loadSettings()
	if st.Language == "" {
		st.Language = askLanguage(r)
		saveSettings(st)
	}
	i18n.Set(st.Language)

	ps := listProfiles()
	if len(ps) == 0 {
		fmt.Println(i18n.T("还没有服务器，先添加一个：", "No servers yet, add one first:"))
		cp := promptAddServer(r)
		if cp == "" {
			return
		}
		runEngine(cp, "", true)
		return
	}
	for {
		fmt.Println(i18n.T("可用的服务器：", "Available servers:"))
		for i, p := range ps {
			fmt.Printf("  %d) %-16s  127.0.0.1:%d\n", i+1, p.name, p.port)
		}
		fmt.Println(i18n.T("  [a] 添加   [d] 删除   [q] 退出", "  [a] add   [d] remove   [q] quit"))
		line := promptLine(r, i18n.T("选择", "Choose"), "")
		switch strings.ToLower(line) {
		case "q":
			return
		case "":
			if len(ps) == 1 { // 只有一台时回车直接启动
				runEngine(ps[0].cfgPath, "", true)
				return
			}
			continue
		case "a":
			if cp := promptAddServer(r); cp != "" {
				runEngine(cp, "", true)
			}
			return
		case "d":
			promptRemoveServer(ps, r)
			ps = listProfiles()
			if len(ps) == 0 {
				if cp := promptAddServer(r); cp != "" {
					runEngine(cp, "", true)
				}
				return
			}
		default:
			if n, e := strconv.Atoi(line); e == nil && n >= 1 && n <= len(ps) {
				runEngine(ps[n-1].cfgPath, "", true)
				return
			}
		}
	}
}

// flagValues 取 --flag 之后最多 n 个非 flag 参数
func flagValues(args []string, f string, n int) []string {
	for i, a := range args {
		if a == f {
			var out []string
			for j := i + 1; j < len(args) && len(out) < n; j++ {
				if strings.HasPrefix(args[j], "--") {
					break
				}
				out = append(out, args[j])
			}
			return out
		}
	}
	return nil
}

// runAddServer CLI：--add-server <名称> <地址|订阅> <端口>
func runAddServer(args []string) {
	if l := loadSettings().Language; l != "" {
		i18n.Set(l)
	}
	vals := flagValues(args, "--add-server", 3)
	if len(vals) < 2 {
		r := bufio.NewReader(os.Stdin)
		if cp := promptAddServer(r); cp != "" {
			fmt.Println(i18n.T("提示：运行 mcnode --play 或直接双击启动。", "Tip: run mcnode --play or double-click to start."))
		}
		return
	}
	name, target := vals[0], vals[1]
	port := 25565
	if len(vals) >= 3 {
		if p, e := strconv.Atoi(vals[2]); e == nil {
			port = p
		}
	}
	if _, err := createProfile(name, target, port); err != nil {
		fmt.Fprintln(os.Stderr, i18n.T("添加失败：", "Add failed: ")+err.Error())
		os.Exit(1)
	}
	fmt.Printf(i18n.T("已添加服务器 %s（本地端口 %d）。\n", "Server %s added (local port %d).\n"), name, port)
}

// runServersList CLI：--servers
func runServersList() {
	if l := loadSettings().Language; l != "" {
		i18n.Set(l)
	}
	ps := listProfiles()
	if len(ps) == 0 {
		fmt.Println(i18n.T("还没有服务器档案。用 --add-server 添加。", "No server profiles yet. Use --add-server."))
		return
	}
	fmt.Println(i18n.T("服务器档案：", "Server profiles:"))
	for i, p := range ps {
		fmt.Printf("  %d) %-16s  127.0.0.1:%d\n", i+1, p.name, p.port)
	}
}

// runPlay CLI：--play <名称>
func runPlay(name string) {
	if l := loadSettings().Language; l != "" {
		i18n.Set(l)
	}
	if name == "" {
		runLauncher()
		return
	}
	p := findProfile(name)
	if p == nil {
		fmt.Fprintln(os.Stderr, i18n.T("找不到服务器：", "Server not found: ")+name)
		os.Exit(1)
	}
	runEngine(p.cfgPath, "", true)
}

// runRemoveServer CLI：--remove-server <名称>
func runRemoveServer(name string) {
	if l := loadSettings().Language; l != "" {
		i18n.Set(l)
	}
	if name == "" {
		fmt.Fprintln(os.Stderr, i18n.T("用法：mcnode --remove-server <名称>", "Usage: mcnode --remove-server <name>"))
		os.Exit(1)
	}
	p := findProfile(name)
	if p == nil {
		fmt.Fprintln(os.Stderr, i18n.T("找不到服务器：", "Server not found: ")+name)
		os.Exit(1)
	}
	if p.cfgPath == filepath.Join(config.ExeDir(), "config.jsonc") {
		fmt.Fprintln(os.Stderr, i18n.T("「默认」档案是根目录配置，请手动删除。", "The default profile is the root config; delete manually."))
		os.Exit(1)
	}
	if err := os.RemoveAll(p.dir); err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}
	fmt.Println(i18n.T("已删除：", "Removed: ") + name)
}
