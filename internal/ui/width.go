package ui

import "strings"

// dispLen 计算字符串的“显示宽度”：CJK/全角字符按 2 列计，其余按 1 列计。
// 状态屏表格对齐用（中英混排时保持列不歪）。
func dispLen(s string) int {
	w := 0
	for _, r := range s {
		switch {
		case r >= 0x1100 && r <= 0x115F, // 谚文
			r == 0x2329 || r == 0x232A,
			r >= 0x2E80 && r <= 0xA4CF && r != 0x303F, // CJK 部首/康熙/注音
			r >= 0xAC00 && r <= 0xD7A3,                // 谚文音节
			r >= 0xF900 && r <= 0xFAFF,                // CJK 兼容
			r >= 0xFE30 && r <= 0xFE4F,                // CJK 兼容形式
			r >= 0xFF00 && r <= 0xFF60,                // 全角形式
			r >= 0xFFE0 && r <= 0xFFE6,                // 全角符号
			r >= 0x20000 && r <= 0x2FFFD,              // CJK 扩展 B+
			r >= 0x30000 && r <= 0x3FFFD:
			w += 2
		case r >= 0x4E00 && r <= 0x9FFF, // CJK 统一表意
			r >= 0x3400 && r <= 0x4DBF: // CJK 扩展 A
			w += 2
		default:
			w++
		}
	}
	return w
}

// pad 按显示宽度右侧补空格到 w 列（中文状态屏对齐）
func pad(s string, w int) string {
	d := dispLen(s)
	if d >= w {
		return s
	}
	return s + strings.Repeat(" ", w-d)
}
