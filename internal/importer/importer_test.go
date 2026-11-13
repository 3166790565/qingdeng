package importer

import (
	"archive/zip"
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"qingdeng/internal/config"
	"qingdeng/internal/parser"
	"qingdeng/internal/store"
)

// newTestDB 在临时目录建一个空库。
func newTestDB(t *testing.T) (*store.DB, *Importer) {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	schema := `CREATE TABLE books (id INTEGER PRIMARY KEY, title TEXT NOT NULL, author TEXT DEFAULT '未知',
	  intro TEXT DEFAULT '', cover_url TEXT DEFAULT '', category TEXT DEFAULT '',
	  word_count INTEGER DEFAULT 0, chapter_count INTEGER DEFAULT 0,
	  split_mode TEXT DEFAULT 'marked', source_filename TEXT, content_hash TEXT UNIQUE,
	  created_at TEXT, updated_at TEXT);
	CREATE TABLE chapters (id INTEGER PRIMARY KEY, book_id INTEGER NOT NULL REFERENCES books(id) ON DELETE CASCADE,
	  idx INTEGER NOT NULL, title TEXT NOT NULL, content TEXT NOT NULL, word_count INTEGER DEFAULT 0,
	  UNIQUE(book_id, idx));
	CREATE VIRTUAL TABLE chapters_fts USING fts5(title, content, content='chapters', content_rowid='id', tokenize='trigram');
	CREATE TRIGGER chapters_ai AFTER INSERT ON chapters BEGIN
	  INSERT INTO chapters_fts(rowid, title, content) VALUES (new.id, new.title, new.content); END;
	CREATE TRIGGER chapters_ad AFTER DELETE ON chapters BEGIN
	  INSERT INTO chapters_fts(chapters_fts, rowid, title, content) VALUES ('delete', old.id, old.title, old.content); END;
	CREATE TRIGGER chapters_au AFTER UPDATE ON chapters BEGIN
	  INSERT INTO chapters_fts(chapters_fts, rowid, title, content) VALUES ('delete', old.id, old.title, old.content);
	  INSERT INTO chapters_fts(rowid, title, content) VALUES (new.id, new.title, new.content); END;
	CREATE TABLE app_settings (key TEXT PRIMARY KEY, value TEXT);
	CREATE TABLE tg_channels (id INTEGER PRIMARY KEY, title TEXT DEFAULT '', username TEXT NOT NULL,
	  chat_id INTEGER, enabled INTEGER DEFAULT 1, last_message_id INTEGER DEFAULT 0,
	  last_sync_at TEXT, created_at TEXT);
	CREATE TABLE import_jobs (id TEXT PRIMARY KEY, status TEXT DEFAULT 'pending', total INTEGER DEFAULT 0,
	  done INTEGER DEFAULT 0, ok INTEGER DEFAULT 0, failed INTEGER DEFAULT 0, log TEXT DEFAULT '[]', created_at TEXT);`
	if err := db.Init(schema); err != nil {
		t.Fatal(err)
	}
	return db, New(db, &config.Config{})
}

func sampleMarked() []byte {
	var b strings.Builder
	b.WriteString("#  测试书\n作者：某某\n\n")
	for i := 1; i <= 5; i++ {
		b.WriteString("第" + itoa(i) + "章 标题" + itoa(i) + "\n\n")
		b.WriteString(strings.Repeat("正文内容写在这里。", 60) + "\n\n")
	}
	return []byte(b.String())
}

func itoa(n int) string { return string(rune('0' + n)) }

func TestImportOneAndDedup(t *testing.T) {
	db, imp := newTestDB(t)
	ctx := context.Background()
	opts := parser.ParseOptions{TargetChars: 3000, AddSubtitle: true, StripAds: true, MergeHardWraps: true}.Normalize()

	rec := imp.ImportOne(ctx, "测试书.txt", sampleMarked(), opts)
	if rec.Status != "ok" {
		t.Fatalf("首次导入应成功，得到 %s: %s", rec.Status, rec.Message)
	}
	if rec.Mode != "marked" || rec.Chapters != 5 {
		t.Fatalf("分章结果不对：mode=%s chapters=%d", rec.Mode, rec.Chapters)
	}

	// 换个换行风格再导一次：内容哈希去重必须命中
	crlf := bytes.ReplaceAll(sampleMarked(), []byte("\n"), []byte("\r\n"))
	again := imp.ImportOne(ctx, "测试书2.txt", crlf, opts)
	if again.Status != "skip" {
		t.Fatalf("换行风格不同应判重跳过，得到 %s: %s", again.Status, again.Message)
	}

	stats, err := db.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Books != 1 {
		t.Fatalf("库里应只有 1 本，得到 %d", stats.Books)
	}
}

func TestImportRejectsNonTxt(t *testing.T) {
	_, imp := newTestDB(t)
	rec := imp.ImportOne(context.Background(), "封面.jpg", []byte("x"), parser.ParseOptions{})
	if rec.Status != "error" {
		t.Fatalf("非 txt 应失败，得到 %s", rec.Status)
	}
}

func TestImportEmptyFile(t *testing.T) {
	_, imp := newTestDB(t)
	rec := imp.ImportOne(context.Background(), "空.txt", []byte("   \n\n "), parser.ParseOptions{})
	if rec.Status != "error" {
		t.Fatalf("空文件应失败，得到 %s", rec.Status)
	}
}

// ZIP 展开：只取 .txt，拦路径穿越，中文名还原。
func TestExpandZip(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	add := func(name, body string) {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(body))
	}
	add("a/书一.txt", "内容")
	add("b/封面.png", "PNG")
	add("c/说明.text", "内容2")
	zw.Close()

	items, errs := Expand([]Upload{{Name: "包.zip", Data: buf.Bytes()}})
	if len(errs) != 0 {
		t.Fatalf("不该有错误：%v", errs)
	}
	if len(items) != 2 {
		t.Fatalf(".txt 与 .text 都应取到，得到 %d 个", len(items))
	}
}

func TestExpandZipRejectsTraversal(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("../../evil.txt")
	w.Write([]byte("x"))
	zw.Close()

	_, errs := Expand([]Upload{{Name: "evil.zip", Data: buf.Bytes()}})
	if len(errs) == 0 {
		t.Fatal("路径穿越应被拒绝")
	}
}

func TestExpandRejectsUnknownType(t *testing.T) {
	_, errs := Expand([]Upload{{Name: "a.pdf", Data: []byte("x")}})
	if len(errs) != 1 {
		t.Fatalf("不支持的扩展名应记错误，得到 %d 条", len(errs))
	}
}

// 任务进度：逐本写库后状态应为 done 且计数正确。
func TestRunImportJob(t *testing.T) {
	db, imp := newTestDB(t)
	ctx := context.Background()
	jobID, err := db.CreateJob(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	imp.RunImport(ctx, jobID, []Upload{{Name: "a.txt", Data: sampleMarked()}}, "", 0)

	job, err := db.GetJob(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if job.Status != store.JobDone || job.OK != 1 || job.Total != 1 {
		t.Fatalf("任务结果不对：%+v", job)
	}
	if len(job.Log) != 1 || job.Log[0].BookID == 0 {
		t.Fatalf("日志应带 book_id：%+v", job.Log)
	}
}

// 重新分章：换掉章节但保留人工改过的书名。
func TestResplitKeepsEditedMeta(t *testing.T) {
	db, imp := newTestDB(t)
	ctx := context.Background()
	opts := parser.ParseOptions{TargetChars: 3000, AddSubtitle: true, StripAds: true, MergeHardWraps: true}.Normalize()

	rec := imp.ImportOne(ctx, "原书.txt", sampleMarked(), opts)
	if rec.Status != "ok" {
		t.Fatal(rec.Message)
	}
	if err := db.UpdateBook(ctx, rec.BookID, map[string]string{"title": "人工改名"}); err != nil {
		t.Fatal(err)
	}

	result := imp.ResplitOne(ctx, rec.BookID, opts)
	if result.Status != "ok" {
		t.Fatalf("重新分章失败：%s", result.Message)
	}
	book, err := db.GetBook(ctx, rec.BookID)
	if err != nil {
		t.Fatal(err)
	}
	if book.Title != "人工改名" {
		t.Fatalf("人工改过的书名不该被覆盖，得到 %q", book.Title)
	}
	if book.ChapterCount == 0 {
		t.Fatal("重新分章后不该没有章节")
	}
}
