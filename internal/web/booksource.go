package web

import (
	"encoding/json"
	"os"
	"path/filepath"

	"qingdeng/internal/config"
)

// SourceRules Legado 书源的搜索规则模板。
var SourceRules = map[string]string{
	"bookList":  "$.data.list[*]",
	"name":      "$.name",
	"author":    "$.author",
	"intro":     "$.intro",
	"kind":      "$.kind",
	"wordCount": "$.wordCount",
	"coverUrl":  "$.coverUrl",
	"bookUrl":   "$.bookUrl",
}

// BuildBookSource 按当前 SITE_BASE_URL 与 API_TOKEN 实时生成书源。
func BuildBookSource(cfg *config.Config, baseURL, token string) map[string]any {
	base := baseURL
	if base == "" {
		base = cfg.SiteBaseURL
	}
	base = trimRightSlash(base)
	if token == "" {
		token = cfg.APIToken
	}
	suffix := ""
	if token != "" {
		suffix = "&token=" + token
	}

	return map[string]any{
		"bookSourceName":  cfg.SiteName,
		"bookSourceType":  0,
		"bookSourceUrl":   base,
		"bookSourceGroup": "自建书库",
		"bookSourceComment": "自建小说站的只读 JSON 接口。\n" +
			"换地址时把本源里所有 " + base + " 替换成实际可访问的地址；\n" +
			"服务端设置了 API_TOKEN 的话，四个 url 都要带同一个 token 参数。",
		"enabled":          true,
		"enabledExplore":   true,
		"enabledCookieJar": false,
		"customOrder":      0,
		"weight":           0,
		"respondTime":      180000,
		"lastUpdateTime":   0,
		"searchUrl":        base + "/api/search?key={{key}}&page={{page}}" + suffix,
		"exploreUrl":       "最近更新::" + base + "/api/search?key=&page={{page}}" + suffix,
		"ruleSearch":       SourceRules,
		"ruleExplore":      SourceRules,
		"ruleBookInfo": map[string]string{
			"init":        "$.data",
			"name":        "$.name",
			"author":      "$.author",
			"intro":       "$.intro",
			"kind":        "$.kind",
			"wordCount":   "$.wordCount",
			"coverUrl":    "$.coverUrl",
			"lastChapter": "$.latestChapterTitle",
			"tocUrl":      "$.tocUrl",
		},
		"ruleToc": map[string]string{
			"chapterList": "$.data.list[*]",
			"chapterName": "$.title",
			"chapterUrl":  "$.url",
			"nextTocUrl":  "$.data.nextUrl",
		},
		"ruleContent": map[string]string{"content": "$.data.content"},
	}
}

// WriteStaticSource 把书源写成本地静态副本，供离线导入。
func WriteStaticSource(cfg *config.Config) error {
	payload := BuildBookSource(cfg, "http://127.0.0.1:8000", "")
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(cfg.LegadoDir, "源.json")
	if err := os.MkdirAll(cfg.LegadoDir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

func trimRightSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}
