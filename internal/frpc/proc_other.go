//go:build !windows

package frpc

import "syscall"

// sysProcAttr 非 Windows 平台返回 nil（无窗口可隐藏），保证可编译（§3）。
func sysProcAttr() *syscall.SysProcAttr {
	return nil
}
