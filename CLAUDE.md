# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

自建小说站：Go + `html/template` + SQLite。前台阅读、后台 txt 批量导入与分章、
Telegram 频道增量同步、一套给开源阅读（Legado）用的只读 JSON 接口。
项目重点是 `internal/parser/txt_parser.go`——实测 120 个真实 txt 里 70% 没有
`第X章` 标记，"无标记整本 txt"是主路径。

代码注释与文档都用中文，改动时保持一致。

## 常用命令

```bash
go build -o qingdeng ./cmd/server
go test ./...
go run ./cmd/genkey                 # 生成 SECRET_KEY

./qingdeng                          # 读 .env 里的 HOST/PORT
./qingdeng -check-sqlite            # 只校验 FTS5 + trigram，构建期用
```

国内拉依赖：`GOPROXY=https://goproxy.cn,direct go mod download`。

## 架构要点

分层是 `cmd → web → importer/tg → store → sqlite`，`parser` 是纯函数包，
不依赖数据库，可单独测试。`web` 不写 SQL，`store` 不碰 `http.Request`。

### `split_mode` 是贯穿全站的核心概念

`marked | single | auto` 这一个字段同时决定四件事，改任何一处都要想到另外三处：

- 解析器走哪条分章路径（`parser.ParseText`）
- 前台是否隐藏目录与翻章按钮（`book.html` / `reader.html` 里的 `.Single`）
- 重新分章时如何把正文拼回可解析的文本（`store.ChapterSourceText`）
- 后台列表的筛选与批量重跑范围（`store.BookIDs`）

### 解析器的对外契约

`ParseOptions` / `Chapter` / `ParsedBook` 定义在 `parser` 包里，该包**不 import
store**。`store.Settings.ParseOptions` 才是从 `app_settings` 造解析选项的工厂：
`target_chars`、两个清理开关、`add_subtitle` 是持久化的，`force_mode`
（`""` / `single` / `auto`）是每次上传或重新分章临时传的。

分章判定的几条经验规则都是实测调出来的，动阈值前先看 README 的"分章器"一节：
标记过密退回、首个标记前正文超过全文一半就不采信标记、超长单段在句末标点二次切分、
提前断章避免攒出双倍长的章、末节过短并入上一节。

### 正文的存储格式是个不变量

`chapters.content` 里段落之间是**单个 `\n`**，不存任何缩进空格（首行缩进交给 CSS
`text-indent`）。`store.ChapterSourceText` 把库里正文拼回去重新解析时，必须把 `\n`
展开成 `\n\n`，否则二次解析的硬换行合并会把段落边界吃掉；并且只有 `marked` 的书才把
章名写回文本，`auto` / `single` 的章名是生成的，写回去会污染标记识别。

### 全文检索

`chapters_fts` 是 FTS5 external content 表（`content='chapters'`），正文只存一份，
靠 `schema.sql` 里的三个 trigger 同步——**不要直接写 `chapters_fts`**，改 `chapters` 就够了。
`tokenize='trigram'` 让中文子串可搜，代价是关键词短于 3 字时用不上，
`store.SearchChapters` 会退回 `LIKE`。

高亮走的是哨兵字符：`snippet()` 用 `char(2)`/`char(3)` 包裹命中，再 `html.EscapeString`，
替换成 `<mark>` 后赋给 `SearchHit.FragHTML`（类型是 `template.HTML`）。这是模板里
`{{ .FragHTML }}` 唯一安全的前提，别改成把标签塞进 SQL 或直接标 `template.HTML` 原始数据。

### 模板：每页一个模板集

`internal/web/render.go` 给每个页面单独建一个 `template.Template`，因为 Go 的
`{{define}}` 是全局命名空间——把所有页面塞进同一个集时，后解析的 `{{define "content"}}`
会覆盖前面的。两个坑：

- 布局文件必须显式命名 `base.html` / `admin/base.html`；`ParseFiles` 用 basename
  当模板名，两个布局的 basename 都叫 `base.html`，会互相覆盖。
- 登录页是完整文档（没有 `define` 块），执行名就是它自己的路径。

新增页面时放对目录（`templates/` 或 `templates/admin/`），`layoutFor` 会自动选布局。

### SQLite 连接

`store.Open` 用 `modernc.org/sqlite`（纯 Go），DSN 里带 `_pragma` 一次设好 WAL /
外键 / busy_timeout。连接池上限 8，写是串行的。`DB.Tx` 包事务，panic 也回滚。

后台任务（导入、同步）在 goroutine 里跑，用 `context.Background()`；每本处理完调
`db.SaveJob` 重算计数并整体写回 log，前端轮询 `/admin/api/jobs/{id}`。

### 后台鉴权有两种守卫，别用错

- `adminRequired`：页面路由，未登录 302 到 `/admin/login?next=...`。
- `adminAPIRequired`：`fetch` 调用的路由，未登录直接 401 JSON。

错误页：`wantsJSON` 按路径里是否含 `/api/` 判断，返回 `{"isSuccess": false, ...}` 或渲染
`error.html`，所以 `/admin/api/*` 也会走 JSON。

### TG 频道：一个长驻 goroutine

`internal/tg/manager.go` 用 `gotd/td`。`telegram.Client.Run(ctx, fn)` 阻塞驱动一个 client，
这里把它跑在 goroutine 里，`ready` channel 通知授权完成。登录三步（发码/验码/云密码）共用
同一个 client，`phone_code_hash` 存在 `pending` 里，有效期 5 分钟。

会话存在 `data/tg.session`（`FileSessionStorage`）。同步游标是
`tg_channels.last_message_id`，只前进不后退。`syncMu` 保证全站同时只有一个同步在跑。

改这块时注意：`client.Run` 退出后必须清掉 `m.client` / `m.api`，否则下次调用会拿到死
client；所有登录态字段都在 `m.mu` 下读写。

### 部署形态

`Dockerfile` 多阶段构建，`CGO_ENABLED=0` 也能用 FTS5（modernc 是纯 Go 实现）。
容器内数据目录恒为 `/data`，compose 用 `environment:` 把 `NOVEL_DATA_DIR` /
`NOVEL_DB_PATH` 写死；换库靠 compose 专属的 `DB_FILE`。

## 注意

- `/api/*` 默认无鉴权（书源抓取需要如此），设了 `API_TOKEN` 才要 `?token=` 或
  `Authorization` 头。上传与后台一律要求管理员会话。全站没有限流。
- 上传的原始文件不落盘，正文只存库里。段落格式化与广告行清理是不可逆的，
  "重新分章"用的是已清理过的正文。
- `data/` 与 `.env` 已 gitignore。改数据库结构就改 `schema.sql`，
  里面全是 `CREATE ... IF NOT EXISTS`，启动时 `db.Init()` 会执行一遍。
