# 青灯书斋（Go 版）

自建小说站：**Go + SQLite**，服务端渲染的阅读前台、txt 批量导入后台、
Telegram 频道定时增量同步，外加一套只读 JSON 接口和配套的开源阅读（Legado）书源。

这是 [小说网2](../小说网2)（FastAPI 版）的 Go 重写。行为契约、数据库 schema、
书源 JSON、后台交互都保持不变，差别在运行时：

| | Python 版 | Go 版 |
| --- | --- | --- |
| HTTP | FastAPI + uvicorn | `net/http` + `ServeMux`（Go 1.22+ 路由模式） |
| 模板 | Jinja2 | `html/template`（每页一个模板集） |
| SQLite | 系统 sqlite3（cgo） | `modernc.org/sqlite`（纯 Go，`CGO_ENABLED=0` 可编译） |
| TG | Telethon + 自建事件循环线程 | `gotd/td`，一个长驻 goroutine |
| 部署 | Python 镜像 + pip | 单个静态二进制，约 20MB |

分章器仍然是这个项目的重点。实测 120 个真实 txt 里 70% 完全没有 `第X章` 标记，
所以"无章节标记的整本 txt"是主路径而不是边缘情况。

## 快速开始

```bash
go build -o qingdeng ./cmd/server

cp .env.example .env        # Windows: copy .env.example .env
# 打开 .env 填上 ADMIN_PASSWORD 和 SECRET_KEY
# 生成 SECRET_KEY：go run ./cmd/genkey

./qingdeng                  # http://127.0.0.1:8000
```

首次启动会在 `data/` 下建 `novel.db`（WAL 模式）。打开 `/admin` 登录后到上传页丢 txt 进去。

国内网络拉依赖：

```bash
GOPROXY=https://goproxy.cn,direct go mod download
```

## 配置

读取优先级：**系统环境变量 > 项目根目录的 `.env` > 内置默认值**。

`.env` 只支持最基本的写法：一行一个 `KEY=VALUE`，`#` 开头的整行是注释，
`export ` 前缀和成对的引号会被去掉。**不支持行尾注释**——密码里带 `#` 很常见，截断了更麻烦。
`.env` 已在 `.gitignore` 里；`.env.example` 是可以提交的模板。

| 变量 | 默认 | 说明 |
| --- | --- | --- |
| `ADMIN_PASSWORD` | 随机生成 | 后台密码。两处都没配就每次启动随机生成一个，打印在启动日志里 |
| `SECRET_KEY` | 随机生成 | 会话 cookie 签名密钥。不固定则重启即登出，日志会提醒 |
| `API_TOKEN` | 空 | 设置后 `/api/*` 需要 `?token=` 或 `Authorization` 头；留空则公开 |
| `SITE_BASE_URL` | `http://127.0.0.1:8000` | 写进书源与 API 返回 URL 的对外地址 |
| `SITE_NAME` / `SITE_TAGLINE` | 青灯书斋 / 一盏灯，一本书 | 站名与副标题 |
| `TG_API_ID` / `TG_API_HASH` | 空 | Telegram 频道的应用凭证 |
| `NOVEL_DATA_DIR` / `NOVEL_DB_PATH` | `data/` | 数据目录与库文件位置 |
| `NOVEL_ENV_FILE` | `.env` | 换一个配置文件路径。只能由系统环境变量指定 |
| `HOST` / `PORT` | 127.0.0.1 / 8000 | 监听地址 |

## 目录结构

```
cmd/
  server/          主程序：加载配置、初始化库、装配路由、优雅退出
  bulkimport/      批量导入命令（本地直写 / HTTP 推送两种模式）
  genkey/          生成 SECRET_KEY
internal/
  config/          .env 解析与配置项
  store/           SQLite 连接、schema、书籍/章节/任务/频道的数据访问
  parser/          解码 + 分章（核心，纯函数，不碰数据库）
  importer/        txt/ZIP 导入、内容去重、重新分章、任务进度
  tg/              TG 登录状态机、频道解析、增量同步、自动调度
  web/             路由、模板渲染、管理员会话、书源接口
  textutil/        跨包共用的文本小工具
schema.sql         books / chapters / chapters_fts / import_jobs / app_settings / tg_channels
templates/  static/  legado/  data/（已 gitignore）
```

## 批量导入

```bash
go build -o bulkimport ./cmd/bulkimport

# 本机直接写库（最快）
./bulkimport -dir "E:/PythonProject/小说爬/小说_90000-"
./bulkimport -dir <目录> -r -workers 8 -limit 200
./bulkimport -dir <目录> -dry-run
```

| 参数 | 作用 |
| --- | --- |
| `-r` | 连子目录一起扫 |
| `-workers N` | 解析并发数，默认 `CPU数-1`；`0` 串行调试 |
| `-limit N` | 只处理前 N 个文件 |
| `-dry-run` | 只解析不写库 |
| `-force-mode single/auto` | 强制整本单章 / 按字数分节 |
| `-target-chars N` | 覆盖后台的「分节字数」 |
| `-no-resume` / `-retry-errors` | 忽略续跑日志 / 重试上次失败的文件 |

- **断点续跑**：中断了直接重跑同一条命令，已处理过的文件按 `data/bulk_import.jsonl` 自动跳过
- **去重**：按"去掉全部空白的正文"的 sha256 判重
- **追更**：目录里新加了 txt，重跑一次即可

### 推到另一台机器/容器里的站点

```bash
./bulkimport -dir <目录> -http http://你的服务器:8000 -admin-password 你的密码
```

会话 cookie 缓存在 `<数据目录>/uploader_cookies.json`，过期自动重新登录，
之后重跑可以不加 `-admin-password`。

## 分章器

解码 → 规范化 → 识别开头 `######` 标题块 → 扫描章节标记 → 三态分章 → 段落格式化。

### 三种模式

| 模式 | 触发条件 | 结果 |
| --- | --- | --- |
| `marked` | 命中 ≥ 2 个标记，且通过合理性校验 | 按标题行切分 |
| `single` | 只有 1 个标记且落在开头，或正文短于分节字数 | 整本一章 |
| `auto` | 无可信标记且正文较长 | 按目标字数在段落边界切节 |

标记要求整行、去空白后不超过 40 字：`第X章/节/回/卷/篇/集/话/幕`、
`序章/楔子/引子/前言/后记/尾声/终章/番外`、`Chapter N`；
`1.` 这类编号列表与 `（一）` 属弱标记，同一样式命中 3 次以上才启用。

标记合理性校验有两条：

- 相邻标记之间正文不足 200 字、且这种情况占比过半 → 判定误命中（目录页、人物列表）
- 首个标记之前的正文超过全文一半 → 标记不是章节结构，退回按字数分节

### auto 模式的分节

贪心累加段落，达到目标字数（默认 3000，后台可调）就断章，每节都在段落边界结束：

- **超长单段**：单段超过目标 1.5 倍时在段末标点 `。！？…` 处二次切分
- **避免双倍章**：已攒到目标 80% 且再加一段会超过目标 1.3 倍时提前断章
- **尾章过短**：末节不足目标 30% 时并入上一节

### 解析设置（后台可改，存数据库）

- **分节字数**：默认 3000
- **剥离开头 `######` 标题块**：默认关闭（建议打开，然后批量重新分章一次）
- **把「幺」改回「么」**：默认关闭
- **分节副标题**：默认开启

## 后台

`/admin`，单一管理员密码，会话是 HMAC-SHA256 签名的 HttpOnly + SameSite=Lax cookie。

- **上传**：拖拽多选 txt，也支持 ZIP（校验路径穿越、解压体积、条目数）
- **TG 频道**：登录 Telegram 后把公开频道里的 txt 逐本爬进书库
- **书籍管理**：分页列表、筛选、编辑书名/作者/简介/封面/分类、删除
- **重新分章**：单本 / 勾选若干本 / 按当前筛选全量重跑
- **章节**：列表分页，单章在线编辑正文
- **解析设置**：上面那几个开关

## Telegram 频道导入

1. 在 [my.telegram.org](https://my.telegram.org) → API development tools 创建应用，
   把 `TG_API_ID` / `TG_API_HASH` 写进 `.env`，重启服务
2. 打开 `/admin/telegram`，输手机号（带国家区号）→ 收验证码 → 输码；两步验证的再输云密码
3. 粘贴 `t.me/xxx` 这样的公开频道链接添加频道
4. 「同步」逐个下载 txt 入库；「自动同步」设间隔（分钟，0 关闭）

会话文件存在数据目录的 `tg.session`，重启不用重登。增量同步按
`tg_channels.last_message_id` 游标，重复内容再由 `content_hash` 判重兜底。

> Go 版用 `gotd/td`，客户端由 `telegram.Client.Run` 驱动在一个 goroutine 里。
> 登录的三步（发码 / 验码 / 云密码）共用同一个 client，`phone_code_hash` 存在内存里，
> 有效期 5 分钟。

## 前台

- 首页：书架网格（无封面时按书名生成稳定的渐变占位块）、最长篇幅榜
- 书籍页：书籍信息 + 分页目录；单章书隐藏目录
- 阅读页：字号 / 行距 / 主题（纸黄、纯白、夜间）与阅读位置存 `localStorage`
- 搜索页：书名模式走 LIKE，全文模式走 FTS5 并高亮片段

全文检索用 FTS5 external content 表（`content='chapters'`）+ `tokenize='trigram'`：
正文只存一份，中文子串检索可用且无需分词器。关键词短于 3 字时退回 LIKE。

## 书源接口与开源阅读

| 接口 | 说明 |
| --- | --- |
| `GET /api/search?key=&page=` | 书名与作者模糊匹配 |
| `GET /api/book/{id}` | 详情，含最新章节 |
| `GET /api/book/{id}/catalog?page=` | 目录（`id`、`title`、`url`，多页时给 `nextUrl`） |
| `GET /api/chapter/{id}` | 章节正文，段落以 `\n` 分隔纯文本 |

网络导入地址：`http://你的地址:8000/legado/源.json`，按服务端当前的
`SITE_BASE_URL` 与 `API_TOKEN` 实时生成。

## 部署

### Docker

```bash
cp .env.example .env
# 必改：ADMIN_PASSWORD、SECRET_KEY、SITE_BASE_URL=https://你的域名
mkdir -p data
docker compose up -d --build
docker compose logs -f web
```

镜像用多阶段构建，最终镜像是 alpine + 一个静态二进制；`data/` 是唯一的持久化目录
（`novel.db`、WAL、TG 会话、批量导入日志都在里面）。

### 裸机 systemd

```ini
[Unit]
Description=青灯书斋
After=network.target

[Service]
User=novel
WorkingDirectory=/srv/novel
Environment=NOVEL_DATA_DIR=/srv/novel/data
ExecStart=/srv/novel/qingdeng -root /srv/novel
Restart=always
RestartSec=3

[Install]
WantedBy=multi-user.target
```

### 交叉编译

纯 Go SQLite 的好处是交叉编译不需要目标平台的 sqlite：

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o qingdeng ./cmd/server
```

### 健康检查

`GET /healthz` 返回 `{"ok": true, "books": N}`，只数一次 `books` 表，不碰 FTS。
容器的 `HEALTHCHECK` 用的就是它。

## 测试

```bash
go test ./...
```

覆盖三层：

- `internal/parser`：三态分章、标记合理性、超长段切分、广告行清理、书名作者抽取
- `internal/importer`：导入与去重、ZIP 展开与路径穿越拦截、任务进度、重新分章保留人工字段
- `internal/web`：页面渲染、后台鉴权、登录与开放重定向防护、书源 JSON 形状、
  token 鉴权、全文检索高亮、上传到轮询的完整链路

分章基线的期望值与 Python 原版实测输出逐项对齐（marked 8 章 / auto 2 章 / single 1 章）。

## 与 Python 版的行为差异

只列真正会影响使用的地方：

- **编码探测**：原版用 `charset-normalizer` 做统计猜测；Go 版按 BOM → 严格 UTF-8 →
  GB18030 → Big5 的确定性顺序。对中文 txt 命中率相同，但少了统计模型的体积。
- **TG 会话**：原版把 StringSession 存进 `app_settings` 表；Go 版存成数据目录下的
  `tg.session` 文件。换库不再影响登录态，但把库拷到别的机器时要一起拷会话文件。
- **上传文件名**：Windows 浏览器提交中文文件名时 multipart 头里可能是 GBK 字节，
  Go 版会检测并还原；原版由 Werkzeug 处理。
- **`/api/docs`**：原版是 FastAPI 自动生成的 Swagger；Go 版是手写的接口说明页。
- **错误页**：路径里含 `/api/` 或 Accept 要求 JSON 时返回 JSON，否则渲染 HTML 错误页，
  与原版一致。
