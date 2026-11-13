package web

import (
	"net/http"
	"strings"

	"qingdeng/internal/config"
	"qingdeng/internal/store"
)

// ---------------------------------------------------------------- 探针与书源

func (a *App) healthz(w http.ResponseWriter, r *http.Request) {
	books, err := a.DB.Ping(r.Context())
	if err != nil {
		JSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	JSON(w, http.StatusOK, map[string]any{"ok": true, "books": books})
}

func (a *App) legadoSource(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Disposition", `inline; filename="source.json"`)
	JSON(w, http.StatusOK, BuildBookSource(a.Cfg, "", ""))
}

// apiDocs 是给书源配置者看的接口说明，替代 FastAPI 自动生成的 /api/docs。
func (a *App) apiDocs(w http.ResponseWriter, r *http.Request) {
	a.Render.HTML(w, http.StatusOK, "api_docs.html", map[string]any{
		"BaseURL":  a.Cfg.SiteBaseURL,
		"HasToken": a.Cfg.APIToken != "",
	})
}

// ---------------------------------------------------------------- 前台页面

func (a *App) index(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	recent, _, err := a.DB.ListBooks(ctx, store.BookFilter{Page: 1, PageSize: 12, Order: "updated"})
	if err != nil {
		a.renderError(w, r, 500, "读取书架失败")
		return
	}
	longest, _, err := a.DB.ListBooks(ctx, store.BookFilter{Page: 1, PageSize: 6, Order: "words"})
	if err != nil {
		a.renderError(w, r, 500, "读取书架失败")
		return
	}
	stats, err := a.DB.Stats(ctx)
	if err != nil {
		a.renderError(w, r, 500, "读取统计失败")
		return
	}
	a.Render.HTML(w, 200, "index.html", map[string]any{
		"Recent":  recent,
		"Longest": longest,
		"Stats":   stats,
		"Page":    Page{Number: 1, Size: 12},
		"Q":       "",
	})
}

func (a *App) shelf(w http.ResponseWriter, r *http.Request) {
	keyword := strings.TrimSpace(r.URL.Query().Get("q"))
	if len([]rune(keyword)) > 60 {
		keyword = string([]rune(keyword)[:60])
	}
	order := r.URL.Query().Get("order")
	page := queryInt(r, "page", 1)
	books, total, err := a.DB.ListBooks(r.Context(), store.BookFilter{
		Page: page, PageSize: config.ShelfPageSize, Keyword: keyword, Order: order,
	})
	if err != nil {
		a.renderError(w, r, 500, "读取书架失败")
		return
	}
	a.Render.HTML(w, 200, "shelf.html", map[string]any{
		"Books": books,
		"Page":  NewPage(r.URL.Query().Get("page"), config.ShelfPageSize, total),
		"Order": order,
		"Q":     keyword,
	})
}

func (a *App) bookDetail(w http.ResponseWriter, r *http.Request) {
	bookID, ok := pathInt(r, "id")
	if !ok {
		a.renderError(w, r, 400, "书籍编号不对")
		return
	}
	ctx := r.Context()
	book, err := a.DB.GetBook(ctx, bookID)
	if err != nil {
		a.renderError(w, r, 500, "读取书籍失败")
		return
	}
	if book == nil {
		a.renderError(w, r, 404, "这本书不存在")
		return
	}

	single := book.ChapterCount <= 1
	var chapters []store.ChapterRow
	total := book.ChapterCount
	if !single { // 单章书隐藏目录，直接开读
		chapters, total, err = a.DB.ListChapters(ctx, bookID, queryInt(r, "page", 1), config.CatalogPageSize)
		if err != nil {
			a.renderError(w, r, 500, "读取目录失败")
			return
		}
	}
	firstIdx, hasFirst, _ := a.DB.FirstChapterIdx(ctx, bookID)

	a.Render.HTML(w, 200, "book.html", map[string]any{
		"Book":     book,
		"Chapters": chapters,
		"Single":   single,
		"HasFirst": hasFirst,
		"FirstIdx": firstIdx,
		"Page":     NewPage(r.URL.Query().Get("page"), config.CatalogPageSize, total),
	})
}

func (a *App) readFirst(w http.ResponseWriter, r *http.Request) {
	bookID, ok := pathInt(r, "id")
	if !ok {
		a.renderError(w, r, 400, "书籍编号不对")
		return
	}
	idx, has, err := a.DB.FirstChapterIdx(r.Context(), bookID)
	if err != nil {
		a.renderError(w, r, 500, "读取章节失败")
		return
	}
	if !has {
		a.renderError(w, r, 404, "这本书还没有正文")
		return
	}
	http.Redirect(w, r, "/book/"+itoa64(bookID)+"/read/"+itoa(idx), http.StatusFound)
}

func (a *App) reader(w http.ResponseWriter, r *http.Request) {
	bookID, ok1 := pathInt(r, "id")
	idx64, err := paramInt(r, "idx")
	if !ok1 || err != nil {
		a.renderError(w, r, 400, "章节编号不对")
		return
	}
	chapterIdx := int(idx64)
	ctx := r.Context()
	chapter, err := a.DB.GetChapterByIdx(ctx, bookID, chapterIdx)
	if err != nil {
		a.renderError(w, r, 500, "读取章节失败")
		return
	}
	if chapter == nil {
		a.renderError(w, r, 404, "这一章不存在")
		return
	}
	book, err := a.DB.GetBook(ctx, bookID)
	if err != nil || book == nil {
		a.renderError(w, r, 404, "这本书不存在")
		return
	}
	prev, next, err := a.DB.Neighbors(ctx, bookID, chapterIdx)
	if err != nil {
		a.renderError(w, r, 500, "读取章节失败")
		return
	}

	var paragraphs []string
	for _, p := range strings.Split(chapter.Content, "\n") {
		if p != "" {
			paragraphs = append(paragraphs, p)
		}
	}

	a.Render.HTML(w, 200, "reader.html", map[string]any{
		"Book":       book,
		"Chapter":    chapter,
		"Paragraphs": paragraphs,
		"Prev":       prev,
		"Next":       next,
		"Single":     book.ChapterCount <= 1,
	})
}

func (a *App) search(w http.ResponseWriter, r *http.Request) {
	keyword := strings.TrimSpace(r.URL.Query().Get("q"))
	if len([]rune(keyword)) > 60 {
		keyword = string([]rune(keyword)[:60])
	}
	mode := "title"
	if r.URL.Query().Get("mode") == "full" {
		mode = "full"
	}
	page := queryInt(r, "page", 1)

	ctx := r.Context()
	var (
		books []store.Book
		hits  []store.SearchHit
		total int
		err   error
	)
	if keyword != "" {
		if mode == "full" {
			hits, total, err = a.DB.SearchChapters(ctx, keyword, page, config.APIPageSize)
		} else {
			books, total, err = a.DB.SearchBooks(ctx, keyword, page, config.APIPageSize)
		}
		if err != nil {
			a.renderError(w, r, 500, "检索失败")
			return
		}
	}

	a.Render.HTML(w, 200, "search.html", map[string]any{
		"Q":      keyword,
		"Mode":   mode,
		"Books":  books,
		"Hits":   hits,
		"Total":  total,
		"Page":   NewPage(r.URL.Query().Get("page"), config.APIPageSize, total),
		"FTSMin": store.FTSMinChars,
	})
}
