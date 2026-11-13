// Package importer 负责上传导入：多文件与 ZIP、内容去重、后台任务进度。
//
// epub 预留：ImportOne 按扩展名分派，目前只有 txt 分支。
package importer

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"unicode"

	"qingdeng/internal/config"
	"qingdeng/internal/parser"
	"qingdeng/internal/store"
)

// TxtSuffixes 认作文本的扩展名。
var TxtSuffixes = []string{".txt", ".text"}

// ZipSuffixes 认作压缩包的扩展名。
var ZipSuffixes = []string{".zip"}

// Importer 把解析与落库串起来。
type Importer struct {
	DB  *store.DB
	Cfg *config.Config
}

// New 构造。
func New(db *store.DB, cfg *config.Config) *Importer {
	return &Importer{DB: db, Cfg: cfg}
}

// ContentHash 按"去掉全部空白的正文"取哈希：同一本书换编码或换行风格不会重复入库。
func ContentHash(text string) string {
	var b strings.Builder
	for _, r := range parser.NormalizeText(text) {
		if unicode.IsSpace(r) {
			continue
		}
		b.WriteRune(r)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// IsTxt 判断文件名是否以文本扩展名结尾。
func IsTxt(name string) bool {
	lower := strings.ToLower(name)
	for _, suffix := range TxtSuffixes {
		if strings.HasSuffix(lower, suffix) {
			return true
		}
	}
	return false
}

func isZip(name string) bool {
	lower := strings.ToLower(name)
	for _, suffix := range ZipSuffixes {
		if strings.HasSuffix(lower, suffix) {
			return true
		}
	}
	return false
}

// Upload 一个待处理的上传条目。
type Upload struct {
	Name string
	Data []byte
}

// Expand 把上传列表里的 ZIP 摊平成 txt 列表；坏包只记错误，不中断整个任务。
func Expand(uploads []Upload) (items []Upload, errors []store.JobRecord) {
	for _, up := range uploads {
		switch {
		case isZip(up.Name):
			inner, err := readZip(up.Data)
			if err != nil {
				errors = append(errors, store.JobRecord{
					File: up.Name, Status: "error", Message: "压缩包读取失败：" + err.Error(),
				})
				continue
			}
			if len(inner) == 0 {
				errors = append(errors, store.JobRecord{
					File: up.Name, Status: "error", Message: "压缩包内没有 txt",
				})
			}
			items = append(items, inner...)
		case IsTxt(up.Name):
			items = append(items, up)
		default:
			errors = append(errors, store.JobRecord{
				File: up.Name, Status: "error", Message: "只支持 .txt 与 .zip",
			})
		}
	}
	return items, errors
}

// readZip 只取 .txt，拦路径穿越与解压炸弹。
func readZip(data []byte) ([]Upload, error) {
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, err
	}

	var files []*zip.File
	for _, f := range reader.File {
		if !f.FileInfo().IsDir() {
			files = append(files, f)
		}
	}
	if len(files) > config.MaxZipEntries {
		return nil, fmt.Errorf("压缩包内条目过多：%d > %d", len(files), config.MaxZipEntries)
	}

	var items []Upload
	var total uint64
	for _, f := range files {
		name := strings.ReplaceAll(f.Name, `\`, "/")
		if strings.HasPrefix(name, "/") || hasDotDot(name) || isWindowsDrive(name) {
			return nil, fmt.Errorf("压缩包内存在非法路径：%s", name)
		}
		base := path.Base(name)
		if !IsTxt(base) {
			continue
		}
		if f.UncompressedSize64 > config.MaxZipEntryBytes {
			return nil, fmt.Errorf("%s 解压后体积超限（%d 字节）", base, f.UncompressedSize64)
		}
		total += f.UncompressedSize64
		if total > config.MaxZipTotalBytes {
			return nil, errors.New("压缩包解压总体积超限")
		}
		if f.NonUTF8 {
			// 没打 UTF-8 标志位的条目名多半是 GBK。Go 把原始字节当成 string 存了，
			// 这里按 GBK 再解一次，解不出来就沿用原名。
			if decoded, enc := parser.DecodeBytes([]byte(f.Name)); enc == "gb18030" {
				base = path.Base(decoded)
			}
		}
		rc, err := f.Open()
		if err != nil {
			return nil, fmt.Errorf("读取 %s 失败：%w", base, err)
		}
		content, err := io.ReadAll(io.LimitReader(rc, config.MaxZipEntryBytes+1))
		rc.Close()
		if err != nil {
			return nil, fmt.Errorf("读取 %s 失败：%w", base, err)
		}
		if len(content) > config.MaxZipEntryBytes {
			return nil, fmt.Errorf("%s 解压后体积超限", base)
		}
		items = append(items, Upload{Name: base, Data: content})
	}
	return items, nil
}

func hasDotDot(name string) bool {
	for _, part := range strings.Split(name, "/") {
		if part == ".." {
			return true
		}
	}
	return false
}

func isWindowsDrive(name string) bool {
	return len(name) >= 2 && name[1] == ':' &&
		((name[0] >= 'A' && name[0] <= 'Z') || (name[0] >= 'a' && name[0] <= 'z'))
}

// ImportOne 导入单本，返回一条日志记录，状态为 ok / skip / error。
func (im *Importer) ImportOne(ctx context.Context, filename string, data []byte, opts parser.ParseOptions) store.JobRecord {
	display := parser.CleanFilename(filename)
	record := store.JobRecord{File: display, Status: "error"}

	if len(data) > config.MaxTxtBytes {
		record.Message = fmt.Sprintf("文件超过 %d MB 上限", config.MaxTxtBytes/1024/1024)
		return record
	}
	if !IsTxt(display) {
		record.Message = "本轮只实现 txt 分支（epub 需要时再接解析器）"
		return record
	}

	text, encoding := parser.DecodeBytes(data)
	if strings.TrimSpace(text) == "" {
		record.Message = "文件没有可用正文"
		return record
	}

	digest := ContentHash(text)
	existing, err := im.DB.GetBookByHash(ctx, digest)
	if err != nil {
		record.Message = "查询去重失败：" + err.Error()
		return record
	}
	if existing != nil {
		record.Status = "skip"
		record.BookID = existing.ID
		record.Title = existing.Title
		record.Message = "内容重复，已跳过"
		return record
	}

	parsed := parser.ParseText(text, display, opts, encoding)
	if len(parsed.Chapters) == 0 {
		record.Message = "解析后没有章节"
		return record
	}

	bookID, err := im.DB.InsertBook(ctx, &parsed, display, digest)
	if err != nil {
		// content_hash 的 UNIQUE 约束兜住并发/同批次重复
		if isUniqueViolation(err) {
			record.Status = "skip"
			record.Title = parsed.Title
			record.Message = "内容重复，已跳过"
			return record
		}
		record.Message = "写库失败：" + err.Error()
		return record
	}

	record.Status = "ok"
	record.BookID = bookID
	record.Title = parsed.Title
	record.Author = parsed.Author
	record.Mode = parsed.SplitMode
	record.ModeLabel = store.SplitModeLabels[parsed.SplitMode]
	record.Chapters = len(parsed.Chapters)
	record.Words = parsed.WordCount
	record.Encoding = encoding
	record.Message = strings.Join(parsed.Notes, "；")
	return record
}

func isUniqueViolation(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unique constraint") || strings.Contains(msg, "constraint failed")
}

// RunImport 后台任务入口。每本处理完就写一次进度，前端轮询即可看到逐本结果。
func (im *Importer) RunImport(ctx context.Context, jobID string, uploads []Upload, forceMode string, targetChars int) {
	var log []store.JobRecord
	finish := func(status string, total int) {
		_ = im.DB.SaveJob(ctx, jobID, status, total, log)
	}

	settings, err := im.DB.LoadSettings(ctx)
	if err != nil {
		log = append(log, store.JobRecord{File: "-", Status: "error", Message: "读取设置失败：" + err.Error()})
		finish(store.JobError, len(log))
		return
	}
	opts := settings.ParseOptions(forceMode, targetChars)

	items, expandErrors := Expand(uploads)
	log = expandErrors
	total := len(items) + len(log)
	finish(store.JobRunning, total)

	for _, item := range items {
		record := func() (rec store.JobRecord) {
			defer func() {
				// 单本失败不拖垮整批
				if p := recover(); p != nil {
					rec = store.JobRecord{
						File: parser.CleanFilename(item.Name), Status: "error",
						Message: fmt.Sprintf("panic: %v", p),
					}
				}
			}()
			return im.ImportOne(ctx, item.Name, item.Data, opts)
		}()
		log = append(log, record)
		finish(store.JobRunning, total)
	}

	finish(store.JobDone, total)
}

// ResplitOne 用库里现存正文重跑解析器。调完规则后批量修复就靠它。
func (im *Importer) ResplitOne(ctx context.Context, bookID int64, opts parser.ParseOptions) store.JobRecord {
	book, err := im.DB.GetBook(ctx, bookID)
	if err != nil {
		return store.JobRecord{File: fmt.Sprintf("#%d", bookID), Status: "error", Message: err.Error()}
	}
	if book == nil {
		return store.JobRecord{File: fmt.Sprintf("#%d", bookID), Status: "error", Message: "书籍不存在"}
	}

	record := store.JobRecord{File: book.Title, Status: "error", BookID: bookID}
	source, err := im.DB.ChapterSourceText(ctx, bookID)
	if err != nil {
		record.Message = "读取正文失败：" + err.Error()
		return record
	}
	if strings.TrimSpace(source) == "" {
		record.Message = "这本书没有正文"
		return record
	}

	sourceName := book.SourceFilename
	if sourceName == "" {
		sourceName = book.Title
	}
	parsed := parser.ParseText(source, sourceName, opts, "")
	if len(parsed.Chapters) == 0 {
		record.Message = "重新解析后没有章节，已保留原样"
		return record
	}
	if err := im.DB.ReplaceChapters(ctx, bookID, &parsed); err != nil {
		record.Message = "替换章节失败：" + err.Error()
		return record
	}

	record.Status = "ok"
	record.Title = book.Title
	record.Mode = parsed.SplitMode
	record.ModeLabel = store.SplitModeLabels[parsed.SplitMode]
	record.Chapters = len(parsed.Chapters)
	record.Words = parsed.WordCount
	record.Message = strings.Join(parsed.Notes, "；")
	return record
}

// RunResplit 批量重新分章的后台任务入口。
func (im *Importer) RunResplit(ctx context.Context, jobID string, bookIDs []int64, forceMode string, targetChars int) {
	var log []store.JobRecord
	settings, err := im.DB.LoadSettings(ctx)
	if err != nil {
		log = append(log, store.JobRecord{File: "-", Status: "error", Message: "读取设置失败：" + err.Error()})
		_ = im.DB.SaveJob(ctx, jobID, store.JobError, len(log), log)
		return
	}
	opts := settings.ParseOptions(forceMode, targetChars)
	total := len(bookIDs)
	_ = im.DB.SaveJob(ctx, jobID, store.JobRunning, total, log)

	for _, id := range bookIDs {
		record := func() (rec store.JobRecord) {
			defer func() {
				if p := recover(); p != nil {
					rec = store.JobRecord{
						File: fmt.Sprintf("#%d", id), Status: "error",
						Message: fmt.Sprintf("panic: %v", p),
					}
				}
			}()
			return im.ResplitOne(ctx, id, opts)
		}()
		log = append(log, record)
		_ = im.DB.SaveJob(ctx, jobID, store.JobRunning, total, log)
	}
	_ = im.DB.SaveJob(ctx, jobID, store.JobDone, total, log)
}
