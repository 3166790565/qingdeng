// Package store 的查询层：书籍与章节的数据访问。
package store

import (
	"context"
	"database/sql"
	"fmt"
	"html"
	"strings"
	"time"

	"html/template"

	"qingdeng/internal/parser"
)

// SplitModes 三态模式，与 parser 的输出一一对应。
var SplitModes = []string{"marked", "single", "auto"}

// SplitModeLabels 给模板用的中文名。
var SplitModeLabels = map[string]string{
	"marked": "标记分章",
	"single": "整本单章",
	"auto":   "按字数分节",
}

// Book 书籍行。
type Book struct {
	ID             int64
	Title          string
	Author         string
	Intro          string
	CoverURL       string
	Category       string
	WordCount      int
	ChapterCount   int
	SplitMode      string
	SourceFilename string
	CreatedAt      string
	UpdatedAt      string
}

// ChapterRow 目录/列表用的轻量章节行。
type ChapterRow struct {
	ID        int64
	Idx       int
	Title     string
	WordCount int
}

// Chapter 完整章节。
type Chapter struct {
	ID        int64
	BookID    int64
	Idx       int
	Title     string
	Content   string
	WordCount int
}

// Job 导入任务。
type Job struct {
	ID        string
	Status    string
	Total     int
	Done      int
	OK        int
	Failed    int
	Log       []JobRecord
	CreatedAt string
}

// JobRecord 任务日志里的一条记录。
type JobRecord struct {
	File string `json:"file"`
	// Path 只在批量导入时填，记落盘路径用于断点续跑；后台不展示
	Path      string `json:"path,omitempty"`
	Status    string `json:"status"` // ok | skip | error
	Message   string `json:"message,omitempty"`
	Title     string `json:"title,omitempty"`
	Author    string `json:"author,omitempty"`
	Mode      string `json:"mode,omitempty"`
	ModeLabel string `json:"mode_label,omitempty"`
	Chapters  int    `json:"chapters,omitempty"`
	Words     int    `json:"words,omitempty"`
	Encoding  string `json:"encoding,omitempty"`
	BookID    int64  `json:"book_id,omitempty"`
}

// Channel TG 频道。
type Channel struct {
	ID            int64
	Title         string
	Username      string
	ChatID        int64
	Enabled       bool
	LastMessageID int
	LastSyncAt    string
	CreatedAt     string
}

// Now 全站统一的时间戳格式。
func Now() string { return time.Now().Format("2006-01-02 15:04:05") }

func scanBook(sc interface{ Scan(...any) error }) (Book, error) {
	var b Book
	err := sc.Scan(&b.ID, &b.Title, &b.Author, &b.Intro, &b.CoverURL, &b.Category,
		&b.WordCount, &b.ChapterCount, &b.SplitMode, &b.SourceFilename,
		&b.CreatedAt, &b.UpdatedAt)
	return b, err
}

const bookColumns = `id, title, COALESCE(author,''), COALESCE(intro,''), COALESCE(cover_url,''),
	COALESCE(category,''), word_count, chapter_count, split_mode,
	COALESCE(source_filename,''), COALESCE(created_at,''), COALESCE(updated_at,'')`

// MakeIntro 取首章前 140 字当简介。
func MakeIntro(parsed *parser.ParsedBook, limit int) string {
	if len(parsed.Chapters) == 0 {
		return ""
	}
	text := strings.Join(strings.Fields(parsed.Chapters[0].Content), "")
	r := []rune(text)
	if len(r) <= limit {
		return text
	}
	return string(r[:limit]) + "…"
}

// InsertBook 在一个事务里写 books + chapters，返回新书 id。
func (d *DB) InsertBook(ctx context.Context, parsed *parser.ParsedBook, sourceFilename, contentHash string) (int64, error) {
	ts := Now()
	var bookID int64
	err := d.Tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`INSERT INTO books(title, author, intro, cover_url, category, word_count,
			   chapter_count, split_mode, source_filename, content_hash, created_at, updated_at)
			 VALUES(?,?,?,'','',?,?,?,?,?,?,?)`,
			parsed.Title, parsed.Author, MakeIntro(parsed, 140), parsed.WordCount,
			len(parsed.Chapters), parsed.SplitMode, sourceFilename, contentHash, ts, ts)
		if err != nil {
			return err
		}
		bookID, err = res.LastInsertId()
		if err != nil {
			return err
		}
		return insertChapters(ctx, tx, bookID, parsed.Chapters)
	})
	return bookID, err
}

func insertChapters(ctx context.Context, tx *sql.Tx, bookID int64, chapters []parser.Chapter) error {
	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO chapters(book_id, idx, title, content, word_count) VALUES(?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, c := range chapters {
		if _, err := stmt.ExecContext(ctx, bookID, c.Idx, c.Title, c.Content, c.WordCount); err != nil {
			return fmt.Errorf("写入第 %d 章失败：%w", c.Idx, err)
		}
	}
	return nil
}

// ReplaceChapters 重新分章：换掉章节，书名作者等人工改过的字段保持不动。
func (d *DB) ReplaceChapters(ctx context.Context, bookID int64, parsed *parser.ParsedBook) error {
	return d.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM chapters WHERE book_id = ?`, bookID); err != nil {
			return err
		}
		if err := insertChapters(ctx, tx, bookID, parsed.Chapters); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx,
			`UPDATE books SET word_count=?, chapter_count=?, split_mode=?, updated_at=? WHERE id=?`,
			parsed.WordCount, len(parsed.Chapters), parsed.SplitMode, Now(), bookID)
		return err
	})
}

// EditableFields 后台允许编辑的字段。
var EditableFields = []string{"title", "author", "intro", "cover_url", "category"}

// UpdateBook 只更新白名单字段。
func (d *DB) UpdateBook(ctx context.Context, bookID int64, fields map[string]string) error {
	var assignments []string
	var args []any
	for _, key := range EditableFields {
		value, ok := fields[key]
		if !ok {
			continue
		}
		assignments = append(assignments, key+" = ?")
		args = append(args, strings.TrimSpace(value))
	}
	if len(assignments) == 0 {
		return nil
	}
	args = append(args, Now(), bookID)
	_, err := d.ExecContext(ctx,
		"UPDATE books SET "+strings.Join(assignments, ", ")+", updated_at = ? WHERE id = ?", args...)
	return err
}

// UpdateChapter 正文按行传入，统一成"段落之间一个 \n"的存储格式。
func (d *DB) UpdateChapter(ctx context.Context, chapterID int64, title, content string) error {
	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	var paragraphs []string
	for _, ln := range lines {
		if p := strings.TrimSpace(ln); p != "" {
			paragraphs = append(paragraphs, p)
		}
	}
	body := strings.Join(paragraphs, "\n")
	if strings.TrimSpace(title) == "" {
		title = "无题"
	}
	words := parser.CountWords(body)

	var bookID int64
	err := d.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`UPDATE chapters SET title = ?, content = ?, word_count = ? WHERE id = ?`,
			strings.TrimSpace(title), body, words, chapterID); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx,
			`SELECT book_id FROM chapters WHERE id = ?`, chapterID).Scan(&bookID); err != nil {
			return err
		}
		return recountBook(ctx, tx, bookID)
	})
	return err
}

func recountBook(ctx context.Context, q interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, bookID int64) error {
	_, err := q.ExecContext(ctx,
		`UPDATE books SET
		   word_count = COALESCE((SELECT SUM(word_count) FROM chapters WHERE book_id = ?), 0),
		   chapter_count = (SELECT COUNT(*) FROM chapters WHERE book_id = ?),
		   updated_at = ?
		 WHERE id = ?`, bookID, bookID, Now(), bookID)
	return err
}

// DeleteBook 章节由外键级联删除，FTS 索引由 trigger 同步。
func (d *DB) DeleteBook(ctx context.Context, bookID int64) error {
	_, err := d.ExecContext(ctx, `DELETE FROM books WHERE id = ?`, bookID)
	return err
}

// Ping 健康检查用：确认库能打开、schema 在，返回书籍数。
// 只查 books 的行数，不碰 chapters 与 FTS 表，几十 G 的库也是毫秒级。
func (d *DB) Ping(ctx context.Context) (int, error) {
	var n int
	err := d.QueryRowContext(ctx, `SELECT COUNT(*) FROM books`).Scan(&n)
	return n, err
}

var bookOrders = map[string]string{
	"updated": "updated_at DESC, id DESC",
	"created": "created_at DESC, id DESC",
	"title":   "title ASC, id ASC",
	"words":   "word_count DESC, id DESC",
}

// BookFilter 列表筛选条件。
type BookFilter struct {
	SplitMode string
	Keyword   string
	Order     string
	Page      int
	PageSize  int
}

func (f BookFilter) where() (string, []any) {
	var conditions []string
	var args []any
	if isSplitMode(f.SplitMode) {
		conditions = append(conditions, "split_mode = ?")
		args = append(args, f.SplitMode)
	}
	if f.Keyword != "" {
		conditions = append(conditions, "(title LIKE ? OR author LIKE ?)")
		like := "%" + f.Keyword + "%"
		args = append(args, like, like)
	}
	if len(conditions) == 0 {
		return "", nil
	}
	return "WHERE " + strings.Join(conditions, " AND "), args
}

func isSplitMode(mode string) bool {
	for _, m := range SplitModes {
		if m == mode {
			return true
		}
	}
	return false
}

// ListBooks 分页列表，返回当页与总数。越界页码收回到最后一页。
func (d *DB) ListBooks(ctx context.Context, f BookFilter) ([]Book, int, error) {
	where, args := f.where()
	var total int
	if err := d.QueryRowContext(ctx, "SELECT COUNT(*) FROM books "+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	pages := (total + f.PageSize - 1) / f.PageSize
	if pages < 1 {
		pages = 1
	}
	page := f.Page
	if page < 1 {
		page = 1
	}
	if page > pages {
		page = pages
	}
	order, ok := bookOrders[f.Order]
	if !ok {
		order = bookOrders["updated"]
	}
	rows, err := d.QueryContext(ctx,
		"SELECT "+bookColumns+" FROM books "+where+" ORDER BY "+order+" LIMIT ? OFFSET ?",
		append(args, f.PageSize, (page-1)*f.PageSize)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var books []Book
	for rows.Next() {
		b, err := scanBook(rows)
		if err != nil {
			return nil, 0, err
		}
		books = append(books, b)
	}
	return books, total, rows.Err()
}

// GetBook 按 id 取书。
func (d *DB) GetBook(ctx context.Context, bookID int64) (*Book, error) {
	row := d.QueryRowContext(ctx, "SELECT "+bookColumns+" FROM books WHERE id = ?", bookID)
	b, err := scanBook(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &b, nil
}

// GetBookByHash 内容去重查询。
func (d *DB) GetBookByHash(ctx context.Context, hash string) (*Book, error) {
	row := d.QueryRowContext(ctx, "SELECT "+bookColumns+" FROM books WHERE content_hash = ?", hash)
	b, err := scanBook(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &b, nil
}

// Stats 首页/概览用的计数。
type Stats struct {
	Books    int
	Chapters int
	Words    int
}

// Stats 汇总。
func (d *DB) Stats(ctx context.Context) (Stats, error) {
	var s Stats
	err := d.QueryRowContext(ctx,
		`SELECT (SELECT COUNT(*) FROM books),
		        (SELECT COUNT(*) FROM chapters),
		        (SELECT COALESCE(SUM(word_count),0) FROM books)`).Scan(&s.Books, &s.Chapters, &s.Words)
	return s, err
}

// CountBySplitMode 分章模式分布。
func (d *DB) CountBySplitMode(ctx context.Context) (map[string]int, error) {
	counts := map[string]int{"marked": 0, "single": 0, "auto": 0}
	rows, err := d.QueryContext(ctx, `SELECT COALESCE(split_mode,''), COUNT(*) FROM books GROUP BY split_mode`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var mode string
		var n int
		if err := rows.Scan(&mode, &n); err != nil {
			return nil, err
		}
		counts[mode] = n
	}
	return counts, rows.Err()
}

// BookIDs 批量重新分章要用的 id 清单，条件与后台列表保持一致。
func (d *DB) BookIDs(ctx context.Context, splitMode, keyword string) ([]int64, error) {
	f := BookFilter{SplitMode: splitMode, Keyword: keyword}
	where, args := f.where()
	rows, err := d.QueryContext(ctx, "SELECT id FROM books "+where+" ORDER BY id", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ListChapters 目录列表。pageSize <= 0 时不分页。
func (d *DB) ListChapters(ctx context.Context, bookID int64, page, pageSize int) ([]ChapterRow, int, error) {
	var total int
	if err := d.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM chapters WHERE book_id = ?`, bookID).Scan(&total); err != nil {
		return nil, 0, err
	}
	sqlText := `SELECT id, idx, title, word_count FROM chapters WHERE book_id = ? ORDER BY idx`
	var args []any
	args = append(args, bookID)
	if pageSize > 0 {
		pages := (total + pageSize - 1) / pageSize
		if pages < 1 {
			pages = 1
		}
		if page < 1 {
			page = 1
		}
		if page > pages {
			page = pages
		}
		sqlText += " LIMIT ? OFFSET ?"
		args = append(args, pageSize, (page-1)*pageSize)
	}
	rows, err := d.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var list []ChapterRow
	for rows.Next() {
		var c ChapterRow
		if err := rows.Scan(&c.ID, &c.Idx, &c.Title, &c.WordCount); err != nil {
			return nil, 0, err
		}
		list = append(list, c)
	}
	return list, total, rows.Err()
}

// GetChapter 按主键取完整章节。
func (d *DB) GetChapter(ctx context.Context, chapterID int64) (*Chapter, error) {
	row := d.QueryRowContext(ctx,
		`SELECT id, book_id, idx, title, content, word_count FROM chapters WHERE id = ?`, chapterID)
	return scanChapter(row)
}

// GetChapterByIdx 按书 + 序号取章节。
func (d *DB) GetChapterByIdx(ctx context.Context, bookID int64, idx int) (*Chapter, error) {
	row := d.QueryRowContext(ctx,
		`SELECT id, book_id, idx, title, content, word_count FROM chapters
		  WHERE book_id = ? AND idx = ?`, bookID, idx)
	return scanChapter(row)
}

func scanChapter(sc interface{ Scan(...any) error }) (*Chapter, error) {
	var c Chapter
	err := sc.Scan(&c.ID, &c.BookID, &c.Idx, &c.Title, &c.Content, &c.WordCount)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// FirstChapterIdx 第一章序号，没有正文时返回 0 与 false。
func (d *DB) FirstChapterIdx(ctx context.Context, bookID int64) (int, bool, error) {
	var idx sql.NullInt64
	if err := d.QueryRowContext(ctx,
		`SELECT MIN(idx) FROM chapters WHERE book_id = ?`, bookID).Scan(&idx); err != nil {
		return 0, false, err
	}
	return int(idx.Int64), idx.Valid, nil
}

// LastChapter 最新章节（详情页展示用）。
func (d *DB) LastChapter(ctx context.Context, bookID int64) (*ChapterRow, error) {
	var c ChapterRow
	err := d.QueryRowContext(ctx,
		`SELECT id, idx, title, word_count FROM chapters WHERE book_id = ?
		  ORDER BY idx DESC LIMIT 1`, bookID).Scan(&c.ID, &c.Idx, &c.Title, &c.WordCount)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// Neighbors 上一章与下一章（只取翻章需要的字段）。
func (d *DB) Neighbors(ctx context.Context, bookID int64, idx int) (prev, next *ChapterRow, err error) {
	read := func(q string) (*ChapterRow, error) {
		var c ChapterRow
		e := d.QueryRowContext(ctx, q, bookID, idx).Scan(&c.ID, &c.Idx, &c.Title, &c.WordCount)
		if e == sql.ErrNoRows {
			return nil, nil
		}
		if e != nil {
			return nil, e
		}
		return &c, nil
	}
	if prev, err = read(`SELECT id, idx, title, word_count FROM chapters
		WHERE book_id = ? AND idx < ? ORDER BY idx DESC LIMIT 1`); err != nil {
		return nil, nil, err
	}
	if next, err = read(`SELECT id, idx, title, word_count FROM chapters
		WHERE book_id = ? AND idx > ? ORDER BY idx ASC LIMIT 1`); err != nil {
		return nil, nil, err
	}
	return prev, next, nil
}

// ChapterSourceText 把库里的正文拼回"可再次解析"的文本，供重新分章使用。
//
// 段落之间留空行，避免二次解析时把段落错误地当作硬换行合并。
// marked 书才把章名写回去，auto/single 的章名是生成的，写回去会污染标记识别。
func (d *DB) ChapterSourceText(ctx context.Context, bookID int64) (string, error) {
	book, err := d.GetBook(ctx, bookID)
	if err != nil || book == nil {
		return "", err
	}
	keepTitles := book.SplitMode == "marked"
	rows, err := d.QueryContext(ctx,
		`SELECT title, content FROM chapters WHERE book_id = ? ORDER BY idx`, bookID)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var parts []string
	for rows.Next() {
		var title, content string
		if err := rows.Scan(&title, &content); err != nil {
			return "", err
		}
		if keepTitles {
			parts = append(parts, title)
		}
		parts = append(parts, strings.ReplaceAll(content, "\n", "\n\n"))
	}
	return strings.Join(parts, "\n\n"), rows.Err()
}

// ---------------------------------------------------------------- 搜索

const (
	hlOpen  = "\x02"
	hlClose = "\x03"
	// FTSMinChars trigram 分词器要求匹配串至少 3 字
	FTSMinChars = 3
)

// SearchHit 全文检索命中。
type SearchHit struct {
	ChapterID    int64
	BookID       int64
	Idx          int
	ChapterTitle string
	BookTitle    string
	Author       string
	SplitMode    string
	Frag         string        // 已转义并带 <mark> 标记的纯文本
	FragHTML     template.HTML // 同上，但类型是 template.HTML，模板里可直出
}

func ftsMatch(keyword string) string {
	return `"` + strings.ReplaceAll(keyword, `"`, `""`) + `"`
}

// highlight 先转义再把哨兵换成 <mark>，模板里可以放心直出。
func highlight(fragment string) string {
	return strings.ReplaceAll(strings.ReplaceAll(html.EscapeString(fragment), hlOpen, "<mark>"), hlClose, "</mark>")
}

func fallbackFragment(content, keyword string, width int) string {
	pos := strings.Index(content, keyword)
	if pos < 0 {
		return truncate(content, width)
	}
	// 按字节下标取窗口时可能切在多字节字符中间，这里按 rune 重新夹取
	r := []rune(content)
	rpos := len([]rune(content[:pos]))
	start := rpos - width/2
	if start < 0 {
		start = 0
	}
	end := rpos + len([]rune(keyword)) + width/2
	if end > len(r) {
		end = len(r)
	}
	piece := strings.ReplaceAll(string(r[start:end]), "\n", " ")
	piece = strings.ReplaceAll(piece, keyword, hlOpen+keyword+hlClose)
	if start > 0 {
		piece = "…" + piece
	}
	if end < len(r) {
		piece += "…"
	}
	return piece
}

func truncate(s string, width int) string {
	r := []rune(s)
	if len(r) <= width {
		return s
	}
	return string(r[:width])
}

// SearchBooks 书名/作者模糊匹配。
func (d *DB) SearchBooks(ctx context.Context, keyword string, page, pageSize int) ([]Book, int, error) {
	return d.ListBooks(ctx, BookFilter{
		Keyword: strings.TrimSpace(keyword), Page: page, PageSize: pageSize, Order: "updated",
	})
}

// SearchChapters 全文检索。关键词短于 3 字时 trigram 用不上，退回 LIKE。
func (d *DB) SearchChapters(ctx context.Context, keyword string, page, pageSize int) ([]SearchHit, int, error) {
	keyword = strings.TrimSpace(keyword)
	if keyword == "" {
		return nil, 0, nil
	}
	offset := (page - 1) * pageSize
	if offset < 0 {
		offset = 0
	}

	var (
		total int
		rows  *sql.Rows
		err   error
	)
	if len([]rune(keyword)) >= FTSMinChars {
		match := ftsMatch(keyword)
		if err = d.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM chapters_fts WHERE chapters_fts MATCH ?`, match).Scan(&total); err != nil {
			return nil, 0, err
		}
		rows, err = d.QueryContext(ctx, `
			SELECT c.id, c.book_id, c.idx, c.title, b.title,
			       COALESCE(b.author,''), b.split_mode,
			       snippet(chapters_fts, 1, char(2), char(3), '…', 16)
			  FROM chapters_fts
			  JOIN chapters c ON c.id = chapters_fts.rowid
			  JOIN books b ON b.id = c.book_id
			 WHERE chapters_fts MATCH ?
			 ORDER BY rank
			 LIMIT ? OFFSET ?`, match, pageSize, offset)
	} else {
		like := "%" + keyword + "%"
		if err = d.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM chapters WHERE content LIKE ?`, like).Scan(&total); err != nil {
			return nil, 0, err
		}
		rows, err = d.QueryContext(ctx, `
			SELECT c.id, c.book_id, c.idx, c.title, b.title,
			       COALESCE(b.author,''), b.split_mode, c.content
			  FROM chapters c JOIN books b ON b.id = c.book_id
			 WHERE c.content LIKE ?
			 ORDER BY c.book_id, c.idx
			 LIMIT ? OFFSET ?`, like, pageSize, offset)
	}
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var hits []SearchHit
	for rows.Next() {
		var h SearchHit
		var frag string
		if err := rows.Scan(&h.ChapterID, &h.BookID, &h.Idx, &h.ChapterTitle, &h.BookTitle,
			&h.Author, &h.SplitMode, &frag); err != nil {
			return nil, 0, err
		}
		if len([]rune(keyword)) < FTSMinChars {
			frag = fallbackFragment(frag, keyword, 80)
		}
		h.Frag = highlight(frag)
		h.FragHTML = template.HTML(h.Frag)
		hits = append(hits, h)
	}
	return hits, total, rows.Err()
}

// ---------------------------------------------------------------- 设置

// GetSetting 读 app_settings 里一个键。
func (d *DB) GetSetting(ctx context.Context, key, fallback string) (string, error) {
	var value sql.NullString
	err := d.QueryRowContext(ctx, `SELECT value FROM app_settings WHERE key = ?`, key).Scan(&value)
	if err == sql.ErrNoRows {
		return fallback, nil
	}
	if err != nil {
		return fallback, err
	}
	if !value.Valid {
		return fallback, nil
	}
	return value.String, nil
}

// SetSetting 写一个键。
func (d *DB) SetSetting(ctx context.Context, key, value string) error {
	_, err := d.ExecContext(ctx,
		`INSERT INTO app_settings(key, value) VALUES(?,?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// DeleteSetting 删一个键。
func (d *DB) DeleteSetting(ctx context.Context, key string) error {
	_, err := d.ExecContext(ctx, `DELETE FROM app_settings WHERE key = ?`, key)
	return err
}
