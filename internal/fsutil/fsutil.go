// Package fsutil 提供原子写入与损坏文件备份工具（F5.4 / §10）。
// 一切数据文件写入必须原子化：先写 .tmp 再 rename，
// 防止断电/被杀进程时留下半截文件导致下次启动解析失败。
package fsutil

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// WriteAtomic 原子写入文件：先写同目录下 <path>.tmp，再 rename 覆盖目标。
// Windows 上 os.Rename 使用 MoveFileEx(REPLACE_EXISTING)，可安全覆盖已存在文件。
func WriteAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	// 确保目录存在（首次运行时 data/、frpc_run/ 可能尚未创建）
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("创建目录失败（%s）：%w", dir, err)
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, perm)
	if err != nil {
		return fmt.Errorf("写入临时文件失败（%s）：%w", tmp, err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("写入临时文件失败（%s）：%w", tmp, err)
	}
	// 落盘，进一步降低突然断电导致半截文件的概率
	if err := f.Sync(); err != nil {
		// Sync 失败不致命（部分文件系统不支持），继续走 rename
		_ = err
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("关闭临时文件失败（%s）：%w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("替换文件失败（%s ← %s）：%w", path, tmp, err)
	}
	return nil
}

// BackupCorrupt 把损坏的数据文件改名为 <path>.corrupt.<时间戳>，
// 让调用方可以放心从空数据重建（损坏自愈，F5.4）。
func BackupCorrupt(path string) (backupPath string, err error) {
	ts := time.Now().Format("20060102-150405")
	backupPath = path + ".corrupt." + ts
	if err := os.Rename(path, backupPath); err != nil {
		return "", fmt.Errorf("备份损坏文件失败（%s）：%w", path, err)
	}
	return backupPath, nil
}

// Exists 判断文件是否存在
func Exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
