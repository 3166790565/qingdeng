package web

import (
	"log"
	"net/http"
	"strconv"
	"strings"

	"qingdeng/internal/config"
	"qingdeng/internal/importer"
	"qingdeng/internal/store"
	"qingdeng/internal/tg"
)

// App 把 HTTP 处理器需要的依赖串起来。
type App struct {
	Cfg    *config.Config
	DB     *store.DB
	Imp    *importer.Importer
	TG     *tg.Manager
	Render *Renderer
	// handler 在构造时一次性装配完成，避免每次调用都往 mux 里重复注册
	handler http.Handler
}

// NewApp 装配路由。
func NewApp(cfg *config.Config, db *store.DB, imp *importer.Importer, mgr *tg.Manager) (*App, error) {
	render, err := NewRenderer(cfg)
	if err != nil {
		return nil, err
	}
	ensureDirs(cfg.StaticDir)
	mux := http.NewServeMux()
	app := &App{Cfg: cfg, DB: db, Imp: imp, TG: mgr, Render: render}
	app.routes(mux)

	// 静态资源由 FileServer 处理，/static/ 前缀必须在建好 mux 后注册一次
	static := http.FileServer(http.Dir(cfg.StaticDir))
	mux.Handle("/static/", http.StripPrefix("/static/", static))
	app.handler = app.recoverer(mux)
	return app, nil
}

// Handler 返回装配好的根处理器。
func (a *App) Handler() http.Handler { return a.handler }

// ---------------------------------------------------------------- 路由表

func (a *App) routes(mux *http.ServeMux) {
	// 前台
	mux.HandleFunc("GET /healthz", a.healthz)
	mux.HandleFunc("GET /legado/源.json", a.legadoSource)
	mux.HandleFunc("GET /api/docs", a.apiDocs)
	mux.HandleFunc("GET /{$}", a.index)
	mux.HandleFunc("GET /books", a.shelf)
	mux.HandleFunc("GET /book/{id}", a.bookDetail)
	mux.HandleFunc("GET /book/{id}/read", a.readFirst)
	mux.HandleFunc("GET /book/{id}/read/{idx}", a.reader)
	mux.HandleFunc("GET /search", a.search)

	// 书源 JSON 接口
	mux.HandleFunc("GET /api/search", a.apiSearch)
	mux.HandleFunc("GET /api/book/{id}", a.apiBook)
	mux.HandleFunc("GET /api/book/{id}/catalog", a.apiCatalog)
	mux.HandleFunc("GET /api/chapter/{id}", a.apiChapter)

	// 后台
	mux.HandleFunc("GET /admin/login", a.loginForm)
	mux.HandleFunc("POST /admin/login", a.login)
	mux.HandleFunc("POST /admin/logout", a.logout)
	mux.HandleFunc("GET /admin", a.adminRequired(a.dashboard))
	mux.HandleFunc("GET /admin/upload", a.adminRequired(a.uploadForm))
	mux.HandleFunc("POST /admin/upload", a.adminAPIRequired(a.upload))
	mux.HandleFunc("GET /admin/api/jobs/{id}", a.adminAPIRequired(a.jobStatus))
	mux.HandleFunc("GET /admin/jobs/{id}", a.adminRequired(a.jobPage))
	mux.HandleFunc("GET /admin/books", a.adminRequired(a.bookList))
	mux.HandleFunc("GET /admin/books/{id}/edit", a.adminRequired(a.bookEditForm))
	mux.HandleFunc("POST /admin/books/{id}/edit", a.adminRequired(a.bookEdit))
	mux.HandleFunc("POST /admin/books/{id}/delete", a.adminRequired(a.bookDelete))
	mux.HandleFunc("POST /admin/books/{id}/resplit", a.adminRequired(a.resplitSingle))
	mux.HandleFunc("POST /admin/books/resplit", a.adminAPIRequired(a.resplitBatch))
	mux.HandleFunc("GET /admin/books/{id}/chapters", a.adminRequired(a.chapterList))
	mux.HandleFunc("GET /admin/chapters/{id}/edit", a.adminRequired(a.chapterEditForm))
	mux.HandleFunc("POST /admin/chapters/{id}/edit", a.adminRequired(a.chapterEdit))
	mux.HandleFunc("GET /admin/settings", a.adminRequired(a.settingsForm))
	mux.HandleFunc("POST /admin/settings", a.adminRequired(a.settingsSave))

	// 后台 TG 页
	mux.HandleFunc("GET /admin/telegram", a.adminRequired(a.telegramPage))
	mux.HandleFunc("POST /admin/telegram/api/login/send", a.adminAPIRequired(a.tgLoginSend))
	mux.HandleFunc("POST /admin/telegram/api/login/verify", a.adminAPIRequired(a.tgLoginVerify))
	mux.HandleFunc("POST /admin/telegram/api/login/password", a.adminAPIRequired(a.tgLoginPassword))
	mux.HandleFunc("POST /admin/telegram/api/logout", a.adminAPIRequired(a.tgLogout))
	mux.HandleFunc("POST /admin/telegram/channels", a.adminRequired(a.tgChannelAdd))
	mux.HandleFunc("POST /admin/telegram/channels/{id}/delete", a.adminRequired(a.tgChannelDelete))
	mux.HandleFunc("POST /admin/telegram/channels/{id}/toggle", a.adminRequired(a.tgChannelToggle))
	mux.HandleFunc("POST /admin/telegram/settings", a.adminRequired(a.tgSettingsSave))
	mux.HandleFunc("POST /admin/telegram/proxy", a.adminRequired(a.tgProxySave))
	mux.HandleFunc("POST /admin/telegram/api/sync", a.adminAPIRequired(a.tgSync))
}

// ---------------------------------------------------------------- 中间件

type handlerFunc func(http.ResponseWriter, *http.Request)

// adminRequired 页面守卫：未登录 302 到登录页并带回跳地址。
func (a *App) adminRequired(next handlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.Render.IsAdmin(r) {
			target := "/admin/login?next=" + urlQueryEscape(r.URL.Path)
			http.Redirect(w, r, target, http.StatusFound)
			return
		}
		next(w, r)
	}
}

// adminAPIRequired 接口守卫：未登录直接 401，不做跳转。
func (a *App) adminAPIRequired(next handlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.Render.IsAdmin(r) {
			JSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "message": "需要管理员登录"})
			return
		}
		next(w, r)
	}
}

// recoverer 兜住 panic，按路径决定返回 JSON 还是错误页。
func (a *App) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if p := recover(); p != nil {
				log.Printf("panic %s %s: %v", r.Method, r.URL.Path, p)
				if wantsJSON(r) {
					JSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "message": "服务端错误"})
					return
				}
				a.renderError(w, r, http.StatusInternalServerError, "服务端错误")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func wantsJSON(r *http.Request) bool {
	if strings.HasPrefix(r.URL.Path, "/api/") || strings.Contains(r.URL.Path, "/api/") {
		return true
	}
	accept := r.Header.Get("Accept")
	return strings.Contains(accept, "application/json") && !strings.Contains(accept, "text/html")
}

// renderError 统一错误页。
func (a *App) renderError(w http.ResponseWriter, r *http.Request, status int, detail string) {
	if wantsJSON(r) {
		JSON(w, status, map[string]any{"isSuccess": false, "errorMsg": detail})
		return
	}
	a.Render.HTML(w, status, "error.html", map[string]any{"Status": status, "Detail": detail})
}

// paramInt 取路径参数里的整数，非法时返回错误。
func paramInt(r *http.Request, name string) (int64, error) {
	return strconv.ParseInt(r.PathValue(name), 10, 64)
}

func queryInt(r *http.Request, name string, fallback int) int {
	n, err := strconv.Atoi(r.URL.Query().Get(name))
	if err != nil || n < 1 {
		return fallback
	}
	return n
}

func urlQueryEscape(s string) string {
	// 只用于回跳地址，标准库的 QueryEscape 会把 "/" 也转义，这里保持与 Python 版一致
	return strings.ReplaceAll(strings.ReplaceAll(s, "%", "%25"), "/", "%2F")
}

func pathInt(r *http.Request, name string) (int64, bool) {
	n, err := paramInt(r, name)
	if err != nil {
		return 0, false
	}
	return n, true
}
