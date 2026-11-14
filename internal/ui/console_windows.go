//go:build windows

package ui

import (
	"errors"
	"syscall"
	"unsafe"
)

// Windows 控制台 API（仅标准库 syscall，零第三方依赖）。
// 用 build tag 隔离，保证非 Windows 平台可编译（§3）。

var (
	kernel32                = syscall.NewLazyDLL("kernel32.dll")
	user32                  = syscall.NewLazyDLL("user32.dll")
	procGetStdHandle        = kernel32.NewProc("GetStdHandle")
	procGetConsoleMode      = kernel32.NewProc("GetConsoleMode")
	procSetConsoleMode      = kernel32.NewProc("SetConsoleMode")
	procSetConsoleCP        = kernel32.NewProc("SetConsoleCP")
	procSetConsoleOutCP     = kernel32.NewProc("SetConsoleOutputCP")
	procSetConsoleTitleW    = kernel32.NewProc("SetConsoleTitleW")
	procFindWindowW         = user32.NewProc("FindWindowW")
	procMessageBoxW         = user32.NewProc("MessageBoxW")
	procShowWindow          = user32.NewProc("ShowWindow")
	procSetForegroundWindow = user32.NewProc("SetForegroundWindow")
)

// SetUTF8Console 把控制台输入/输出代码页切换到 UTF-8（65001）。
// 直接双击 exe（未经 BAT 的 chcp 65001）时，保证中文与表格正常显示。
func SetUTF8Console() {
	procSetConsoleCP.Call(uintptr(65001))
	procSetConsoleOutCP.Call(uintptr(65001))
}

// SetConsoleTitle 设置控制台窗口标题（供第二个实例找到并唤醒本实例）。
func SetConsoleTitle(title string) {
	if p, err := syscall.UTF16PtrFromString(title); err == nil {
		procSetConsoleTitleW.Call(uintptr(unsafe.Pointer(p)))
	}
}

// FocusExistingWindow 按窗口标题查找已存在的实例窗口并还原/置顶；找到返回 true。
func FocusExistingWindow(title string) bool {
	p, err := syscall.UTF16PtrFromString(title)
	if err != nil {
		return false
	}
	h, _, _ := procFindWindowW.Call(0, uintptr(unsafe.Pointer(p)))
	if h == 0 {
		return false
	}
	procShowWindow.Call(h, 9) // SW_RESTORE
	procSetForegroundWindow.Call(h)
	return true
}

// MessageBox 弹出一个模态消息框（MB_OK | MB_ICONERROR | MB_SETFOREGROUND）。
func MessageBox(title, text string) {
	t, err1 := syscall.UTF16PtrFromString(text)
	c, err2 := syscall.UTF16PtrFromString(title)
	if err1 != nil || err2 != nil {
		return
	}
	procMessageBoxW.Call(0, uintptr(unsafe.Pointer(t)), uintptr(unsafe.Pointer(c)), 0x10|0x10000)
}

const (
	enableProcessedInput            = 0x0001 // 输入：Ctrl-C 产生信号
	enableLineInput                 = 0x0002 // 输入：行模式（需回车提交）
	enableEchoInput                 = 0x0004 // 输入：回显
	enableVirtualTerminalProcessing = 0x0004 // 输出：VT 转义序列处理
)

// stdHandle 获取标准句柄
func stdHandle(which int) (uintptr, error) {
	r, _, _ := procGetStdHandle.Call(uintptr(uint32(which)))
	if r == 0 || r == uintptr(^uintptr(0)) {
		return 0, errors.New("获取控制台句柄失败（可能被重定向到了管道/文件）")
	}
	return r, nil
}

// EnableVTOutput 启用标准输出的 VT 处理（ANSI 颜色与光标控制）。
// 需要 Windows 10 1809+；失败时返回错误，调用方降级为无颜色输出。
func EnableVTOutput() error {
	h, err := stdHandle(syscall.STD_OUTPUT_HANDLE)
	if err != nil {
		return err
	}
	var mode uint32
	ret, _, _ := procGetConsoleMode.Call(h, uintptr(unsafe.Pointer(&mode)))
	if ret == 0 {
		return errors.New("读取控制台输出模式失败")
	}
	mode |= enableVirtualTerminalProcessing
	ret, _, _ = procSetConsoleMode.Call(h, uintptr(mode))
	if ret == 0 {
		return errors.New("启用 ANSI 转义支持失败（需要 Windows 10 1809+，或控制台被重定向）")
	}
	return nil
}

// MakeRawInput 把标准输入切到单字符模式（关行输入、关回显，保留 Ctrl-C 信号），
// 返回恢复函数。watch 模式的单字符命令依赖此设置。
func MakeRawInput() (restore func(), err error) {
	h, err := stdHandle(syscall.STD_INPUT_HANDLE)
	if err != nil {
		return nil, err
	}
	var old uint32
	ret, _, _ := procGetConsoleMode.Call(h, uintptr(unsafe.Pointer(&old)))
	if ret == 0 {
		return nil, errors.New("读取控制台输入模式失败")
	}
	nw := old &^ enableLineInput &^ enableEchoInput
	procSetConsoleMode.Call(h, uintptr(nw))
	return func() {
		procSetConsoleMode.Call(h, uintptr(old))
	}, nil
}
