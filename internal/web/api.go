package web

import (
	"net/http"
	"strings"

	"qingdeng/internal/config"
	"qingdeng/internal/store"
)

// 书源接口统一返回 {"isSuccess": true, "data": ...}；
// 失败返回 {"isSuccess": false, "errorMsg": "..."}。

func apiOK(w http.ResponseWriter, data any) {
	JSON(w, 200, map[string]any{"isSuccess": true, "data": data})
}

func apiFail(w http.ResponseWriter, status int, message string) {
	JSON(w, status, map[string]any{"isSuccess": false, "errorMsg": message})
}

// apiTokenOK API_TOKEN 未配置时公开；配置后接受 ?token= 或 Authorization 头。
func (a *App) apiTokenOK(r *http.Request) bool {
	if a.Cfg.APIToken == "" {
		return true
	}
	supplied := r.URL.Query().Get("token")
	if supplied == "" {
		header := r.Header.Get("Authorization")
		if strings.HasPrefix(strings.ToLower(header), "bearer ") {
			supplied = strings.TrimSpace(header[7:])
		} else {
			supplied = strings.TrimSpace(header)
		}
	}
	return hmacEqual(supplied, a.Cfg.APIToken)
}

func (a *App) apiURL(path string) string {
	base := a.Cfg.SiteBaseURL + path
	if a.Cfg.APIToken == "" {
		return base
	}
	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}
	return base + sep + "token=" + a.Cfg.APIToken
}

func (a *App) bookBrief(book store.Book) map[string]any {
	return map[string]any{
		"id":           book.ID,
		"name":         book.Title,
		"author":       book.Author,
		"intro":        book.Intro,
		"coverUrl":     book.CoverURL,
		"kind":         book.Category,
		"wordCount":    book.WordCount,
		"chapterCount": book.ChapterCount,
		"updateTime":   book.UpdatedAt,
		"bookUrl":      a.apiURL("/api/book/" + itoa64(book.ID)),
		"tocUrl":       a.apiURL("/api/book/" + itoa64(book.ID) + "/catalog"),
	}
}

func (a *App) apiSearch(w http.ResponseWriter, r *http.Request) {
	if !a.apiTokenOK(r) {
		apiFail(w, 401, "无效的 API token")
		return
	}
	key := strings.TrimSpace(r.URL.Query().Get("key"))
	if len([]rune(key)) > 60 {
		key = string([]rune(key)[:60])
	}
	page := queryInt(r, "page", 1)
	books, total, err := a.DB.ListBooks(r.Context(), store.BookFilter{
		Page: page, PageSize: config.APIPageSize, Keyword: key, Order: "updated",
	})
	if err != nil {
		apiFail(w, 500, "检索失败")
		return
	}
	list := make([]map[string]any, 0, len(books))
	for _, b := range books {
		list = append(list, a.bookBrief(b))
	}
	apiOK(w, map[string]any{"total": total, "page": page, "list": list})
}

func (a *App) apiBook(w http.ResponseWriter, r *http.Request) {
	if !a.apiTokenOK(r) {
		apiFail(w, 401, "无效的 API token")
		return
	}
	bookID, ok := pathInt(r, "id")
	if !ok {
		apiFail(w, 400, "书籍编号不对")
		return
	}
	ctx := r.Context()
	book, err := a.DB.GetBook(ctx, bookID)
	if err != nil {
		apiFail(w, 500, "读取失败")
		return
	}
	if book == nil {
		apiFail(w, 404, "书籍不存在")
		return
	}
	latest, err := a.DB.LastChapter(ctx, bookID)
	if err != nil {
		apiFail(w, 500, "读取失败")
		return
	}
	data := a.bookBrief(*book)
	if latest != nil {
		data["latestChapterTitle"] = latest.Title
	} else {
		data["latestChapterTitle"] = ""
	}
	apiOK(w, data)
}

func (a *App) apiCatalog(w http.ResponseWriter, r *http.Request) {
	if !a.apiTokenOK(r) {
		apiFail(w, 401, "无效的 API token")
		return
	}
	bookID, ok := pathInt(r, "id")
	if !ok {
		apiFail(w, 400, "书籍编号不对")
		return
	}
	ctx := r.Context()
	book, err := a.DB.GetBook(ctx, bookID)
	if err != nil || book == nil {
		apiFail(w, 404, "书籍不存在")
		return
	}
	page := queryInt(r, "page", 1)
	chapters, total, err := a.DB.ListChapters(ctx, bookID, page, config.CatalogPageSize)
	if err != nil {
		apiFail(w, 500, "读取目录失败")
		return
	}
	pages := (total + config.CatalogPageSize - 1) / config.CatalogPageSize
	if pages < 1 {
		pages = 1
	}
	if page > pages {
		page = pages
	}
	nextURL := ""
	if page < pages {
		nextURL = a.apiURL("/api/book/" + itoa64(bookID) + "/catalog?page=" + itoa(page+1))
	}
	list := make([]map[string]any, 0, len(chapters))
	for _, c := range chapters {
		list = append(list, map[string]any{
			"id":    c.ID,
			"idx":   c.Idx,
			"title": c.Title,
			"url":   a.apiURL("/api/chapter/" + itoa64(c.ID)),
		})
	}
	apiOK(w, map[string]any{
		"total": total, "page": page, "pages": pages, "nextUrl": nextURL, "list": list,
	})
}

func (a *App) apiChapter(w http.ResponseWriter, r *http.Request) {
	if !a.apiTokenOK(r) {
		apiFail(w, 401, "无效的 API token")
		return
	}
	chapterID, ok := pathInt(r, "id")
	if !ok {
		apiFail(w, 400, "章节编号不对")
		return
	}
	chapter, err := a.DB.GetChapter(r.Context(), chapterID)
	if err != nil {
		apiFail(w, 500, "读取失败")
		return
	}
	if chapter == nil {
		apiFail(w, 404, "章节不存在")
		return
	}
	apiOK(w, map[string]any{
		"id":        chapter.ID,
		"bookId":    chapter.BookID,
		"idx":       chapter.Idx,
		"title":     chapter.Title,
		"wordCount": chapter.WordCount,
		"content":   chapter.Content,
	})
}
