//go:build windows

package frpc

import "syscall"

// sysProcAttr Windows 下隐藏 frpc 弹出的控制台窗口（§8.2）。
// 用 build tag 隔离 Windows API，保证交叉可编译（§3）。
func sysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{HideWindow: true}
}
