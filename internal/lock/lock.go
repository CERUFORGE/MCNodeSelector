// Package lock 单实例锁（F15）：data/mcnode.lock 写入 PID；
// 二次启动检测到存活实例 → 中文提示后退出；陈旧锁自动清理。
package lock

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Acquire 获取单实例锁。已有存活实例时返回中文错误；
// 锁文件存在但进程已死（陈旧锁）→ 自动清理后重新获取。
// 返回的 release 函数幂等，且只在锁仍属于当前进程时删除（防误删他人锁）。
func Acquire(path string) (release func(), err error) {
	if data, rerr := os.ReadFile(path); rerr == nil {
		if pid, perr := strconv.Atoi(strings.TrimSpace(string(data))); perr == nil {
			if pid != os.Getpid() && pidAlive(pid) {
				return nil, fmt.Errorf(
					"检测到另一个 MCNodeSelector 实例正在运行（PID %d）。\n"+
						"  - 如需查看状态：mcnode --status\n"+
						"  - 如确认没有实例在跑：删除锁文件 %s 后重试", pid, path)
			}
		}
		// 陈旧锁（进程已退出）：自动清理
		_ = os.Remove(path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("创建数据目录失败（%s）：%w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		return nil, fmt.Errorf("写入锁文件失败（%s）：%w", path, err)
	}
	released := false
	return func() {
		if released {
			return
		}
		released = true
		if data, rerr := os.ReadFile(path); rerr == nil {
			if pid, perr := strconv.Atoi(strings.TrimSpace(string(data))); perr == nil && pid == os.Getpid() {
				_ = os.Remove(path)
			}
		}
	}, nil
}

// Check 检查锁状态（诊断用，不获取锁）：
// 返回 锁文件中的 PID 与对应进程是否存活；无锁文件返回 (0, false)。
func Check(path string) (pid int, alive bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	pid, err = strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, false
	}
	return pid, pidAlive(pid)
}
