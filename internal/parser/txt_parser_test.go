package parser

import (
	"strings"
	"testing"
)

// defaultOpts 与后台默认设置一致。
func defaultOpts() ParseOptions {
	return ParseOptions{
		TargetChars: 3000, AddSubtitle: true, StripAds: true, MergeHardWraps: true,
	}.Normalize()
}

// 三态分章的基准样本，期望值与 Python 原版实测输出逐项对齐
// （marked 8 章 5131 字 / auto 2 章 5040 字 / single 1 章 40 字）。
func TestSplitModes(t *testing.T) {
	marked := "#  剑来\n作者：烽火戏诸侯\n\n" +
		strings.Repeat("第1章 风雨1\n\n"+strings.Repeat("天地之间一片苍茫，少年负剑而行。", 40)+"\n\n", 1)
	for i := 2; i <= 8; i++ {
		marked += "第" + itoa(i) + "章 风雨" + itoa(i) + "\n\n" +
			strings.Repeat("天地之间一片苍茫，少年负剑而行。", 40) + "\n\n"
	}

	cases := []struct {
		name  string
		text  string
		mode  string
		title string
	}{
		{"带标记", marked, "marked", "剑来"},
		{"无标记长篇", strings.Repeat("清早，路至诚打了个哈欠，推开窗看见院子里的海棠开得正盛。", 30*6), "auto", "无标记长篇"},
		{"短篇", "一壶酒，一碟花生米。\n他坐在窗边看了很久的雨。\n都说江湖远，其实江湖就在这条巷子里。", "single", "短篇"},
	}
	for _, tc := range cases {
		book := ParseText(tc.text, tc.name+".txt", defaultOpts(), "utf-8")
		if book.SplitMode != tc.mode {
			t.Errorf("%s: split_mode = %s, want %s", tc.name, book.SplitMode, tc.mode)
		}
		if book.Title != tc.title {
			t.Errorf("%s: title = %q, want %q", tc.name, book.Title, tc.title)
		}
		if len(book.Chapters) == 0 {
			t.Errorf("%s: 没有章节", tc.name)
		}
	}
}

// 短篇必须单章，长文必须按字数切，两者都不该出现目录。
func TestShortBookIsSingle(t *testing.T) {
	book := ParseText("第一段。\n第二段。", "短.txt", defaultOpts(), "utf-8")
	if book.SplitMode != "single" || len(book.Chapters) != 1 {
		t.Fatalf("短篇应单章，得到 %s/%d", book.SplitMode, len(book.Chapters))
	}
	if book.Chapters[0].Title != "短" {
		t.Fatalf("单章书名应取文件名，得到 %q", book.Chapters[0].Title)
	}
}

// 只有开头孤立一个标记时，那个标记是书名不是章节。
func TestLonelyHeadMarkBecomesTitle(t *testing.T) {
	text := "第一章 开始\n\n" + strings.Repeat("正文内容。", 400)
	book := ParseText(text, "x.txt", defaultOpts(), "utf-8")
	if book.SplitMode == "marked" {
		t.Fatalf("开头孤零零一个标记不该判为 marked")
	}
	// 标题被摘出来，正文里不该再有那行标记
	if strings.Contains(book.Chapters[0].Content, "第一章 开始") {
		t.Fatalf("书名式标记应被摘出正文")
	}
}

// 标记过密（目录页）应退回按字数分节。
func TestDenseMarksFallBackToAuto(t *testing.T) {
	var b strings.Builder
	for i := 1; i <= 20; i++ {
		b.WriteString("第" + itoa(i) + "章 标题\n\n短。\n\n")
	}
	book := ParseText(b.String(), "dense.txt", defaultOpts(), "utf-8")
	if book.SplitMode == "marked" {
		t.Fatalf("过密标记应被判为不可信，得到 %s", book.SplitMode)
	}
}

// 首个标记之前超过全文一半正文时，标记不采信。
func TestLateMarksIgnored(t *testing.T) {
	text := strings.Repeat("前半段正文。", 2000) + "\n第1章 迟到的标记\n第十节\n"
	book := ParseText(text, "late.txt", defaultOpts(), "utf-8")
	if book.SplitMode == "marked" {
		t.Fatalf("迟到标记不该被采信，得到 %s", book.SplitMode)
	}
}

// 超长单段要在句末标点二次切分，而不是切出一个几千字的章。
func TestLongParagraphSplit(t *testing.T) {
	para := strings.Repeat("这一句比较长，用来凑字数。", 800) // 约 10400 字，无句末标点
	book := ParseText(para, "long.txt", defaultOpts(), "utf-8")
	if len(book.Chapters) < 2 {
		t.Fatalf("超长单段应被切成多节，得到 %d 章", len(book.Chapters))
	}
	longest := 0
	for _, c := range book.Chapters {
		if c.WordCount > longest {
			longest = c.WordCount
		}
	}
	if longest > 6000 {
		t.Fatalf("单节字数 %d 超出合理范围，二次切分没生效", longest)
	}
}

// 广告行与分隔线要清掉，正文里的网址式比喻不能误伤。
func TestStripAdsAndSeparators(t *testing.T) {
	// 广告行黑名单只在整行短于 60 字时生效，所以这个"正文里出现网址"的
	// 反例必须足够长，否则按设计就会被当成广告删掉。
	longLine := "正文第二段，网址是 https:// 开头的比喻句，" +
		strings.Repeat("这句话足够长所以不该被当成广告行。", 5)
	text := "正文第一段。\n=====\n请收藏本站：https://example.com\n" + longLine + "\n----\n"
	book := ParseText(text, "ad.txt", defaultOpts(), "utf-8")
	content := book.Chapters[0].Content
	if strings.Contains(content, "=====") || strings.Contains(content, "请收藏本站") {
		t.Fatalf("广告行与分隔线应被清掉，得到 %q", content)
	}
	if !strings.Contains(content, "比喻句") {
		t.Fatalf("长正文行不该被当成广告删掉")
	}
}

// 书名/作者抽取：标题块优先，其次是文件名里的《书名》与「书名-作者」。
func TestExtractMeta(t *testing.T) {
	cases := []struct {
		file, headerBlock, headerTitle string
		title, author                  string
	}{
		{"《斗破苍穹》作者：天蚕土豆.txt", "", "", "斗破苍穹", "天蚕土豆"},
		{"凡人修仙传-忘语.txt", "", "", "凡人修仙传", "忘语"},
		{"无信息.txt", "#  剑来\n作者：烽火戏诸侯", "剑来", "剑来", "烽火戏诸侯"},
		{"纯文件名.txt", "", "", "纯文件名", "未知"},
	}
	for _, tc := range cases {
		title, author := ExtractMeta(tc.file, tc.headerBlock, tc.headerTitle)
		if title != tc.title || author != tc.author {
			t.Errorf("%s: got (%q,%q), want (%q,%q)", tc.file, title, author, tc.title, tc.author)
		}
	}
}

// URL 编码的旧文件名要解码。
func TestCleanFilename(t *testing.T) {
	got := CleanFilename("%E5%89%91%E6%9D%A5.txt")
	if got != "剑来.txt" {
		t.Fatalf("URL 解码失败：%q", got)
	}
	if CleanFilename(`C:\books\剑来.txt`) != "剑来.txt" {
		t.Fatalf("Windows 路径未剥掉目录")
	}
}

// 分节章名带副标题时取首段第一个短句。
func TestAutoSubtitle(t *testing.T) {
	text := strings.Repeat("清早，路至诚打了个哈欠。后面还有别的句子。", 400)
	book := ParseText(text, "sub.txt", defaultOpts(), "utf-8")
	if len(book.Chapters) == 0 {
		t.Fatal("没有章节")
	}
	if !strings.Contains(book.Chapters[0].Title, "·") {
		t.Fatalf("副标题没生成，章名是 %q", book.Chapters[0].Title)
	}
}
