//go:build windows

package lock

import (
	"syscall"
	"unsafe"
)

// pidAlive 判断 Windows 进程是否存活：
// OpenProcess(PROCESS_QUERY_LIMITED_INFORMATION) + GetExitCodeProcess。
var (
	kernel32        = syscall.NewLazyDLL("kernel32.dll")
	procOpenProcess = kernel32.NewProc("OpenProcess")
	procGetExitCode = kernel32.NewProc("GetExitCodeProcess")
)

const stillActive = 259 // STILL_ACTIVE 退出码

func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	// PROCESS_QUERY_LIMITED_INFORMATION = 0x1000
	h, _, _ := procOpenProcess.Call(0x1000, 0, uintptr(pid))
	if h == 0 {
		return false // 打不开：进程不存在或无权限（按不存在处理）
	}
	defer syscall.CloseHandle(syscall.Handle(h))
	var code uint32
	ret, _, _ := procGetExitCode.Call(h, uintptr(unsafe.Pointer(&code)))
	if ret == 0 {
		return true // 查询失败：宁可误报存活（防止双开），返回 true
	}
	return code == stillActive
}
