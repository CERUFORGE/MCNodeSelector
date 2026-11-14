//go:build !windows

// Package tray 在非 Windows 平台为空实现（仅保证可编译）。
package tray

import "errors"

// Tray 空句柄
type Tray struct{}

// Start 非 Windows 平台不支持系统托盘
func Start(tip string, onExit func()) (*Tray, error) {
	return nil, errors.New("当前平台不支持系统托盘")
}

// Notify 空实现
func (t *Tray) Notify(title, body string) {}

// ShowConsole 空实现
func (t *Tray) ShowConsole() {}

// HideConsole 空实现
func (t *Tray) HideConsole() {}

// Stop 空实现
func (t *Tray) Stop() {}

// SetCommandHandler 空实现
func (t *Tray) SetCommandHandler(f func(string)) {}
