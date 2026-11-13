package parser

import (
	"net/url"
	"sort"
	"strconv"
)

// 这些小工具不导出，避免调用方依赖实现细节。

func itoa(n int) string { return strconv.Itoa(n) }

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

func sortInts(xs []int) { sort.Ints(xs) }

func truncateRunes(s string, limit int) string {
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return string(r[:limit])
}

func urlUnescape(s string) (string, error) {
	return url.PathUnescape(s)
}
