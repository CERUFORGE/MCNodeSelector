// Package config 中剥离 JSONC 注释的部分。
// 自写约 40 行状态机，零第三方依赖（§6.1）：
//   - 支持 // 行注释与 /* */ 块注释；
//   - 必须正确处理字符串字面量内部的 //（如 token 值含 // 不得误删）；
//   - 正确处理 \" 转义；
//   - 保留换行符，保证语法报错的行号与原文件对得上。
package config

// StripComments 剥离 JSONC 源码中的注释，返回纯 JSON 字节。
// 状态：普通态 / 字符串态 / 行注释态 / 块注释态。
func StripComments(src []byte) []byte {
	out := make([]byte, 0, len(src))
	const (
		stNormal = iota // 普通代码
		stString        // 字符串字面量内部
		stLine          // // 行注释内部
		stBlock         // /* 块注释内部
	)
	state := stNormal
	for i := 0; i < len(src); i++ {
		c := src[i]
		switch state {
		case stString:
			out = append(out, c)
			if c == '\\' && i+1 < len(src) {
				// 转义序列（\" \\ \n 等）：原样保留下一个字节，防止 \" 被误判为字符串结束
				out = append(out, src[i+1])
				i++
			} else if c == '"' {
				state = stNormal // 字符串正常结束
			}
		case stLine:
			if c == '\n' {
				// 行注释到换行结束：保留换行（维持行号）
				out = append(out, c)
				state = stNormal
			}
			// 注释内容直接丢弃
		case stBlock:
			if c == '*' && i+1 < len(src) && src[i+1] == '/' {
				// 块注释结束：用一个空格占位（防止 ab/*x*/cd 拼成一个 token）
				out = append(out, ' ')
				i++
				state = stNormal
			} else if c == '\n' {
				out = append(out, c) // 块注释内的换行也要保留（维持行号）
			}
		default: // stNormal
			switch {
			case c == '"':
				state = stString
				out = append(out, c)
			case c == '/' && i+1 < len(src) && src[i+1] == '/':
				state = stLine
				i++
			case c == '/' && i+1 < len(src) && src[i+1] == '*':
				state = stBlock
				i++
			default:
				out = append(out, c)
			}
		}
	}
	return out
}
