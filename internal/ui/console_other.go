//go:build !windows

package ui

import "errors"

// EnableVTOutput 非 Windows 平台默认终端支持 ANSI，直接成功。
func EnableVTOutput() error { return nil }

// SetUTF8Console 非 Windows 平台默认使用 UTF-8，无需切换代码页。
func SetUTF8Console() {}

// SetConsoleTitle 非 Windows 平台空实现
func SetConsoleTitle(title string) {}

// FocusExistingWindow 非 Windows 平台空实现
func FocusExistingWindow(title string) bool { return false }

// MessageBox 非 Windows 平台空实现
func MessageBox(title, text string) {}

// MakeRawInput 非 Windows 平台保持默认行输入（命令需回车提交），提供占位实现保证可编译。
func MakeRawInput() (restore func(), err error) {
	return func() {}, errors.New("非 Windows 平台使用行输入模式（命令后需按回车）")
}
