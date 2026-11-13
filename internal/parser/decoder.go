package parser

import (
	"bytes"
	"unicode/utf8"

	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/encoding/traditionalchinese"
	"golang.org/x/text/encoding/unicode"
	"golang.org/x/text/transform"
)

// BOM 检测表。顺序有讲究：UTF-32 的前缀与 UTF-16 重叠，长的必须在前。
var boms = []struct {
	bom []byte
	enc string
}{
	{[]byte{0xEF, 0xBB, 0xBF}, "utf-8-sig"},
	{[]byte{0xFF, 0xFE, 0x00, 0x00}, "utf-32-le"},
	{[]byte{0x00, 0x00, 0xFE, 0xFF}, "utf-32-be"},
	{[]byte{0xFF, 0xFE}, "utf-16-le"},
	{[]byte{0xFE, 0xFF}, "utf-16-be"},
}

// DecodeBytes 返回 (文本, 实际使用的编码名)。任何输入都能解出文本，不抛异常。
//
// 顺序：BOM → 严格 UTF-8 → GB18030 → Big5 → 最后 UTF-8 替换。
// Python 版依赖 charset-normalizer 做统计猜测，Go 这边用"严格 UTF-8 试解 +
// 中文常见编码依次兜底"的确定性顺序——对中文 txt 的实际命中率一样，
// 而且不引入一个统计模型的体积与不确定性。
func DecodeBytes(data []byte) (string, string) {
	if len(data) == 0 {
		return "", "utf-8"
	}

	for _, item := range boms {
		if bytes.HasPrefix(data, item.bom) {
			if text, ok := tryDecode(data, item.enc); ok {
				return text, item.enc
			}
		}
	}

	// 严格 utf-8 能过就直接用：中文 txt 里 utf-8 与 gb18030 互相误判的概率很低
	if utf8.Valid(data) {
		return string(data), "utf-8"
	}

	if text, ok := tryDecode(data, "gb18030"); ok {
		return text, "gb18030"
	}
	if text, ok := tryDecode(data, "big5"); ok {
		return text, "big5"
	}
	return string(bytes.ToValidUTF8(data, []byte("�"))), "utf-8/replace"
}

func tryDecode(data []byte, encoding string) (string, bool) {
	switch encoding {
	case "utf-8", "utf-8-sig":
		return string(bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})), true
	case "utf-16-le":
		dec := unicode.UTF16(unicode.LittleEndian, unicode.UseBOM)
		out, _, err := transform.Bytes(dec.NewDecoder(), data)
		if err != nil {
			return "", false
		}
		return string(out), true
	case "utf-16-be":
		dec := unicode.UTF16(unicode.BigEndian, unicode.UseBOM)
		out, _, err := transform.Bytes(dec.NewDecoder(), data)
		if err != nil {
			return "", false
		}
		return string(out), true
	case "utf-32-le", "utf-32-be":
		// UTF-32 在中文小说里几乎不存在，退化成 UTF-16 试解，失败就交给后面的兜底
		order := unicode.LittleEndian
		if encoding == "utf-32-be" {
			order = unicode.BigEndian
		}
		dec := unicode.UTF16(order, unicode.IgnoreBOM)
		out, _, err := transform.Bytes(dec.NewDecoder(), data)
		if err != nil {
			return "", false
		}
		return string(out), true
	case "gb18030":
		out, _, err := transform.Bytes(simplifiedchinese.GB18030.NewDecoder(), data)
		if err != nil {
			return "", false
		}
		return string(out), true
	case "big5":
		out, _, err := transform.Bytes(traditionalchinese.Big5.NewDecoder(), data)
		if err != nil {
			return "", false
		}
		return string(out), true
	}
	return "", false
}
