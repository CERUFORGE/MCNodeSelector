// Package logger 实现分级日志（F12）：
//   - DEBUG / INFO / WARN / ERROR 四级，配置文件设置；
//   - 按天滚动到 logs/mcnode-YYYY-MM-DD.log，超期自动清理；
//   - token 自动脱敏（保留前 4 后 4 字符，不足 8 位则全打码）；
//   - watch 模式下可关闭控制台输出（屏幕只画状态，日志只写文件）。
package logger

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Level 日志级别
type Level int

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
)

// ParseLevel 解析配置中的日志级别字符串（非法值回退 INFO 并提示）
func ParseLevel(s string) Level {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "DEBUG":
		return LevelDebug
	case "INFO":
		return LevelInfo
	case "WARN":
		return LevelWarn
	case "ERROR":
		return LevelError
	default:
		return LevelInfo
	}
}

// String 级别文本
func (l Level) String() string {
	switch l {
	case LevelDebug:
		return "DEBUG"
	case LevelInfo:
		return "INFO"
	case LevelWarn:
		return "WARN"
	case LevelError:
		return "ERROR"
	default:
		return "INFO"
	}
}

// ANSI 颜色（Windows 10 1809+ 在启用 VT 处理后可用，main 中统一启用）
const (
	colorDebug = "\x1b[90m" // 灰
	colorInfo  = "\x1b[0m"  // 默认
	colorWarn  = "\x1b[33m" // 黄
	colorError = "\x1b[31m" // 红
	colorReset = "\x1b[0m"
)

// Logger 全局日志器（进程内单例使用，见文件底部包装函数）
type Logger struct {
	mu       sync.Mutex
	level    Level
	dir      string
	keepDays int
	console  bool
	f        *os.File
	curDay   string // 当前日志文件对应日期 YYYY-MM-DD
	secrets  []string
}

// std 全局默认日志器；未 Init 前仅输出到控制台（级别 INFO）
var std = &Logger{level: LevelInfo, console: true}

// Init 初始化全局日志器：打开/创建日志目录，清理过期日志
func Init(level Level, dir string, keepDays int, console bool) {
	std.mu.Lock()
	defer std.mu.Unlock()
	if std.f != nil {
		std.f.Close()
		std.f = nil
	}
	std.level = level
	std.dir = dir
	std.keepDays = keepDays
	std.console = console
	_ = os.MkdirAll(dir, 0o755)
	std.openLocked()  // 打开今天的日志文件
	std.cleanLocked() // 清理过期日志
}

// SetConsole 动态开关控制台输出（进入 watch 模式时关闭，退出时恢复）
func SetConsole(on bool) {
	std.mu.Lock()
	std.console = on
	std.mu.Unlock()
}

// SetLevel 热加载后动态调整日志级别
func SetLevel(l Level) {
	std.mu.Lock()
	std.level = l
	std.mu.Unlock()
}

// RegisterSecret 登记需要脱敏的敏感串（如各节点 token）。
// 之后任何日志行中出现该串都会被自动打码，防止 token 泄漏到日志。
func RegisterSecret(s string) {
	if strings.TrimSpace(s) == "" {
		return
	}
	std.mu.Lock()
	defer std.mu.Unlock()
	// 去重
	for _, old := range std.secrets {
		if old == s {
			return
		}
	}
	std.secrets = append(std.secrets, s)
}

// ResetSecrets 清空已登记的敏感串（配置热加载时重建）
func ResetSecrets() {
	std.mu.Lock()
	std.secrets = nil
	std.mu.Unlock()
}

// MaskToken token 脱敏：长度 > 8 时保留前 4 后 4，否则全打码（F12.3）
func MaskToken(s string) string {
	if len(s) > 8 {
		return s[:4] + "****" + s[len(s)-4:]
	}
	if len(s) == 0 {
		return ""
	}
	return "****"
}

// Close 关闭日志文件（程序退出时调用）
func Close() {
	std.mu.Lock()
	defer std.mu.Unlock()
	if std.f != nil {
		std.f.Close()
		std.f = nil
	}
}

// openLocked 打开/滚动到今天的日志文件（调用方须持锁）
func (l *Logger) openLocked() {
	today := time.Now().Format("2006-01-02")
	if l.f != nil && today == l.curDay {
		return // 无需滚动
	}
	if l.f != nil {
		l.f.Close()
		l.f = nil
	}
	if l.dir == "" {
		return
	}
	path := filepath.Join(l.dir, "mcnode-"+today+".log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		// 日志文件打不开绝不 panic：降级为仅控制台，并提示一次
		fmt.Fprintf(os.Stderr, "[logger] 无法打开日志文件 %s：%v（日志将仅输出到控制台）\n", path, err)
		return
	}
	l.f = f
	l.curDay = today
}

// logNameReg 用于识别本工具的日志文件（清理过期时用）
var logNameReg = regexp.MustCompile(`^mcnode-(\d{4}-\d{2}-\d{2})\.log$`)

// cleanLocked 删除超过保留天数的旧日志（调用方须持锁）
func (l *Logger) cleanLocked() {
	if l.dir == "" || l.keepDays <= 0 {
		return
	}
	entries, err := os.ReadDir(l.dir)
	if err != nil {
		return
	}
	deadline := time.Now().AddDate(0, 0, -l.keepDays).Format("2006-01-02")
	var doomed []string
	for _, e := range entries {
		m := logNameReg.FindStringSubmatch(e.Name())
		if m == nil || !e.Type().IsRegular() {
			continue
		}
		if m[1] < deadline { // 日期字符串比较即时间先后比较
			doomed = append(doomed, e.Name())
		}
	}
	sort.Strings(doomed)
	for _, name := range doomed {
		_ = os.Remove(filepath.Join(l.dir, name))
	}
}

// write 写一条日志（级别、格式化、脱敏、滚动、双通道输出）
func (l *Logger) write(level Level, format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if level < l.level {
		return // 级别不够，直接丢弃
	}
	msg := format
	if len(args) > 0 {
		msg = fmt.Sprintf(format, args...)
	}
	// 脱敏：把已登记的 token 原文替换为打码形式
	for _, s := range l.secrets {
		if strings.Contains(msg, s) {
			msg = strings.ReplaceAll(msg, s, MaskToken(s))
		}
	}
	line := fmt.Sprintf("%s [%s] %s", time.Now().Format("2006-01-02 15:04:05"), level.String(), msg)
	l.openLocked() // 每次写入前检查是否需要跨天滚动
	if l.f != nil {
		_, _ = l.f.WriteString(line + "\n")
	}
	if l.console {
		color := colorInfo
		switch level {
		case LevelDebug:
			color = colorDebug
		case LevelWarn:
			color = colorWarn
		case LevelError:
			color = colorError
		}
		fmt.Fprintln(os.Stdout, color+line+colorReset)
	}
}

// ---------- 以下为全局便捷函数（业务代码直接调用） ----------

// Debugf 调试日志（frpc 原始输出、内部细节；默认级别不落盘不显示）
func Debugf(format string, args ...any) { std.write(LevelDebug, format, args...) }

// Infof 一般信息
func Infof(format string, args ...any) { std.write(LevelInfo, format, args...) }

// Warnf 警告（可自动恢复的异常：数据损坏重建、钩子失败等）
func Warnf(format string, args ...any) { std.write(LevelWarn, format, args...) }

// Errorf 错误（影响功能，需要用户关注）
func Errorf(format string, args ...any) { std.write(LevelError, format, args...) }
