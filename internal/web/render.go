// Package web 是 HTTP 层：路由、模板渲染、管理员会话与书源接口。
package web

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"qingdeng/internal/config"
	"qingdeng/internal/store"
	"qingdeng/internal/textutil"
)

// Renderer 持有编译好的模板集合。
//
// 每个页面单独一个模板集：Go 的 define 是全局命名空间，
// 把所有页面塞进同一个 Template 时，后解析的 {{define "content"}} 会覆盖前面的。
// 因此这里按 (布局, 页面) 两两组合各建一个集，执行时统一用布局名。
type Renderer struct {
	pages map[string]*pageSet
	cfg   *config.Config
}

// pageSet 一个页面的模板与执行入口。
type pageSet struct {
	tmpl *template.Template
	exec string
}

// layoutFor 返回 (布局文件名, 执行入口)。
//
// Go 的 define 是全局命名空间，把所有页面塞进同一个 Template 时
// 后解析的 {{define "content"}} 会覆盖前面的，所以每个页面单独建集。
// 执行入口：前台是 base.html，后台是 admin/base.html，登录页是自身。
func layoutFor(name string) (file, exec string) {
	if name == "admin/login.html" {
		// 登录页是完整文档，没有 {{define}} 块，直接用文件自己的模板名执行
		return "", "admin/login.html"
	}
	if strings.HasPrefix(name, "admin/") {
		return "admin/base.html", "admin/base.html"
	}
	return "base.html", "base.html"
}

// NewRenderer 解析 templates/ 下全部模板。
func NewRenderer(cfg *config.Config) (*Renderer, error) {
	funcs := template.FuncMap{
		"wordCount":     wordCount,
		"modeLabel":     func(mode string) string { return store.SplitModeLabels[mode] },
		"coverGradient": coverGradient,
		"coverInitial":  coverInitial,
		"dateOnly":      func(s string) string { return firstN(s, 10) },
		"shortTime":     func(s string) string { return firstN(s, 16) },
		"shortID":       func(s string) string { return firstN(s, 8) },
		"truncate":      func(s string, n int) string { return textutil.TruncateRunes(s, n) },
		"deref":         deref,
		"seq":           seq,
		"dict":          dict,
		"sub1":          func(n int) int { return n - 1 },
		"inc":           func(n int) int { return n + 1 },
		"runeLen":       func(s string) int { return len([]rune(s)) },
		// queryQS 拼可选的 &key=value，空值返回空串
		"queryQS": func(key, value string) string {
			if value == "" {
				return ""
			}
			return "&" + key + "=" + url.QueryEscape(value)
		},
	}
	macros := filepath.Join(cfg.TemplateDir, "_macros.html")
	pages := map[string]*pageSet{}

	collect := func(pattern string) ([]string, error) {
		files, err := filepath.Glob(filepath.Join(cfg.TemplateDir, pattern))
		if err != nil {
			return nil, err
		}
		return files, nil
	}

	rootFiles, err := collect("*.html")
	if err != nil {
		return nil, err
	}
	adminFiles, err := collect(filepath.Join("admin", "*.html"))
	if err != nil {
		return nil, err
	}

	for _, file := range append(rootFiles, adminFiles...) {
		base := filepath.Base(file)
		rel := strings.ReplaceAll(strings.TrimPrefix(file, cfg.TemplateDir+string(filepath.Separator)), string(filepath.Separator), "/")
		// 布局与宏是单独加载的，不当成页面；按相对路径判断，
		// 否则 admin/base.html 会因为 basename 也叫 base.html 被误跳过。
		if rel == "base.html" || rel == "admin/base.html" || base == "_macros.html" {
			continue
		}
		layoutFile, exec := layoutFor(rel)
		// 每个页面一个独立的模板集，根模板名固定为 "root"。
		// 不能拿页面 basename 当根名——admin/books.html 与 books.html 撞名时
		// 会互相顶掉，前台页面的 base.html 也可能被后续 Parse 覆盖。
		set, err := template.New("root").Funcs(funcs).ParseFiles(macros)
		if err != nil {
			return nil, fmt.Errorf("解析 _macros.html 失败：%w", err)
		}
		if layoutFile != "" {
			layoutBody, err := os.ReadFile(filepath.Join(cfg.TemplateDir, layoutFile))
			if err != nil {
				return nil, fmt.Errorf("读取布局 %s 失败：%w", layoutFile, err)
			}
			if _, err := set.New(layoutFile).Parse(string(layoutBody)); err != nil {
				return nil, fmt.Errorf("解析布局 %s 失败：%w", layoutFile, err)
			}
		}
		body, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("读取模板 %s 失败：%w", rel, err)
		}
		// 页面文件用自己的相对路径命名，{{define}} 块全局生效
		if _, err := set.New(rel).Parse(string(body)); err != nil {
			return nil, fmt.Errorf("解析模板 %s 失败：%w", rel, err)
		}
		pages[rel] = &pageSet{tmpl: set, exec: exec}
	}
	return &Renderer{pages: pages, cfg: cfg}, nil
}

// Common 注入每个页面都需要的公共变量。
func (r *Renderer) Common() map[string]any {
	return map[string]any{
		"SiteName":        r.cfg.SiteName,
		"SiteTagline":     r.cfg.SiteTagline,
		"SplitModeList":   store.SplitModes,
		"SplitModeLabels": store.SplitModeLabels,
		// 顶栏搜索框在所有页面都渲染，缺省值必须给，
		// 否则 map 取不到键会输出 <no value>
		"Q":     "",
		"Flash": "",
		"Kind":  "",
		"OrderOptions": []OrderOption{
			{Key: "updated", Label: "最近更新"},
			{Key: "created", Label: "最近入库"},
			{Key: "words", Label: "字数"},
			{Key: "title", Label: "书名"},
		},
	}
}

// HTML 渲染页面到缓冲区再写响应：模板出错时能给 500，而不是半截 HTML。
func (r *Renderer) HTML(w http.ResponseWriter, status int, name string, data map[string]any) {
	if data == nil {
		data = map[string]any{}
	}
	for k, v := range r.Common() {
		if _, exists := data[k]; !exists {
			data[k] = v
		}
	}
	set, ok := r.pages[name]
	if !ok {
		log.Printf("模板 %s 不存在", name)
		http.Error(w, "页面不存在", http.StatusInternalServerError)
		return
	}
	var buf bytes.Buffer
	// 用布局名执行：页面文件的 define 会覆盖布局里的同名块
	if err := set.tmpl.ExecuteTemplate(&buf, set.exec, data); err != nil {
		log.Printf("渲染 %s 失败：%v", name, err)
		http.Error(w, "页面渲染失败", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = buf.WriteTo(w)
}

// JSON 统一 JSON 响应。
func JSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(payload)
}

// ---------------------------------------------------------------- 过滤器实现

func wordCount(value int) string {
	if value >= 10000 {
		return fmt.Sprintf("%.1f 万字", float64(value)/10000)
	}
	return strconv.Itoa(value) + " 字"
}

// coverPalette 无封面时按书名生成稳定的渐变占位块，一共 6 套暖色调。
var coverPalette = [][2]string{
	{"#2f4f4a", "#6b8f83"},
	{"#4a3f5c", "#8a7aa3"},
	{"#5c3f3f", "#a37a72"},
	{"#3f4a5c", "#7a90a3"},
	{"#4f4a2f", "#8f8a6b"},
	{"#3f5c4a", "#7aa389"},
}

func coverGradient(title string) string {
	if title == "" {
		title = "书"
	}
	sum := 0
	for _, r := range title {
		sum += int(r)
	}
	pair := coverPalette[sum%len(coverPalette)]
	return fmt.Sprintf("linear-gradient(150deg, %s 0%%, %s 100%%)", pair[0], pair[1])
}

func coverInitial(title string) string {
	runes := []rune(title)
	if len(runes) == 0 {
		return "书"
	}
	return string(runes[0])
}

func firstN(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n])
}

func deref(row *store.ChapterRow) store.ChapterRow {
	if row == nil {
		return store.ChapterRow{}
	}
	return *row
}

// seq 生成 1..n，模板里画序号用。
func seq(n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = i + 1
	}
	return out
}

// dict 在小范围内拼上下文变量。
func dict(kv ...any) map[string]any {
	out := make(map[string]any, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		key, _ := kv[i].(string)
		out[key] = kv[i+1]
	}
	return out
}

// ---------------------------------------------------------------- 管理员会话

const sessionCookieName = "novel_admin"

// signSession 用 HMAC-SHA256 给 "admin|时间戳" 签名，cookie 值形如 payload.sig。
//
// Python 版用 itsdangerous 的 TimestampSigner；Go 这边自己实现同样的语义：
// 带时间戳、常量时间校验、过期即失效。
func (r *Renderer) signSession() string {
	payload := fmt.Sprintf("admin|%d", time.Now().Unix())
	mac := hmac.New(sha256.New, []byte(r.cfg.SecretKey))
	mac.Write([]byte(payload))
	sig := hex.EncodeToString(mac.Sum(nil))
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + sig
}

func (r *Renderer) sessionValid(value string) bool {
	payloadB64, sig, ok := strings.Cut(value, ".")
	if !ok {
		return false
	}
	payload, err := base64.RawURLEncoding.DecodeString(payloadB64)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(r.cfg.SecretKey))
	mac.Write(payload)
	expected := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(expected), []byte(sig)) {
		return false
	}
	parts := strings.Split(string(payload), "|")
	if len(parts) != 2 || parts[0] != "admin" {
		return false
	}
	issued, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return false
	}
	return time.Now().Unix()-issued <= int64(r.cfg.SessionMaxAge)
}

// IsAdmin 请求是否带着有效管理员会话。
func (r *Renderer) IsAdmin(req *http.Request) bool {
	cookie, err := req.Cookie(sessionCookieName)
	if err != nil {
		return false
	}
	return r.sessionValid(cookie.Value)
}

// SetSession 下发会话 cookie。
func (r *Renderer) SetSession(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    r.signSession(),
		Path:     "/",
		MaxAge:   r.cfg.SessionMaxAge,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

// ClearSession 清会话 cookie。
func (r *Renderer) ClearSession(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookieName, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
}

// VerifyPassword 常量时间比较，避免按字符试探。
func (r *Renderer) VerifyPassword(password string) bool {
	// 双方都取 sha256 再比，长度差异不泄露信息
	a := sha256.Sum256([]byte(password))
	b := sha256.Sum256([]byte(r.cfg.AdminPassword))
	return hmac.Equal(a[:], b[:])
}

// ensureDirs 确保静态目录存在，StaticFiles 挂载时不会 panic。
func ensureDirs(paths ...string) {
	for _, p := range paths {
		_ = os.MkdirAll(p, 0o755)
	}
}

// OrderOption 书架/后台列表的排序选项。
type OrderOption struct {
	Key   string
	Label string
}
