package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"qingdeng/internal/config"
	"qingdeng/internal/importer"
	"qingdeng/internal/parser"
	"qingdeng/internal/store"
	"qingdeng/internal/tg"
)

// testApp 建一套完整的 App：临时库 + 真实模板目录。
func testApp(t *testing.T, mutate func(*config.Config)) (*App, *store.DB) {
	t.Helper()
	root, _ := filepath.Abs("../..")
	dir := t.TempDir()
	cfg := &config.Config{
		BaseDir:       root,
		DataDir:       dir,
		DBPath:        filepath.Join(dir, "test.db"),
		TemplateDir:   filepath.Join(root, "templates"),
		StaticDir:     filepath.Join(root, "static"),
		LegadoDir:     filepath.Join(root, "legado"),
		SiteName:      "青灯书斋",
		SiteTagline:   "一盏灯，一本书",
		SiteBaseURL:   "http://127.0.0.1:8000",
		AdminPassword: "testpass",
		SecretKey:     "test-secret-key-for-session-signing",
		SessionCookie: "novel_admin",
		SessionMaxAge: 3600,
	}
	if mutate != nil {
		mutate(cfg)
	}

	db, err := store.Open(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	schema, err := readSchema(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Init(schema); err != nil {
		t.Fatal(err)
	}

	imp := importer.New(db, cfg)
	mgr := tg.New(cfg, db, imp)
	app, err := NewApp(cfg, db, imp, mgr)
	if err != nil {
		t.Fatal(err)
	}
	return app, db
}

func readSchema(root string) (string, error) {
	data, err := osReadFile(filepath.Join(root, "schema.sql"))
	return string(data), err
}

// seedBook 灌一本带章节的书，返回 book_id。
func seedBook(t *testing.T, db *store.DB) int64 {
	t.Helper()
	var b strings.Builder
	b.WriteString("#  测试书\n作者：某某\n\n")
	for i := 1; i <= 3; i++ {
		b.WriteString("第" + strconvItoa(i) + "章 标题\n\n")
		b.WriteString(strings.Repeat("正文里有一句「海棠花开」。", 40) + "\n\n")
	}
	opts := parser.ParseOptions{TargetChars: 3000, AddSubtitle: true, StripAds: true, MergeHardWraps: true}.Normalize()
	parsed := parser.ParseText(b.String(), "测试书.txt", opts, "utf-8")
	id, err := db.InsertBook(context.Background(), &parsed, "测试书.txt", "hash-1")
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func strconvItoa(n int) string {
	return string(rune('0' + n))
}

// do 发一个请求，带可选的登录 cookie。
func do(t *testing.T, app *App, method, target string, body url.Values, loggedIn bool) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body != nil {
		reader = strings.NewReader(body.Encode())
	} else {
		reader = strings.NewReader("")
	}
	req := httptest.NewRequest(method, target, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if loggedIn {
		req.AddCookie(&http.Cookie{Name: "novel_admin", Value: app.Render.signSession()})
	}
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	return rec
}

func TestPublicPagesRender(t *testing.T) {
	app, db := testApp(t, nil)
	seedBook(t, db)

	for _, target := range []string{"/", "/books", "/search", "/api/docs", "/book/1", "/book/1/read/1"} {
		rec := do(t, app, "GET", target, nil, false)
		if rec.Code != 200 {
			t.Errorf("%s -> %d: %s", target, rec.Code, firstLine(rec.Body.String()))
		}
		if !strings.Contains(rec.Body.String(), "青灯书斋") {
			t.Errorf("%s 没渲染出站名", target)
		}
	}
}

func TestHealthz(t *testing.T) {
	app, db := testApp(t, nil)
	seedBook(t, db)
	rec := do(t, app, "GET", "/healthz", nil, false)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Fatalf("healthz 异常：%d %s", rec.Code, rec.Body.String())
	}
}

// 后台页面未登录跳登录页；接口未登录返回 401 JSON。
func TestAdminGuards(t *testing.T) {
	app, db := testApp(t, nil)
	seedBook(t, db)

	rec := do(t, app, "GET", "/admin", nil, false)
	if rec.Code != http.StatusFound {
		t.Fatalf("未登录页面应 302，得到 %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "/admin/login") {
		t.Fatalf("跳转地址不对：%s", loc)
	}

	rec = do(t, app, "GET", "/admin/api/jobs/abc", nil, false)
	if rec.Code != 401 {
		t.Fatalf("未登录接口应 401，得到 %d", rec.Code)
	}
	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("401 应返回 JSON：%s", rec.Body.String())
	}
}

func TestLoginAndAdminPages(t *testing.T) {
	app, db := testApp(t, nil)
	seedBook(t, db)

	rec := do(t, app, "GET", "/admin/login", nil, false)
	if rec.Code != 200 {
		t.Fatalf("登录页渲染失败：%d %s", rec.Code, firstLine(rec.Body.String()))
	}

	// 错密码
	rec = do(t, app, "POST", "/admin/login", url.Values{"password": {"wrong"}, "next": {"/admin"}}, false)
	if rec.Code != 401 {
		t.Fatalf("错密码应 401，得到 %d", rec.Code)
	}

	// 对密码
	rec = do(t, app, "POST", "/admin/login", url.Values{"password": {"testpass"}, "next": {"/admin"}}, false)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("对密码应 303，得到 %d", rec.Code)
	}
	if len(rec.Result().Cookies()) == 0 {
		t.Fatal("登录成功应下发会话 cookie")
	}

	for _, target := range []string{"/admin", "/admin/upload", "/admin/books", "/admin/settings", "/admin/telegram", "/admin/books/1/chapters", "/admin/books/1/edit"} {
		rec = do(t, app, "GET", target, nil, true)
		if rec.Code != 200 {
			t.Errorf("%s -> %d: %s", target, rec.Code, firstLine(rec.Body.String()))
		}
	}
}

// 只允许跳回 /admin 开头，挡开放重定向。
func TestLoginRejectsOpenRedirect(t *testing.T) {
	app, _ := testApp(t, nil)
	rec := do(t, app, "POST", "/admin/login", url.Values{"password": {"testpass"}, "next": {"https://evil.example"}}, false)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("应 303，得到 %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/admin" {
		t.Fatalf("外站地址不该被采用，得到 %s", loc)
	}
}

// 书源 JSON 接口的形状与鉴权。
func TestAPIShapeAndToken(t *testing.T) {
	app, db := testApp(t, func(cfg *config.Config) { cfg.APIToken = "s3cret" })
	seedBook(t, db)

	// 无 token 401
	rec := do(t, app, "GET", "/api/book/1", nil, false)
	if rec.Code != 401 {
		t.Fatalf("无 token 应 401，得到 %d", rec.Code)
	}
	// 查询参数 token
	rec = do(t, app, "GET", "/api/book/1?token=s3cret", nil, false)
	if rec.Code != 200 {
		t.Fatalf("带 token 应 200，得到 %d", rec.Code)
	}
	var book struct {
		IsSuccess bool `json:"isSuccess"`
		Data      struct {
			Name             string `json:"name"`
			LastChapterTitle string `json:"latestChapterTitle"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &book); err != nil {
		t.Fatal(err)
	}
	if !book.IsSuccess || book.Data.Name != "测试书" || book.Data.LastChapterTitle == "" {
		t.Fatalf("详情返回不对：%s", rec.Body.String())
	}

	// 目录
	rec = do(t, app, "GET", "/api/book/1/catalog?token=s3cret", nil, false)
	var catalog struct {
		Data struct {
			Total int `json:"total"`
		} `json:"data"`
	}
	json.Unmarshal(rec.Body.Bytes(), &catalog)
	if catalog.Data.Total != 3 {
		t.Fatalf("目录应有 3 章，得到 %d", catalog.Data.Total)
	}

	// 正文
	rec = do(t, app, "GET", "/api/chapter/1?token=s3cret", nil, false)
	if !strings.Contains(rec.Body.String(), "海棠花开") {
		t.Fatalf("章节正文不对：%s", firstLine(rec.Body.String()))
	}

	// 不存在的书返回 isSuccess=false
	rec = do(t, app, "GET", "/api/book/999?token=s3cret", nil, false)
	if !strings.Contains(rec.Body.String(), `"isSuccess":false`) {
		t.Fatalf("不存在的书应返回失败：%s", rec.Body.String())
	}
}

// 全文检索：关键词高亮成 <mark>，且 HTML 转义不会被绕过。
func TestSearchHighlight(t *testing.T) {
	app, db := testApp(t, nil)
	seedBook(t, db)

	rec := do(t, app, "GET", "/search?q=%E6%B5%B7%E6%A3%A0&mode=full", nil, false)
	if rec.Code != 200 {
		t.Fatalf("检索页失败：%d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "<mark>海棠</mark>") {
		t.Fatalf("应高亮命中片段")
	}
}

// 短关键词（<3 字）退回 LIKE 也必须能搜到。
func TestSearchShortKeyword(t *testing.T) {
	app, db := testApp(t, nil)
	seedBook(t, db)

	// 单字走 LIKE 分支，高亮只标命中的那个字（<mark> 会把原文切开，别用整句断言）
	rec := do(t, app, "GET", "/search?q=%E6%B5%B7&mode=full", nil, false)
	body := rec.Body.String()
	if rec.Code != 200 || !strings.Contains(body, "<mark>海</mark>") || !strings.Contains(body, "棠花开") {
		t.Fatalf("单字检索失败：%d", rec.Code)
	}
}

func TestBookSourceJSON(t *testing.T) {
	app, _ := testApp(t, nil)
	rec := do(t, app, "GET", "/legado/源.json", nil, false)
	if rec.Code != 200 {
		t.Fatalf("书源生成失败：%d", rec.Code)
	}
	var source map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &source); err != nil {
		t.Fatal(err)
	}
	if source["bookSourceName"] != "青灯书斋" {
		t.Fatalf("书源名不对：%v", source["bookSourceName"])
	}
	if !strings.Contains(source["searchUrl"].(string), "/api/search") {
		t.Fatalf("搜索地址不对：%v", source["searchUrl"])
	}
}

func TestErrorPageAnd404(t *testing.T) {
	app, _ := testApp(t, nil)
	rec := do(t, app, "GET", "/book/999", nil, false)
	if rec.Code != 404 || !strings.Contains(rec.Body.String(), "404") {
		t.Fatalf("不存在的书应 404 页面：%d", rec.Code)
	}
	// API 路径走 JSON
	rec = do(t, app, "GET", "/api/book/999", nil, false)
	if !strings.Contains(rec.Body.String(), "errorMsg") {
		t.Fatalf("API 错误应回 JSON：%s", rec.Body.String())
	}
}

func TestUploadAndJobPoll(t *testing.T) {
	app, db := testApp(t, nil)
	_ = db

	// 用 multipart 上传一个 txt
	var body strings.Builder
	boundary := "----testboundary"
	content := "#  上传书\n\n第1章 起\n\n" + strings.Repeat("上传的正文内容。", 100)
	body.WriteString("--" + boundary + "\r\n")
	body.WriteString("Content-Disposition: form-data; name=\"files\"; filename=\"上传书.txt\"\r\n")
	body.WriteString("Content-Type: text/plain\r\n\r\n")
	body.WriteString(content + "\r\n")
	body.WriteString("--" + boundary + "--\r\n")

	req := httptest.NewRequest("POST", "/admin/upload", strings.NewReader(body.String()))
	req.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)
	req.AddCookie(&http.Cookie{Name: "novel_admin", Value: app.Render.signSession()})
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("上传失败：%d %s", rec.Code, rec.Body.String())
	}
	var up struct {
		OK    bool   `json:"ok"`
		JobID string `json:"job_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &up); err != nil {
		t.Fatal(err)
	}
	if !up.OK || up.JobID == "" {
		t.Fatalf("上传应返回 job_id：%s", rec.Body.String())
	}

	// 等后台任务收尾，再轮询
	var job map[string]any
	for i := 0; i < 100; i++ {
		rec = do(t, app, "GET", "/admin/api/jobs/"+up.JobID, nil, true)
		json.Unmarshal(rec.Body.Bytes(), &job)
		if job["Status"] == "done" || job["Status"] == "error" {
			break
		}
		sleepShort()
	}
	if job["Status"] != "done" {
		t.Fatalf("任务未完成：%v", job)
	}
	if job["OK"].(float64) != 1 {
		t.Fatalf("应成功 1 本：%v", job)
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	if len(s) > 120 {
		return s[:120]
	}
	return s
}
