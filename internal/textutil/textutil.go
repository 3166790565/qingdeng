// Package textutil 放跨包共用的纯文本小工具，避免 parser 与 web 互相导入。
package textutil

import (
	"unicode"
	"unicode/utf8"
)

// CountNonSpace 统计去掉所有空白后的字符数，中文按字计。
// 用 utf8.RuneCountInString 而不是 len()，避免把 UTF-8 字节数当成字。
func CountNonSpace(s string) int {
	n := 0
	for _, r := range s {
		if unicode.IsSpace(r) {
			continue
		}
		n++
	}
	return n
}

// TruncateRunes 按字符截断，超出时补省略号。
func TruncateRunes(s string, limit int) string {
	if utf8.RuneCountInString(s) <= limit {
		return s
	}
	runes := []rune(s)
	return string(runes[:limit]) + "…"
}

// RuneLen 字符数，替代 len 用于中文长度判断。
func RuneLen(s string) int { return utf8.RuneCountInString(s) }
