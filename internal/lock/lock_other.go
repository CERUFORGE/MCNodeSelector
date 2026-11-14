//go:build !windows

package lock

import "syscall"

// pidAlive 非 Windows 平台用 signal 0 探测进程存活
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}
