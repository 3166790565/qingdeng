package web

import (
	"context"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"

	"qingdeng/internal/config"
	"qingdeng/internal/importer"
	"qingdeng/internal/parser"
	"qingdeng/internal/store"
)

// ---------------------------------------------------------------- 登录

func (a *App) loginForm(w http.ResponseWriter, r *http.Request) {
	if a.Render.IsAdmin(r) {
		http.Redirect(w, r, "/admin", http.StatusFound)
		return
	}
	next := r.URL.Query().Get("next")
	if next == "" {
		next = "/admin"
	}
	a.Render.HTML(w, 200, "admin/login.html", map[string]any{
		"Next": next, "Error": "",
	})
}

func (a *App) login(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	password := r.FormValue("password")
	next := r.FormValue("next")

	if !a.Render.VerifyPassword(password) {
		a.Render.HTML(w, 401, "admin/login.html", map[string]any{
			"Next": next, "Error": "密码不对",
		})
		return
	}
	// 只允许跳回站内后台路径，挡开放重定向
	target := "/admin"
	if strings.HasPrefix(next, "/admin") {
		target = next
	}
	a.Render.SetSession(w)
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func (a *App) logout(w http.ResponseWriter, r *http.Request) {
	a.Render.ClearSession(w)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// ---------------------------------------------------------------- 概览

func (a *App) dashboard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	stats, err := a.DB.Stats(ctx)
	if err != nil {
		a.renderError(w, r, 500, "读取统计失败")
		return
	}
	modeCounts, _ := a.DB.CountBySplitMode(ctx)
	jobs, _ := a.DB.RecentJobs(ctx, 8)
	settings, _ := a.DB.LoadSettings(ctx)

	a.Render.HTML(w, 200, "admin/dashboard.html", map[string]any{
		"Active":     "home",
		"Stats":      stats,
		"ModeCounts": modeCounts,
		"Jobs":       jobs,
		"Settings":   settings,
		"Flash":      r.URL.Query().Get("flash"),
		"Kind":       r.URL.Query().Get("kind"),
	})
}

// ---------------------------------------------------------------- 上传

func (a *App) uploadForm(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	settings, _ := a.DB.LoadSettings(ctx)
	jobs, _ := a.DB.RecentJobs(ctx, 8)
	a.Render.HTML(w, 200, "admin/upload.html", map[string]any{
		"Active":    "upload",
		"Settings":  settings,
		"Jobs":      jobs,
		"ResumeJob": r.URL.Query().Get("job"),
		"Flash":     r.URL.Query().Get("flash"),
		"Kind":      r.URL.Query().Get("kind"),
	})
}

func (a *App) upload(w http.ResponseWriter, r *http.Request) {
	// 上传体积可能很大（ZIP 512MB），放开内存限制，超过部分落临时文件
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		JSON(w, 400, map[string]any{"ok": false, "message": "上传内容解析失败：" + err.Error()})
		return
	}
	files := r.MultipartForm.File["files"]
	var uploads []importer.Upload
	for _, header := range files {
		file, err := header.Open()
		if err != nil {
			continue
		}
		data, err := io.ReadAll(io.LimitReader(file, config.MaxZipTotalBytes))
		file.Close()
		if err != nil || len(data) == 0 {
			continue
		}
		uploads = append(uploads, importer.Upload{Name: cleanUploadName(header.Filename), Data: data})
	}
	if len(uploads) == 0 {
		JSON(w, 400, map[string]any{"ok": false, "message": "没有收到文件"})
		return
	}

	forceMode := r.FormValue("force_mode")
	targetChars := formInt(r, "target_chars")
	jobID, err := a.DB.CreateJob(r.Context(), len(uploads))
	if err != nil {
		JSON(w, 500, map[string]any{"ok": false, "message": "建任务失败：" + err.Error()})
		return
	}

	go a.Imp.RunImport(context.Background(), jobID, uploads, forceMode, targetChars)
	JSON(w, 200, map[string]any{"ok": true, "job_id": jobID, "files": len(uploads)})
}

func (a *App) jobStatus(w http.ResponseWriter, r *http.Request) {
	job, err := a.DB.GetJob(r.Context(), r.PathValue("id"))
	if err != nil {
		JSON(w, 500, map[string]any{"ok": false, "message": "读取任务失败"})
		return
	}
	if job == nil {
		JSON(w, 404, map[string]any{"ok": false, "message": "任务不存在"})
		return
	}
	JSON(w, 200, job)
}

func (a *App) jobPage(w http.ResponseWriter, r *http.Request) {
	job, err := a.DB.GetJob(r.Context(), r.PathValue("id"))
	if err != nil || job == nil {
		a.renderError(w, r, 404, "任务不存在")
		return
	}
	a.Render.HTML(w, 200, "admin/job.html", map[string]any{
		"Active": "upload", "Job": job,
	})
}

// ---------------------------------------------------------------- 书籍管理

func (a *App) bookList(w http.ResponseWriter, r *http.Request) {
	keyword := strings.TrimSpace(r.URL.Query().Get("q"))
	splitMode := r.URL.Query().Get("split_mode")
	order := r.URL.Query().Get("order")
	settings, _ := a.DB.LoadSettings(r.Context())
	books, total, err := a.DB.ListBooks(r.Context(), store.BookFilter{
		Page: queryInt(r, "page", 1), PageSize: config.AdminPageSize,
		SplitMode: splitMode, Keyword: keyword, Order: order,
	})
	if err != nil {
		a.renderError(w, r, 500, "读取书籍失败")
		return
	}
	a.Render.HTML(w, 200, "admin/books.html", map[string]any{
		"Active":    "books",
		"Books":     books,
		"Page":      NewPage(r.URL.Query().Get("page"), config.AdminPageSize, total),
		"SplitMode": splitMode,
		"Q":         keyword,
		"Order":     order,
		"Settings":  settings,
		"Flash":     r.URL.Query().Get("flash"),
		"Kind":      r.URL.Query().Get("kind"),
	})
}

func (a *App) bookEditForm(w http.ResponseWriter, r *http.Request) {
	bookID, ok := pathInt(r, "id")
	if !ok {
		a.renderError(w, r, 400, "书籍编号不对")
		return
	}
	ctx := r.Context()
	book, err := a.DB.GetBook(ctx, bookID)
	if err != nil || book == nil {
		a.renderError(w, r, 404, "书籍不存在")
		return
	}
	settings, _ := a.DB.LoadSettings(ctx)
	a.Render.HTML(w, 200, "admin/book_edit.html", map[string]any{
		"Active": "books", "Book": book, "Settings": settings,
		"Flash": r.URL.Query().Get("flash"), "Kind": r.URL.Query().Get("kind"),
	})
}

func (a *App) bookEdit(w http.ResponseWriter, r *http.Request) {
	bookID, ok := pathInt(r, "id")
	if !ok {
		a.renderError(w, r, 400, "书籍编号不对")
		return
	}
	ctx := r.Context()
	book, err := a.DB.GetBook(ctx, bookID)
	if err != nil || book == nil {
		a.renderError(w, r, 404, "书籍不存在")
		return
	}
	_ = r.ParseForm()
	title := strings.TrimSpace(r.FormValue("title"))
	if title == "" {
		flashRedirect(w, r, "/admin/books/"+itoa64(bookID)+"/edit", "书名不能为空", false)
		return
	}
	author := strings.TrimSpace(r.FormValue("author"))
	if author == "" {
		author = "未知"
	}
	err = a.DB.UpdateBook(ctx, bookID, map[string]string{
		"title":     title,
		"author":    author,
		"intro":     r.FormValue("intro"),
		"cover_url": r.FormValue("cover_url"),
		"category":  r.FormValue("category"),
	})
	if err != nil {
		flashRedirect(w, r, "/admin/books/"+itoa64(bookID)+"/edit", "保存失败："+err.Error(), false)
		return
	}
	flashRedirect(w, r, "/admin/books/"+itoa64(bookID)+"/edit", "已保存", true)
}

func (a *App) bookDelete(w http.ResponseWriter, r *http.Request) {
	bookID, ok := pathInt(r, "id")
	if !ok {
		a.renderError(w, r, 400, "书籍编号不对")
		return
	}
	ctx := r.Context()
	book, err := a.DB.GetBook(ctx, bookID)
	if err != nil || book == nil {
		a.renderError(w, r, 404, "书籍不存在")
		return
	}
	if err := a.DB.DeleteBook(ctx, bookID); err != nil {
		flashRedirect(w, r, "/admin/books", "删除失败："+err.Error(), false)
		return
	}
	flashRedirect(w, r, "/admin/books", "已删除《"+book.Title+"》", true)
}

// ---------------------------------------------------------------- 重新分章

func (a *App) resplitSingle(w http.ResponseWriter, r *http.Request) {
	bookID, ok := pathInt(r, "id")
	if !ok {
		a.renderError(w, r, 400, "书籍编号不对")
		return
	}
	_ = r.ParseForm()
	settings, _ := a.DB.LoadSettings(r.Context())
	opts := settings.ParseOptions(r.FormValue("force_mode"), formInt(r, "target_chars"))
	result := a.Imp.ResplitOne(r.Context(), bookID, opts)

	if result.Status != "ok" {
		message := result.Message
		if message == "" {
			message = "重新分章失败"
		}
		flashRedirect(w, r, "/admin/books/"+itoa64(bookID)+"/chapters", message, false)
		return
	}
	message := "已重新分章：" + result.ModeLabel + "，" + itoa(result.Chapters) + " 章"
	if result.Message != "" {
		message += "（" + result.Message + "）"
	}
	flashRedirect(w, r, "/admin/books/"+itoa64(bookID)+"/chapters", message, true)
}

func (a *App) resplitBatch(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	ids := strings.TrimSpace(r.FormValue("ids"))
	var selected []int64
	if ids != "" {
		fields := strings.FieldsFunc(ids, func(r rune) bool { return r == ',' || r == ' ' })
		for _, part := range fields {
			if id, ok := parseInt64(part); ok {
				selected = append(selected, id)
			}
		}
	} else {
		var err error
		selected, err = a.DB.BookIDs(r.Context(),
			r.FormValue("split_mode"), strings.TrimSpace(r.FormValue("q")))
		if err != nil {
			JSON(w, 500, map[string]any{"ok": false, "message": "读取书籍失败"})
			return
		}
	}
	if len(selected) == 0 {
		JSON(w, 400, map[string]any{"ok": false, "message": "没有匹配的书籍"})
		return
	}

	jobID, err := a.DB.CreateJob(r.Context(), len(selected))
	if err != nil {
		JSON(w, 500, map[string]any{"ok": false, "message": "建任务失败：" + err.Error()})
		return
	}
	go a.Imp.RunResplit(context.Background(), jobID, selected,
		r.FormValue("force_mode"), formInt(r, "target_chars"))
	JSON(w, 200, map[string]any{"ok": true, "job_id": jobID, "books": len(selected)})
}

// ---------------------------------------------------------------- 章节

func (a *App) chapterList(w http.ResponseWriter, r *http.Request) {
	bookID, ok := pathInt(r, "id")
	if !ok {
		a.renderError(w, r, 400, "书籍编号不对")
		return
	}
	ctx := r.Context()
	book, err := a.DB.GetBook(ctx, bookID)
	if err != nil || book == nil {
		a.renderError(w, r, 404, "书籍不存在")
		return
	}
	chapters, total, err := a.DB.ListChapters(ctx, bookID, queryInt(r, "page", 1), config.AdminPageSize)
	if err != nil {
		a.renderError(w, r, 500, "读取目录失败")
		return
	}
	settings, _ := a.DB.LoadSettings(ctx)
	a.Render.HTML(w, 200, "admin/chapters.html", map[string]any{
		"Active": "books", "Book": book, "Chapters": chapters,
		"Page":     NewPage(r.URL.Query().Get("page"), config.AdminPageSize, total),
		"Settings": settings,
		"Flash":    r.URL.Query().Get("flash"),
		"Kind":     r.URL.Query().Get("kind"),
	})
}

func (a *App) chapterEditForm(w http.ResponseWriter, r *http.Request) {
	chapterID, ok := pathInt(r, "id")
	if !ok {
		a.renderError(w, r, 400, "章节编号不对")
		return
	}
	ctx := r.Context()
	chapter, err := a.DB.GetChapter(ctx, chapterID)
	if err != nil || chapter == nil {
		a.renderError(w, r, 404, "章节不存在")
		return
	}
	book, _ := a.DB.GetBook(ctx, chapter.BookID)
	a.Render.HTML(w, 200, "admin/chapter_edit.html", map[string]any{
		"Active": "books", "Chapter": chapter, "Book": book,
		"Flash": r.URL.Query().Get("flash"), "Kind": r.URL.Query().Get("kind"),
	})
}

func (a *App) chapterEdit(w http.ResponseWriter, r *http.Request) {
	chapterID, ok := pathInt(r, "id")
	if !ok {
		a.renderError(w, r, 400, "章节编号不对")
		return
	}
	ctx := r.Context()
	chapter, err := a.DB.GetChapter(ctx, chapterID)
	if err != nil || chapter == nil {
		a.renderError(w, r, 404, "章节不存在")
		return
	}
	_ = r.ParseForm()
	content := r.FormValue("content")
	if strings.TrimSpace(content) == "" {
		flashRedirect(w, r, "/admin/chapters/"+itoa64(chapterID)+"/edit", "正文不能为空", false)
		return
	}
	if err := a.DB.UpdateChapter(ctx, chapterID, r.FormValue("title"), content); err != nil {
		flashRedirect(w, r, "/admin/chapters/"+itoa64(chapterID)+"/edit", "保存失败："+err.Error(), false)
		return
	}
	flashRedirect(w, r, "/admin/chapters/"+itoa64(chapterID)+"/edit", "已保存", true)
}

// ---------------------------------------------------------------- 解析设置

func (a *App) settingsForm(w http.ResponseWriter, r *http.Request) {
	settings, _ := a.DB.LoadSettings(r.Context())
	a.Render.HTML(w, 200, "admin/settings.html", map[string]any{
		"Active": "settings", "Settings": settings,
		"Flash": r.URL.Query().Get("flash"), "Kind": r.URL.Query().Get("kind"),
	})
}

func (a *App) settingsSave(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	target := formInt(r, "target_chars")
	if target < 500 {
		target = 500
	}
	if target > 50000 {
		target = 50000
	}
	err := a.DB.SaveSettings(r.Context(), map[string]any{
		"target_chars":       target,
		"strip_header_block": formBool(r, "strip_header_block"),
		"fix_yao_variant":    formBool(r, "fix_yao_variant"),
		"add_subtitle":       formBool(r, "add_subtitle"),
	})
	if err != nil {
		flashRedirect(w, r, "/admin/settings", "保存失败："+err.Error(), false)
		return
	}
	flashRedirect(w, r, "/admin/settings", "设置已保存，对之后的导入与重新分章生效", true)
}

// ---------------------------------------------------------------- 小工具

func parseInt64(s string) (int64, bool) {
	var v int64
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
		v = v*10 + int64(s[i]-'0')
	}
	return v, s != ""
}

// cleanUploadName 还原被 multipart 当原始字节塞进来的文件名。
//
// Windows 上的浏览器提交中文文件名时不保证用 UTF-8：multipart 头里的 filename
// 可能是 GBK 字节。Go 会把这段字节原样当 string，直接入库就会变成乱码。
// 合法 UTF-8 原样返回；否则按 GB18030 试解一次。
func cleanUploadName(name string) string {
	if name == "" {
		return "未命名.txt"
	}
	if utf8.ValidString(name) {
		return name
	}
	if decoded, enc := parser.DecodeBytes([]byte(name)); enc == "gb18030" {
		return decoded
	}
	return name
}
