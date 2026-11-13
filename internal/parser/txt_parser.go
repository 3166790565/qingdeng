// Package parser 负责 txt 解码与分章。
//
// 三态分章（样本实测 120 个 txt 里 70% 完全没有章节标记，所以无标记是主路径）：
//
//   - marked：正文有可信的章节标记，按标记行切
//   - single：整本一章（短篇；或只有一个标记且落在开头）
//   - auto  ：无标记的长文，按目标字数在段落边界切节
//
// 本包不依赖数据库，纯函数，单独拿出来跑就能验证分章行为。
package parser

import (
	"regexp"
	"strings"
	"unicode/utf8"

	"qingdeng/internal/textutil"
)

// ParseOptions 解析选项。前四项来自后台设置页，ForceMode 来自上传表单或重新分章。
type ParseOptions struct {
	TargetChars      int    // 目标分节字数，默认 3000
	StripHeaderBlock bool   // 剥离开头的 ###### 标题块
	FixYaoVariant    bool   // 把"幺"改回"么"
	AddSubtitle      bool   // auto 分节章名附带首句副标题
	StripAds         bool   // 广告行黑名单
	MergeHardWraps   bool   // 合并被硬换行截断的句子
	ForceMode        string // "" 自动判定 | "single" 强制单章 | "auto" 强制按字数切
}

// Normalize 钳制取值范围，非法 ForceMode 归零。
func (o ParseOptions) Normalize() ParseOptions {
	if o.TargetChars == 0 {
		o.TargetChars = 3000
	}
	if o.TargetChars < 500 {
		o.TargetChars = 500
	}
	if o.TargetChars > 50000 {
		o.TargetChars = 50000
	}
	if o.ForceMode != "single" && o.ForceMode != "auto" {
		o.ForceMode = ""
	}
	return o
}

// Chapter 一章。Content 里段落之间用单个 \n 分隔，不含缩进空格。
type Chapter struct {
	Idx       int
	Title     string
	Content   string
	WordCount int
}

// ParsedBook 解析结果。
type ParsedBook struct {
	Title     string
	Author    string
	Chapters  []Chapter
	SplitMode string
	WordCount int
	Encoding  string
	Notes     []string
}

// ---------------------------------------------------------------- 规范化

var (
	reNewlines  = regexp.MustCompile(`\r+\n|\r+`)
	reZeroWidth = regexp.MustCompile(`[\x{FEFF}\x{200B}-\x{200F}\x{2060}\x{180E}]`)
	// 整行都是装饰字符的分隔线，含开头标题块的 ###### 围栏
	reSeparator = regexp.MustCompile(`^[#=*\-_~—＝－…·.\s]{3,}$`)
	reIndent    = regexp.MustCompile(`^[ \t　\x{00a0}]{2,}`)
	reManyBlank = regexp.MustCompile(`\n{3,}`)
)

// 命中且整行短于 60 字才删，避免误伤正文里出现的网址式比喻
var reAdPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)https?://`),
	regexp.MustCompile(`(?i)www\.`),
	regexp.MustCompile(`(?i)\.(com|net|org|cn|cc|xyz|top|info|me)\b`),
	regexp.MustCompile(`更新最快`),
	regexp.MustCompile(`请收藏`),
	regexp.MustCompile(`本书由.{0,20}整理`),
	regexp.MustCompile(`最新章节.{0,10}(阅读|下载|访问)`),
	regexp.MustCompile(`免费(阅读|下载|观看)`),
	regexp.MustCompile(`(书友|读者|微信|QQ)\s*群?\s*[:：]?\s*\d{5,}`),
}

// CountWords 字数统计：去掉所有空白后的字符数，中文按字计。
func CountWords(text string) int {
	return textutil.CountNonSpace(text)
}

// NormalizeText 换行归一、去零宽字符、连续空行压成一个空行。
func NormalizeText(text string) string {
	text = reNewlines.ReplaceAllString(text, "\n")
	text = reZeroWidth.ReplaceAllString(text, "")
	text = strings.ReplaceAll(text, " ", " ")
	return reManyBlank.ReplaceAllString(text, "\n\n")
}

type line struct {
	text     string // 已 strip；空串代表段落分隔的空行
	indented bool   // 原行有缩进，是"新段落开始"的强信号
}

func isAd(text string) bool {
	if utf8.RuneCountInString(text) >= 60 {
		return false
	}
	for _, re := range reAdPatterns {
		if re.MatchString(text) {
			return true
		}
	}
	return false
}

// toLines 切行 → 丢弃分隔线与广告行 → 连续空行压成一个。
func toLines(text string, opts ParseOptions) []line {
	var out []line
	for _, raw := range strings.Split(text, "\n") {
		indented := reIndent.MatchString(raw)
		t := strings.TrimSpace(raw)
		if t == "" {
			if len(out) > 0 && out[len(out)-1].text != "" {
				out = append(out, line{})
			}
			continue
		}
		if reSeparator.MatchString(t) {
			continue
		}
		if opts.StripAds && isAd(t) {
			continue
		}
		out = append(out, line{text: t, indented: indented})
	}
	for len(out) > 0 && out[len(out)-1].text == "" {
		out = out[:len(out)-1]
	}
	return out
}

// ExtractHeaderBlock 剥出开头的 ###### 标题块。
//
// 返回 (块原文, 去掉块之后的正文, 块内书名)；没有块时块与书名为空串。
// 重新分章时正文里的 ###### 围栏早已被清掉、只剩 "#  书名" 一行，
// 所以这里只要求首个非空行以 # 开头，两种形态都能认出来。
func ExtractHeaderBlock(text string) (block, rest, headerTitle string) {
	lines := strings.Split(text, "\n")
	start := 0
	for start < len(lines) && strings.TrimSpace(lines[start]) == "" {
		start++
	}
	if start >= len(lines) || !strings.HasPrefix(strings.TrimSpace(lines[start]), "#") {
		return "", text, ""
	}

	var titles []string
	end := start
	cursor := start
	limit := start + 12
	if len(lines) < limit {
		limit = len(lines)
	}
	for cursor < limit {
		stripped := strings.TrimSpace(lines[cursor])
		if stripped == "" {
			cursor++
			continue
		}
		if !strings.HasPrefix(stripped, "#") {
			break
		}
		inner := strings.TrimSpace(strings.Trim(stripped, "#"))
		if inner != "" {
			titles = append(titles, inner)
		}
		end = cursor
		cursor++
	}

	block = strings.Join(lines[start:end+1], "\n")
	if end+1 <= len(lines) {
		rest = strings.Join(lines[end+1:], "\n")
	}
	if len(titles) > 0 {
		headerTitle = titles[0]
	}
	return block, rest, headerTitle
}

// ---------------------------------------------------------------- 章节标记

const numChars = "0-9０-９零一二三四五六七八九十百千万两壹贰叁肆伍陆柒捌玖拾廿"

// 强标记：单独成行即可采信
var strongMarks = []*regexp.Regexp{
	regexp.MustCompile(`^第[` + numChars + `]{1,8}[章节節回卷篇集话話幕][\s：:、.．\-—]*.{0,36}$`),
	regexp.MustCompile(`^(序章|序|楔子|引子|前言|后记|後記|尾声|终章|番外.{0,20})$`),
	regexp.MustCompile(`(?i)^chapter\s*\d{1,4}\b.{0,36}$`),
}

// 弱标记：同一样式命中 3 次以上才启用，否则编号列表会被当成章节
var weakMarks = []*regexp.Regexp{
	regexp.MustCompile(`^\d{1,4}[、.。．,，]\s*.{0,36}$`),
	regexp.MustCompile(`^[（(【\[]\s*[` + numChars + `]{1,4}\s*[）)】\]]\s*$`),
}

const (
	markMaxLen       = 40   // 整行去空白后不超过 40 字，避免误伤正文里提到的"第一章"
	weakMinHits      = 3    // 弱标记同一样式命中次数下限
	minGapChars      = 200  // 相邻标记之间正文少于此值算"过密"
	maxPreambleRatio = 0.5  // 首个标记之前的正文占比上限
	headRatio        = 0.05 // 单个标记落在正文前 5% 才算书名式标记
)

// findMarks 返回被判定为章节标题的行下标。
func findMarks(lines []line) []int {
	var strong []int
	weak := map[int][]int{}
	for i, ln := range lines {
		text := ln.text
		if text == "" || utf8.RuneCountInString(text) > markMaxLen {
			continue
		}
		matched := false
		for _, re := range strongMarks {
			if re.MatchString(text) {
				strong = append(strong, i)
				matched = true
				break
			}
		}
		if matched {
			continue
		}
		for kind, re := range weakMarks {
			if re.MatchString(text) {
				weak[kind] = append(weak[kind], i)
				break
			}
		}
	}
	// 有可信强标记时不再混入弱标记，否则正文里的编号列表会把章切碎
	if len(strong) >= 2 {
		return strong
	}
	marks := make(map[int]bool, len(strong))
	for _, i := range strong {
		marks[i] = true
	}
	for _, hits := range weak {
		if len(hits) >= weakMinHits {
			for _, i := range hits {
				marks[i] = true
			}
		}
	}
	out := make([]int, 0, len(marks))
	for i := range marks {
		out = append(out, i)
	}
	sortInts(out)
	return out
}

func bodyChars(lines []line) int {
	var b strings.Builder
	for _, ln := range lines {
		b.WriteString(ln.text)
	}
	return CountWords(b.String())
}

// marksArePlausible 间距校验：过密的标记多半是目录页或人物列表，不是真章节。
func marksArePlausible(lines []line, marks []int) bool {
	if len(marks) < 2 {
		return false
	}
	total := bodyChars(lines)
	preamble := CountWords(joinTexts(lines[:marks[0]]))
	// 实测有整本 4 万字、只在末尾冒出两个标记的文件。这种标记不是章节结构，
	// 采信它会把前面全部正文攒成一个读不下去的超长章，退回按字数分节更好。
	if total > 0 && float64(preamble) > float64(total)*maxPreambleRatio {
		return false
	}
	bounds := append(append([]int{}, marks...), len(lines))
	thin := 0
	for i := 0; i+1 < len(bounds); i++ {
		a, b := bounds[i], bounds[i+1]
		if CountWords(joinTexts(lines[a+1:b])) < minGapChars {
			thin++
		}
	}
	return thin*2 <= len(bounds)-1
}

func markAtHead(lines []line, pos int) bool {
	total := bodyChars(lines)
	if total == 0 {
		return true
	}
	before := CountWords(joinTexts(lines[:pos]))
	return before <= 200 || float64(before) <= float64(total)*headRatio
}

func joinTexts(lines []line) string {
	var b strings.Builder
	for _, ln := range lines {
		b.WriteString(ln.text)
	}
	return b.String()
}

// ---------------------------------------------------------------- 段落格式化

// 行尾出现这些字符说明句子已收，下一行是新段落
const sentEnd = "。！？；…”\"』」）)】》!?;：:～~—"

func continues(prev string, ln line) bool {
	if ln.indented {
		return false
	}
	if prev == "" {
		return false
	}
	last, _ := utf8.DecodeLastRuneInString(prev)
	if strings.ContainsRune(sentEnd, last) {
		return false
	}
	return utf8.RuneCountInString(prev) <= 4000
}

func buildParagraphs(lines []line, opts ParseOptions) []string {
	var paragraphs []string
	current := ""
	for _, ln := range lines {
		if ln.text == "" {
			if current != "" {
				paragraphs = append(paragraphs, current)
				current = ""
			}
			continue
		}
		switch {
		case current == "":
			current = ln.text
		case opts.MergeHardWraps && continues(current, ln):
			current += ln.text
		default:
			paragraphs = append(paragraphs, current)
			current = ln.text
		}
	}
	if current != "" {
		paragraphs = append(paragraphs, current)
	}
	return paragraphs
}

// ---------------------------------------------------------------- auto 分节

const (
	cutPunct        = "。！？…"
	closers         = "”\"』」）)】》…"
	longParaRatio   = 1.5 // 超过目标的这个倍数就在段内二次切分
	tailMinRatio    = 0.3 // 末节不足目标的这个比例就并入上一节
	earlyCloseRatio = 0.8 // 已攒到目标的这个比例，就允许提前断章
	overshootRatio  = 1.3 // 再加一段会超过目标的这个倍数，那就先断
)

// splitLongParagraph 超长单段（实测最长 6737 字）在句末标点处二次切分。
// 切点取窗口内最接近目标字数的句末标点；找不到标点就按字数硬切。
func splitLongParagraph(text string, target int) []string {
	runes := []rune(text)
	var pieces []string
	start := 0
	total := len(runes)
	window := int(float64(target) * longParaRatio)
	for total-start > window {
		ideal := start + target
		lo := start + target/2
		if lo <= start {
			lo = start + 1
		}
		hi := start + window
		if hi > total {
			hi = total
		}
		best := -1
		for i := lo; i < hi; i++ {
			if strings.ContainsRune(cutPunct, runes[i]) && (best < 0 || abs(i-ideal) < abs(best-ideal)) {
				best = i
			}
		}
		cut := 0
		if best < 0 {
			cut = ideal
			if cut > total {
				cut = total
			}
		} else {
			cut = best + 1
			// 别把收尾引号留到下一节
			for cut < total && strings.ContainsRune(closers, runes[cut]) {
				cut++
			}
		}
		pieces = append(pieces, string(runes[start:cut]))
		start = cut
	}
	if start < total {
		pieces = append(pieces, string(runes[start:]))
	}
	return pieces
}

var reSubSplit = regexp.MustCompile(`[。！？…!?]`)

func subtitle(paragraph string) string {
	head := reSubSplit.Split(strings.TrimSpace(paragraph), 2)[0]
	head = strings.TrimSpace(strings.Trim(head, "“”\"'‘’『』「」()（）　#*·-—_"))
	n := utf8.RuneCountInString(head)
	if n >= 2 && n <= 20 {
		return head
	}
	return ""
}

func makeChapter(idx int, title string, paragraphs []string) Chapter {
	content := strings.Join(paragraphs, "\n")
	if title == "" {
		title = "第 " + itoa(idx) + " 节"
	}
	return Chapter{Idx: idx, Title: title, Content: content, WordCount: CountWords(content)}
}

func buildAuto(paragraphs []string, opts ParseOptions, notes *[]string) []Chapter {
	target := opts.TargetChars
	var units []string
	longParas := 0
	for _, para := range paragraphs {
		if utf8.RuneCountInString(para) > int(float64(target)*longParaRatio) {
			units = append(units, splitLongParagraph(para, target)...)
			longParas++
		} else {
			units = append(units, para)
		}
	}
	if longParas > 0 {
		*notes = append(*notes, itoa(longParas)+" 个超长段落已在句末标点二次切分")
	}

	var sections [][]string
	var current []string
	currentLen := 0
	for _, unit := range units {
		unitLen := CountWords(unit)
		// 已经差不多够一节了，再加这一段会明显超标：先断，别攒出双倍长的章
		if len(current) > 0 &&
			float64(currentLen) >= float64(target)*earlyCloseRatio &&
			float64(currentLen+unitLen) > float64(target)*overshootRatio {
			sections = append(sections, current)
			current = nil
			currentLen = 0
		}
		current = append(current, unit)
		currentLen += unitLen
		if currentLen >= target { // 总在段落边界断章
			sections = append(sections, current)
			current = nil
			currentLen = 0
		}
	}
	if len(current) > 0 {
		sections = append(sections, current)
	}

	if len(sections) >= 2 {
		tail := CountWords(strings.Join(sections[len(sections)-1], ""))
		if float64(tail) < float64(target)*tailMinRatio {
			last := len(sections) - 1
			sections[last-1] = append(sections[last-1], sections[last]...)
			sections = sections[:last]
			*notes = append(*notes, "末节过短，已并入上一节")
		}
	}

	chapters := make([]Chapter, 0, len(sections))
	for i, section := range sections {
		idx := i + 1
		title := "第 " + itoa(idx) + " 节"
		if opts.AddSubtitle {
			if hint := subtitle(section[0]); hint != "" {
				title = title + " · " + hint
			}
		}
		chapters = append(chapters, makeChapter(idx, title, section))
	}
	return chapters
}

func buildMarked(lines []line, marks []int, opts ParseOptions, notes *[]string) []Chapter {
	var chapters []Chapter
	preamble := buildParagraphs(lines[:marks[0]], opts)
	preambleChars := CountWords(strings.Join(preamble, ""))

	if len(preamble) > 0 && preambleChars >= 100 {
		if preambleChars > opts.TargetChars {
			// 首个标记之前是无标记正文，按字数切开，别攒成一个读不下去的超长章
			plain := opts
			plain.AddSubtitle = false
			sections := buildAuto(preamble, plain, notes)
			for _, section := range sections {
				chapters = append(chapters,
					makeChapter(len(chapters)+1, "开篇 "+section.Title, []string{section.Content}))
			}
			*notes = append(*notes, "首个标记前有 "+itoa(preambleChars)+" 字正文，已切成 "+itoa(len(sections))+" 节")
		} else {
			chapters = append(chapters, makeChapter(1, "开篇", preamble))
		}
		preamble = nil
	}

	bounds := append(append([]int{}, marks...), len(lines))
	for i := 0; i+1 < len(bounds); i++ {
		a, b := bounds[i], bounds[i+1]
		paragraphs := buildParagraphs(lines[a+1:b], opts)
		if len(preamble) > 0 { // 太短的引言并进第一章，不单独成章
			paragraphs = append(preamble, paragraphs...)
			preamble = nil
		}
		if len(paragraphs) == 0 { // 卷标题这类空章不入库
			continue
		}
		chapters = append(chapters, makeChapter(len(chapters)+1, lines[a].text, paragraphs))
	}
	return chapters
}

// ---------------------------------------------------------------- 书名与作者

var (
	reAuthor    = regexp.MustCompile(`作\s*者\s*[:：]?\s*([^\s,，。/\\|、]{1,20})`)
	reExt       = regexp.MustCompile(`(?i)\.(txt|text)$`)
	reTitleAuth = regexp.MustCompile(`^(?P<title>[^-－—_]{1,60})\s*[-－—_]\s*(?P<author>[^-－—_\s]{1,20})$`)
	reBookTitle = regexp.MustCompile(`《([^》]{1,60})》`)
)

// CleanFilename 旧目录里的文件名是 URL 编码的中文，导入前先解码。
func CleanFilename(name string) string {
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	if strings.Contains(name, "%") {
		if decoded, err := urlUnescape(name); err == nil {
			name = decoded
		}
	}
	return name
}

// FilenameStem 去掉目录与扩展名。
func FilenameStem(name string) string {
	return strings.TrimSpace(reExt.ReplaceAllString(CleanFilename(name), ""))
}

func pickMeta(text string) (title, author string) {
	body := strings.TrimSpace(text)
	if m := reAuthor.FindStringSubmatchIndex(body); m != nil {
		author = body[m[2]:m[3]]
		body = strings.TrimSpace(body[:m[0]] + " " + body[m[1]:])
	}
	if m := reBookTitle.FindStringSubmatch(body); m != nil {
		return strings.TrimSpace(m[1]), author
	}
	if m := reTitleAuth.FindStringSubmatch(body); m != nil {
		name := m[reTitleAuth.SubexpIndex("title")]
		who := m[reTitleAuth.SubexpIndex("author")]
		if author == "" {
			author = who
		}
		return strings.TrimSpace(name), author
	}
	body = strings.Trim(body, " #*=-_·　")
	if strings.Contains(body, "\n") || utf8.RuneCountInString(body) > 60 {
		return "", author
	}
	return body, author
}

// ExtractMeta 书名抽取始终执行，与是否剥离标题块无关。
func ExtractMeta(filename, headerBlock, headerTitle string) (string, string) {
	title, author := "", ""
	for _, source := range []string{headerTitle, headerBlock, FilenameStem(filename)} {
		if source == "" {
			continue
		}
		foundTitle, foundAuthor := pickMeta(source)
		if title == "" {
			title = foundTitle
		}
		if author == "" {
			author = foundAuthor
		}
	}
	if title == "" {
		title = FilenameStem(filename)
	}
	if title == "" {
		title = "未命名"
	}
	if author == "" {
		author = "未知"
	}
	return truncateRunes(title, 80), truncateRunes(author, 40)
}

// ---------------------------------------------------------------- 入口

// ParseText 分章主入口。
func ParseText(text, filename string, options ParseOptions, encoding string) ParsedBook {
	opts := options.Normalize()
	var notes []string

	text = NormalizeText(text)
	if opts.FixYaoVariant {
		// 在切块之前替换，书名跟着一起纠正
		text = strings.ReplaceAll(text, "幺", "么")
	}
	block, rest, headerTitle := ExtractHeaderBlock(text)
	body := text
	if block != "" && opts.StripHeaderBlock {
		body = rest
	}

	lines := toLines(body, opts)
	title, author := ExtractMeta(filename, block, headerTitle)
	if opts.FixYaoVariant { // 文件名来的书名也要纠正
		title = strings.ReplaceAll(title, "幺", "么")
		author = strings.ReplaceAll(author, "幺", "么")
	}

	mode := opts.ForceMode
	var marks []int
	singleTitle := ""

	if mode == "" {
		marks = findMarks(lines)
		if marksArePlausible(lines, marks) {
			mode = "marked"
		} else {
			if len(marks) == 1 && markAtHead(lines, marks[0]) {
				// 开头孤零零一个标记是书名式标题，不是章节分界：摘出来当章名
				singleTitle = lines[marks[0]].text
				lines = append(append([]line{}, lines[:marks[0]]...), lines[marks[0]+1:]...)
			}
			marks = nil
			mode = "single"
			if bodyChars(lines) > opts.TargetChars {
				mode = "auto"
			}
		}
	}

	var chapters []Chapter
	if mode == "marked" {
		chapters = buildMarked(lines, marks, opts, &notes)
		if len(chapters) == 0 { // 标记全是空章，退回单章
			mode = "single"
		}
	}
	switch mode {
	case "single":
		paragraphs := buildParagraphs(lines, opts)
		if len(paragraphs) > 0 {
			name := singleTitle
			if name == "" {
				name = title
			}
			chapters = []Chapter{makeChapter(1, name, paragraphs)}
		}
	case "auto":
		chapters = buildAuto(buildParagraphs(lines, opts), opts, &notes)
	}

	words := 0
	for _, c := range chapters {
		words += c.WordCount
	}
	return ParsedBook{
		Title:     title,
		Author:    author,
		Chapters:  chapters,
		SplitMode: mode,
		WordCount: words,
		Encoding:  encoding,
		Notes:     notes,
	}
}

// ParseBytes 解码后分章。
func ParseBytes(data []byte, filename string, options ParseOptions) ParsedBook {
	text, encoding := DecodeBytes(data)
	return ParseText(text, filename, options, encoding)
}
