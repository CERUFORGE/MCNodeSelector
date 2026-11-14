// Package tray 提供 Windows 系统托盘图标与气泡通知（纯 syscall，零第三方依赖）。
//
// 能力：
//   - 启动后在通知区域显示托盘图标；
//   - 双击托盘图标 / 菜单 → 显示或隐藏控制台窗口；
//   - 右键托盘 → 菜单（显示窗口 / 隐藏窗口 / 退出）；
//   - Notify() → 通过托盘气泡弹出桌面通知（切换节点 / 全部节点不可用等）；
//   - 启动成功后移除控制台窗口关闭按钮（防止误关），改用托盘菜单退出。
//
// 非 Windows 平台为空实现（tray_other.go），保证可编译。
package tray

import (
	"fmt"
	"runtime"
	"sync"
	"syscall"
	"unsafe"

	"mcnode/internal/i18n"
)

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	shell32  = syscall.NewLazyDLL("shell32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")

	procRegisterClassExW    = user32.NewProc("RegisterClassExW")
	procCreateWindowExW     = user32.NewProc("CreateWindowExW")
	procDefWindowProcW      = user32.NewProc("DefWindowProcW")
	procGetMessageW         = user32.NewProc("GetMessageW")
	procTranslateMessage    = user32.NewProc("TranslateMessage")
	procDispatchMessageW    = user32.NewProc("DispatchMessageW")
	procPostQuitMessage     = user32.NewProc("PostQuitMessage")
	procDestroyWindow       = user32.NewProc("DestroyWindow")
	procPostMessageW        = user32.NewProc("PostMessageW")
	procLoadIconW           = user32.NewProc("LoadIconW")
	procCreatePopupMenu     = user32.NewProc("CreatePopupMenu")
	procAppendMenuW         = user32.NewProc("AppendMenuW")
	procTrackPopupMenu      = user32.NewProc("TrackPopupMenu")
	procDestroyMenu         = user32.NewProc("DestroyMenu")
	procGetCursorPos        = user32.NewProc("GetCursorPos")
	procSetForegroundWindow = user32.NewProc("SetForegroundWindow")
	procShowWindow          = user32.NewProc("ShowWindow")
	procGetConsoleWindow    = kernel32.NewProc("GetConsoleWindow")
	procGetSystemMenu       = user32.NewProc("GetSystemMenu")
	procDeleteMenu          = user32.NewProc("DeleteMenu")
	procDrawMenuBar         = user32.NewProc("DrawMenuBar")
	procShellNotifyIconW    = shell32.NewProc("Shell_NotifyIconW")
	procGetModuleHandleW    = kernel32.NewProc("GetModuleHandleW")
)

const (
	wmApp           = 0x8000
	trayCallbackMsg = wmApp + 1
	wmCommand       = 0x0111
	wmClose         = 0x0010
	wmDestroy       = 0x0002
	wmNull          = 0x0000
	wmLButtonDblClk = 0x0203
	wmRButtonUp     = 0x0205

	nimAdd    = 0
	nimModify = 1
	nimDelete = 2

	nifMessage = 0x0001
	nifIcon    = 0x0002
	nifTip     = 0x0004
	nifInfo    = 0x0010

	niifInfo    = 0x00000001
	niifWarning = 0x00000002

	swHide = 0
	swShow = 5

	mfString    = 0x00000000
	mfSeparator = 0x00000800

	tpmReturnCmd = 0x0100
	tpmNonotify  = 0x0080

	idiApplication = 32512

	scClose     = 0xF060
	mfByCommand = 0x00000000
)

// Tray 托盘句柄
type Tray struct {
	mu        sync.Mutex
	hwnd      uintptr
	nid       notifyIconDataW
	className *uint16
	onExit    func()
	onCmd     func(string)
	started   bool
}

// SetCommandHandler 设置托盘菜单命令回调（"s"=下一节点 "m"=多线 "t"=重测 "logs"=打开日志）
func (t *Tray) SetCommandHandler(f func(string)) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.onCmd = f
	t.mu.Unlock()
}

// 单例（同一进程只允许一个托盘，WndProc 通过 g 访问）
var g *Tray

type wndClassExW struct {
	cbSize        uint32
	style         uint32
	lpfnWndProc   uintptr
	cbClsExtra    int32
	cbWndExtra    int32
	hInstance     uintptr
	hIcon         uintptr
	hCursor       uintptr
	hbrBackground uintptr
	lpszMenuName  *uint16
	lpszClassName *uint16
	hIconSm       uintptr
}

type point struct{ x, y int32 }

type msg struct {
	hwnd    uintptr
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	pt      point
}

type notifyIconDataW struct {
	cbSize           uint32
	hWnd             uintptr
	uID              uint32
	uFlags           uint32
	uCallbackMessage uint32
	hIcon            uintptr
	szTip            [128]uint16
	dwState          uint32
	dwStateMask      uint32
	szInfo           [256]uint16
	uTimeoutOrVer    uint32
	szInfoTitle      [64]uint16
	dwInfoFlags      uint32
	guidItem         [16]byte
	hBalloonIcon     uintptr
}

var (
	wndProcCallback = syscall.NewCallback(wndProc)
	// ctrlHandler 兜底：控制台关闭事件时尝试隐藏窗口（不同系统版本行为可能不同）
	procSetConsoleCtrlHandler = kernel32.NewProc("SetConsoleCtrlHandler")
	ctrlHandlerCallback       = syscall.NewCallback(ctrlHandler)
)

// Start 创建托盘图标并启动消息循环。onExit 在用户选择“退出”时调用（异步）。
// tip 为鼠标悬停提示文字。
func Start(tip string, onExit func()) (*Tray, error) {
	t := &Tray{onExit: onExit}
	errCh := make(chan error, 1)
	ready := make(chan struct{})
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		errCh <- t.initWindow(tip)
		close(ready)
		if t.hwnd == 0 {
			return
		}
		var m msg
		for {
			r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
			if int32(r) <= 0 { // 0=WM_QUIT，-1=错误
				break
			}
			procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
			procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
		}
	}()
	<-ready
	if err := <-errCh; err != nil {
		return nil, err
	}
	return t, nil
}

func (t *Tray) initWindow(tip string) error {
	g = t // WndProc 需要访问；在创建窗口前设置
	hInst, _, _ := procGetModuleHandleW.Call(0)
	name, err := syscall.UTF16PtrFromString("MCNodeSelectorTrayWnd")
	if err != nil {
		return err
	}
	t.className = name
	wc := wndClassExW{
		cbSize:        uint32(unsafe.Sizeof(wndClassExW{})),
		lpfnWndProc:   wndProcCallback,
		hInstance:     hInst,
		lpszClassName: t.className,
	}
	if r, _, e := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); r == 0 {
		return fmt.Errorf("注册托盘窗口类失败：%v", e)
	}
	hwnd, _, e := procCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(t.className)),
		uintptr(unsafe.Pointer(t.className)),
		0, 0, 0, 0, 0,
		0, 0, hInst, 0,
	)
	if hwnd == 0 {
		return fmt.Errorf("创建托盘窗口失败：%v", e)
	}
	t.hwnd = hwnd

	// 优先用 exe 内嵌的应用图标（与任务栏/窗口图标一致）；失败退回系统默认图标
	hicon, _, _ := procLoadIconW.Call(hInst, 1)
	if hicon == 0 {
		hicon, _, _ = procLoadIconW.Call(0, idiApplication)
	}
	t.nid = notifyIconDataW{
		cbSize:           uint32(unsafe.Sizeof(notifyIconDataW{})),
		hWnd:             hwnd,
		uID:              1,
		uFlags:           nifMessage | nifIcon | nifTip,
		uCallbackMessage: trayCallbackMsg,
		hIcon:            hicon,
	}
	copyUTF16(t.nid.szTip[:], tip)
	if r, _, e := procShellNotifyIconW.Call(nimAdd, uintptr(unsafe.Pointer(&t.nid))); r == 0 {
		procDestroyWindow.Call(hwnd)
		t.hwnd = 0
		return fmt.Errorf("添加托盘图标失败（通知区域可能不可用）：%v", e)
	}
	procSetConsoleCtrlHandler.Call(ctrlHandlerCallback, 1)
	t.disableConsoleClose()
	t.started = true
	return nil
}

// disableConsoleClose 移除控制台窗口的关闭按钮，防止误关（改用托盘菜单退出）
func (t *Tray) disableConsoleClose() {
	h, _, _ := procGetConsoleWindow.Call()
	if h == 0 {
		return
	}
	hmenu, _, _ := procGetSystemMenu.Call(h, 0)
	if hmenu == 0 {
		return
	}
	procDeleteMenu.Call(hmenu, scClose, mfByCommand)
	procDrawMenuBar.Call(h)
}

// Notify 弹出托盘气泡通知（桌面通知）
func (t *Tray) Notify(title, body string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.started {
		return
	}
	n := t.nid
	n.uFlags = nifMessage | nifIcon | nifTip | nifInfo
	n.dwInfoFlags = niifInfo
	copyUTF16(n.szInfo[:], body)
	copyUTF16(n.szInfoTitle[:], title)
	procShellNotifyIconW.Call(nimModify, uintptr(unsafe.Pointer(&n)))
}

// ShowConsole 显示控制台窗口
func (t *Tray) ShowConsole() {
	h, _, _ := procGetConsoleWindow.Call()
	if h != 0 {
		procShowWindow.Call(h, swShow)
	}
}

// HideConsole 隐藏控制台窗口到托盘
func (t *Tray) HideConsole() {
	h, _, _ := procGetConsoleWindow.Call()
	if h != 0 {
		procShowWindow.Call(h, swHide)
	}
}

// Stop 移除托盘图标并退出消息循环
func (t *Tray) Stop() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.started {
		return
	}
	procShellNotifyIconW.Call(nimDelete, uintptr(unsafe.Pointer(&t.nid)))
	if t.hwnd != 0 {
		procPostMessageW.Call(t.hwnd, wmClose, 0, 0)
	}
	t.started = false
}

// showMenu 右键菜单
func (t *Tray) showMenu() {
	if t == nil || t.hwnd == 0 {
		return
	}
	hmenu, _, _ := procCreatePopupMenu.Call()
	if hmenu == 0 {
		return
	}
	show, _ := syscall.UTF16PtrFromString(i18n.T("显示窗口", "Show window"))
	hide, _ := syscall.UTF16PtrFromString(i18n.T("隐藏窗口（后台运行）", "Hide (run in background)"))
	next, _ := syscall.UTF16PtrFromString(i18n.T("切换下一节点", "Next node"))
	multi, _ := syscall.UTF16PtrFromString(i18n.T("多线开关", "Toggle multipath"))
	retest, _ := syscall.UTF16PtrFromString(i18n.T("立即重测", "Retest now"))
	logs, _ := syscall.UTF16PtrFromString(i18n.T("打开日志目录", "Open logs"))
	quit, _ := syscall.UTF16PtrFromString(i18n.T("退出", "Quit"))
	procAppendMenuW.Call(hmenu, mfString, 1, uintptr(unsafe.Pointer(show)))
	procAppendMenuW.Call(hmenu, mfString, 2, uintptr(unsafe.Pointer(hide)))
	procAppendMenuW.Call(hmenu, mfSeparator, 0, 0)
	procAppendMenuW.Call(hmenu, mfString, 4, uintptr(unsafe.Pointer(next)))
	procAppendMenuW.Call(hmenu, mfString, 5, uintptr(unsafe.Pointer(multi)))
	procAppendMenuW.Call(hmenu, mfString, 6, uintptr(unsafe.Pointer(retest)))
	procAppendMenuW.Call(hmenu, mfString, 7, uintptr(unsafe.Pointer(logs)))
	procAppendMenuW.Call(hmenu, mfSeparator, 0, 0)
	procAppendMenuW.Call(hmenu, mfString, 3, uintptr(unsafe.Pointer(quit)))

	var pt point
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	procSetForegroundWindow.Call(t.hwnd)
	r, _, _ := procTrackPopupMenu.Call(
		hmenu, tpmReturnCmd|tpmNonotify,
		uintptr(pt.x), uintptr(pt.y), 0, t.hwnd, 0,
	)
	procDestroyMenu.Call(hmenu)
	procPostMessageW.Call(t.hwnd, wmNull, 0, 0)

	switch uint32(r) {
	case 1:
		t.ShowConsole()
	case 2:
		t.HideConsole()
	case 4:
		t.dispatch("s")
	case 5:
		t.dispatch("m")
	case 6:
		t.dispatch("t")
	case 7:
		t.dispatch("logs")
	case 3:
		if t.onExit != nil {
			go t.onExit()
		}
	}
}

// dispatch 调用命令回调（"s"/"m"/"t"/"logs"）
func (t *Tray) dispatch(cmd string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	f := t.onCmd
	t.mu.Unlock()
	if f != nil {
		go f(cmd)
	}
}

// wndProc 隐藏窗口的消息处理
func wndProc(hwnd, m, wParam, lParam uintptr) uintptr {
	t := g
	switch uint32(m) {
	case trayCallbackMsg:
		switch uint32(lParam) & 0xFFFF {
		case wmLButtonDblClk:
			if t != nil {
				t.ShowConsole()
			}
		case wmRButtonUp:
			if t != nil {
				t.showMenu()
			}
		}
		return 0
	case wmCommand:
		id := uint32(wParam) & 0xFFFF
		switch id {
		case 1:
			if t != nil {
				t.ShowConsole()
			}
		case 2:
			if t != nil {
				t.HideConsole()
			}
		case 3:
			if t != nil && t.onExit != nil {
				go t.onExit()
			}
		case 4:
			if t != nil {
				t.dispatch("s")
			}
		case 5:
			if t != nil {
				t.dispatch("m")
			}
		case 6:
			if t != nil {
				t.dispatch("t")
			}
		case 7:
			if t != nil {
				t.dispatch("logs")
			}
		}
		return 0
	case wmClose:
		procDestroyWindow.Call(hwnd)
		return 0
	case wmDestroy:
		procPostQuitMessage.Call(0)
		return 0
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, m, wParam, lParam)
	return r
}

// ctrlHandler 控制台控制事件：关闭事件时尝试隐藏窗口（兜底，具体行为依系统版本）
func ctrlHandler(ctrlType uint32) uintptr {
	const ctrlCloseEvent = 2
	if ctrlType == ctrlCloseEvent {
		if g != nil {
			g.HideConsole()
		}
		return 1 // 请求忽略关闭事件
	}
	return 0 // 其他事件（Ctrl-C 等）交回默认处理
}

// copyUTF16 把字符串复制到定长 UTF-16 缓冲区（含终止符）
func copyUTF16(dst []uint16, s string) {
	u, err := syscall.UTF16FromString(s)
	if err != nil {
		return
	}
	n := copy(dst, u)
	if n < len(dst) {
		dst[n] = 0
	}
}
