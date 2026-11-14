package main

import (
	"bufio"
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"mcnode/internal/config"
	"mcnode/internal/history"
	"mcnode/internal/i18n"
	"mcnode/internal/logger"
)

// ---------- 玩家配置（零门槛） ----------

// writePlayerConfig 写一份「玩家版」最小配置：只需要订阅地址 + 端口，其余全默认。
func writePlayerConfig(cfgPath, src string, port int, lang string) error {
	if lang != "en" {
		lang = "zh"
	}
	if port <= 0 || port > 65535 {
		port = 25565
	}
	m := map[string]any{
		"general": map[string]any{
			"language": lang, "log_level": "INFO", "log_dir": "logs", "log_keep_days": 7,
			"data_dir": "data", "single_instance": true, "all_nodes_down_wait_sec": 30,
			"auto_update_check": true, "update_repo": "",
		},
		"minecraft": map[string]any{"local_host": "127.0.0.1", "local_port": port},
		"remote":    map[string]any{"url": src, "timeout_ms": 5000, "interval_min": 10},
		"multipath": map[string]any{"enabled": false},
		"nodes":     []any{},
	}
	out, _ := json.MarshalIndent(m, "", "  ")
	return os.WriteFile(cfgPath, out, 0o644)
}

// runSetup 极简向导：只问 3 个（语言、订阅地址、端口），其余默认。
func runSetup(cfgPath string) {
	r := bufio.NewReader(os.Stdin)
	ask := func(prompt, def string) string {
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
	// 先选语言，再用所选语言显示其余内容
	st := loadSettings()
	defLang := st.Language
	if defLang == "" {
		defLang = "zh"
	}
	lang := ask(i18n.T("界面语言 zh/en", "Language zh/en"), defLang)
	i18n.Set(lang)
	st.Language = lang
	saveSettings(st)

	fmt.Println(i18n.T("=== MCNodeSelector 快速配置 ===", "=== MCNodeSelector quick setup ==="))
	if _, err := os.Stat(cfgPath); err == nil {
		if strings.ToLower(ask(i18n.T("配置已存在，是否覆盖? (y/N)", "Config exists, overwrite? (y/N)"), "n")) != "y" {
			fmt.Println(i18n.T("已取消。", "Cancelled."))
			return
		}
	}
	src := ask(i18n.T("管理员给你的节点订阅地址或文件（直接粘贴，可留空）", "Node subscription URL/file from your admin (paste here, blank ok)"), "")
	portStr := ask(i18n.T("游戏连接端口", "Game port"), "25565")
	port, _ := strconv.Atoi(portStr)

	if err := writePlayerConfig(cfgPath, src, port, lang); err != nil {
		fmt.Fprintln(os.Stderr, i18n.T("写入配置失败：", "Failed to write config: ")+err.Error())
		os.Exit(1)
	}
	fmt.Println(i18n.T("完成！双击 mcnode.exe（或 start.bat）即可；游戏连 127.0.0.1:", "Done! Double-click mcnode.exe (or start.bat); connect your game to 127.0.0.1:") + strconv.Itoa(port))
	if src == "" {
		fmt.Println(i18n.T("提示：还没有节点来源，请把管理员给的订阅地址填进 config.jsonc 的 remote.url，或把 nodes.json 放到同目录。",
			"Tip: no node source yet. Put the admin's subscription URL into config.jsonc remote.url, or drop nodes.json next to the exe."))
	}
}

// runMakePlayerConfig 管理员用：生成一份「发给他人的玩家配置」。
func runMakePlayerConfig(cfgPath, src string) {
	if src == "" {
		fmt.Fprintln(os.Stderr, i18n.T("用法：mcnode --make-player-config <订阅URL|节点文件>", "Usage: mcnode --make-player-config <subscription URL|nodes file>"))
		os.Exit(1)
	}
	if err := writePlayerConfig(cfgPath, src, 25565, "zh"); err != nil {
		fmt.Fprintln(os.Stderr, i18n.T("写入失败：", "Write failed: ")+err.Error())
		os.Exit(1)
	}
	fmt.Println(i18n.T("已生成玩家配置：", "Player config written: ") + cfgPath)
	fmt.Println(i18n.T("把「mcnode.exe + 这个 config.jsonc」一起发给玩家，双击即可，无需任何设置。",
		"Ship mcnode.exe + this config.jsonc together; players just double-click, no setup."))
}

// ---------- --import 导入节点 ----------

func runImport(cfgPath, src string) {
	if src == "" {
		fmt.Fprintln(os.Stderr, i18n.T("用法：mcnode --import <文件|URL>", "Usage: mcnode --import <file|URL>"))
		os.Exit(1)
	}
	data, err := readSource(src)
	if err != nil {
		fmt.Fprintln(os.Stderr, i18n.T("读取失败：", "Read failed: ")+err.Error())
		os.Exit(1)
	}
	nodes, err := parseNodes(data)
	if err != nil {
		fmt.Fprintln(os.Stderr, i18n.T("解析失败：", "Parse failed: ")+err.Error())
		os.Exit(1)
	}
	if len(nodes) == 0 {
		fmt.Fprintln(os.Stderr, i18n.T("没有有效节点。", "No valid nodes."))
		os.Exit(1)
	}
	fmt.Printf(i18n.T("导入成功：%d 个节点\n", "Imported %d nodes\n"), len(nodes))
	for i := range nodes {
		n := &nodes[i]
		fmt.Printf("  - %s  %s:%d  remote=%d  enabled=%v\n", n.Name, n.ServerAddr, n.ServerPort, n.RemotePort, n.IsEnabled())
	}
	// 本地文件：复制到程序目录 nodes.json，方便在配置里用
	if !strings.HasPrefix(src, "http://") && !strings.HasPrefix(src, "https://") {
		dst := filepath.Join(filepath.Dir(cfgPath), "nodes.json")
		if err := os.WriteFile(dst, data, 0o644); err == nil {
			fmt.Println(i18n.T("已复制为 nodes.json，可把 remote.url 设为该文件路径。",
				"Copied to nodes.json; set remote.url to that file path."))
		}
	}
}

func httpGet(src string) ([]byte, error) {
	c := &http.Client{Timeout: 10 * time.Second}
	try := func(u string) ([]byte, error) {
		req, _ := http.NewRequest("GET", u, nil)
		req.Header.Set("User-Agent", "MCNodeSelector/"+version)
		resp, err := c.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
		}
		return io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	}
	b, err := try(src)
	if err == nil {
		return b, nil
	}
	// 协议回退：http ↔ https
	if strings.HasPrefix(src, "http://") {
		if b2, e2 := try("https://" + strings.TrimPrefix(src, "http://")); e2 == nil {
			return b2, nil
		}
	} else if strings.HasPrefix(src, "https://") {
		if b2, e2 := try("http://" + strings.TrimPrefix(src, "https://")); e2 == nil {
			return b2, nil
		}
	}
	return nil, err
}

func readSource(src string) ([]byte, error) {
	kind, val := classifyTarget(src)
	switch kind {
	case "url":
		return httpGet(val)
	case "file":
		return os.ReadFile(val)
	default:
		return nil, fmt.Errorf("这是一个单节点地址，不是节点列表；请提供 nodes.json 文件或订阅 URL")
	}
}

func parseNodes(data []byte) ([]config.Node, error) {
	data = bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF}) // 去掉可能的 UTF-8 BOM
	var arr []config.Node
	if err := json.Unmarshal(data, &arr); err == nil && len(arr) > 0 {
		return arr, nil
	}
	var obj struct {
		Nodes []config.Node `json:"nodes"`
	}
	if err := json.Unmarshal(data, &obj); err != nil {
		return nil, err
	}
	return obj.Nodes, nil
}

// ---------- 启动时静默检查更新 ----------

func silentUpdateCheck(repo string) {
	rel, err := fetchLatestRelease(repo)
	if err != nil || rel.TagName == "" {
		return
	}
	if strings.TrimPrefix(rel.TagName, "v") != strings.TrimPrefix(version, "v") {
		logger.Infof("发现新版本 %s（当前 %s）：%s", rel.TagName, version, rel.HTMLURL)
	}
}

type ghRelease struct {
	TagName string `json:"tag_name"`
	HTMLURL string `json:"html_url"`
}

func fetchLatestRelease(repo string) (ghRelease, error) {
	var rel ghRelease
	c := &http.Client{Timeout: 8 * time.Second}
	req, _ := http.NewRequest("GET", "https://api.github.com/repos/"+repo+"/releases/latest", nil)
	req.Header.Set("User-Agent", "MCNodeSelector/"+version)
	resp, err := c.Do(req)
	if err != nil {
		return rel, err
	}
	defer resp.Body.Close()
	err = json.NewDecoder(resp.Body).Decode(&rel)
	return rel, err
}

// ---------- --export 导出历史 ----------

func runExport(cfgPath, out string) {
	cfg := loadCfgOrExit(cfgPath)
	if out == "" {
		out = "history.csv"
	}
	hist := history.New(cfg.HistoryPath(), cfg.Scoring.History.UpdateAlpha, cfg.Scoring.History.NeutralScore)
	snap := hist.Snapshot()
	f, err := os.Create(out)
	if err != nil {
		fmt.Fprintln(os.Stderr, i18n.T("创建文件失败：", "Create failed: ")+err.Error())
		os.Exit(1)
	}
	defer f.Close()
	w := csv.NewWriter(f)
	_ = w.Write([]string{"node", "score", "rounds", "updated_at"})
	for name, e := range snap {
		_ = w.Write([]string{name, fmt.Sprintf("%.3f", e.Score), strconv.Itoa(e.Rounds), e.UpdatedAt.Format(time.RFC3339)})
	}
	w.Flush()
	fmt.Println(i18n.T("已导出：", "Exported: ") + out)
}

// openPath 用系统文件管理器打开路径（日志目录等）。
func openPath(p string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("explorer", p)
	case "darwin":
		cmd = exec.Command("open", p)
	default:
		cmd = exec.Command("xdg-open", p)
	}
	_ = cmd.Start()
}

// ---------- --check-update ----------

func runCheckUpdate(cfgPath string) {
	cfg := loadCfgOrExit(cfgPath)
	repo := strings.TrimSpace(cfg.General.UpdateRepo)
	if repo == "" {
		fmt.Println(i18n.T("未配置 general.update_repo（格式 owner/repo），无法检查更新。",
			"general.update_repo (owner/repo) is not set; cannot check for updates."))
		return
	}
	c := &http.Client{Timeout: 8 * time.Second}
	req, _ := http.NewRequest("GET", "https://api.github.com/repos/"+repo+"/releases/latest", nil)
	req.Header.Set("User-Agent", "MCNodeSelector/"+version)
	resp, err := c.Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, i18n.T("检查更新失败（网络）：", "Update check failed (network): ")+err.Error())
		return
	}
	defer resp.Body.Close()
	var rel struct {
		TagName string `json:"tag_name"`
		HTMLURL string `json:"html_url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		fmt.Fprintln(os.Stderr, i18n.T("检查更新失败（解析）：", "Update check failed (parse): ")+err.Error())
		return
	}
	if rel.TagName == "" {
		fmt.Println(i18n.T("未获取到版本信息。", "No release info found."))
		return
	}
	if strings.TrimPrefix(rel.TagName, "v") == strings.TrimPrefix(version, "v") {
		fmt.Printf(i18n.T("已是最新版本 %s。\n", "Already up to date: %s\n"), version)
	} else {
		fmt.Printf(i18n.T("发现新版本 %s（当前 %s）：%s\n", "New version %s available (current %s): %s\n"),
			rel.TagName, version, rel.HTMLURL)
	}
}

// ---------- --install-autostart / --uninstall-autostart ----------

func runAutostart(cfgPath string, install bool) {
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}
	switch runtime.GOOS {
	case "windows":
		const key = `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`
		if install {
			cmd := exec.Command("reg", "add", key, "/v", "MCNodeSelector", "/t", "REG_SZ", "/d", `"`+exe+`" --auto`, "/f")
			if out, err := cmd.CombinedOutput(); err != nil {
				fmt.Fprintln(os.Stderr, err.Error(), string(out))
				os.Exit(1)
			}
			fmt.Println(i18n.T("已设置开机自启（当前用户）。", "Autostart installed (current user)."))
		} else {
			cmd := exec.Command("reg", "delete", key, "/v", "MCNodeSelector", "/f")
			_ = cmd.Run()
			fmt.Println(i18n.T("已取消开机自启。", "Autostart removed."))
		}
	case "linux":
		dir := filepath.Join(os.Getenv("HOME"), ".config", "systemd", "user")
		unit := filepath.Join(dir, "mcnode.service")
		if install {
			_ = os.MkdirAll(dir, 0o755)
			body := fmt.Sprintf(`[Unit]
Description=MCNodeSelector
After=network-online.target

[Service]
ExecStart=%s --auto
Restart=always
RestartSec=5

[Install]
WantedBy=default.target
`, exe)
			if err := os.WriteFile(unit, []byte(body), 0o644); err != nil {
				fmt.Fprintln(os.Stderr, err.Error())
				os.Exit(1)
			}
			_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
			_ = exec.Command("systemctl", "--user", "enable", "--now", "mcnode").Run()
			fmt.Println(i18n.T("已安装 systemd 用户服务并启动。", "systemd user service installed and started."))
		} else {
			_ = exec.Command("systemctl", "--user", "disable", "--now", "mcnode").Run()
			_ = os.Remove(unit)
			fmt.Println(i18n.T("已移除 systemd 用户服务。", "systemd user service removed."))
		}
	default:
		fmt.Println(i18n.T("该平台暂不支持自动安装自启，请手动配置。", "Autostart is not supported on this platform; configure manually."))
	}
}
