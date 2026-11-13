package web

import (
	"crypto/hmac"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

func itoa(n int) string { return strconv.Itoa(n) }

func itoa64(n int64) string { return strconv.FormatInt(n, 10) }

// hmacEqual 常量时间比较，避免 token 被按字符试探。
func hmacEqual(a, b string) bool { return hmac.Equal([]byte(a), []byte(b)) }

// flashRedirect 重定向并带上提示文字，靠 query 参数传递。
func flashRedirect(w http.ResponseWriter, r *http.Request, path, message string, ok bool) {
	kind := "ok"
	if !ok {
		kind = "err"
	}
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	target := path + sep + "flash=" + url.QueryEscape(message) + "&kind=" + kind
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// formInt 取表单整数，空值或非法返回 0。
func formInt(r *http.Request, key string) int {
	n, err := strconv.Atoi(strings.TrimSpace(r.FormValue(key)))
	if err != nil {
		return 0
	}
	return n
}

// formBool 复选框：只要传了值就算勾选（HTML checkbox 不勾选时不发送字段）。
func formBool(r *http.Request, key string) bool {
	return strings.TrimSpace(r.FormValue(key)) != ""
}
