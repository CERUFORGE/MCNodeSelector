// Package i18n 提供最简双语支持：调用方同时给出中文与英文，按全局语言选择。
// 语言由 config.general.language 决定（zh / en，缺省 zh）。
package i18n

import "strings"

var lang = "zh"

// Set 设置当前语言（zh / en）
func Set(l string) {
	if strings.EqualFold(l, "en") {
		lang = "en"
	} else {
		lang = "zh"
	}
}

// Lang 当前语言
func Lang() string { return lang }

// T 按当前语言返回中文或英文
func T(zh, en string) string {
	if lang == "en" {
		return en
	}
	return zh
}
