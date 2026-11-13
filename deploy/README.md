# 云服务器部署

镜像已推送到 Docker Hub，直接拉取运行，不需要在服务器上编译。

- `bxacc123/qingdeng:latest`
- `bxacc123/qingdeng:2026.11.14`

## 一分钟跑起来

```bash
mkdir -p /srv/qingdeng && cd /srv/qingdeng
curl -O https://raw.githubusercontent.com/3166790565/qingdeng/main/docker-compose.prod.yml
# 代理只监听 127.0.0.1 且无法改为 0.0.0.0 时，用这个：
# curl -O https://raw.githubusercontent.com/3166790565/qingdeng/main/docker-compose.prod-host.yml
curl -O https://raw.githubusercontent.com/3166790565/qingdeng/main/.env.example
mv .env.example .env
vi .env      # 必改 ADMIN_PASSWORD / SECRET_KEY / SITE_BASE_URL
mkdir -p data

IMAGE=bxacc123/qingdeng:latest docker compose -f docker-compose.prod.yml up -d
docker compose -f docker-compose.prod.yml logs -f web
```

打开 `http://服务器IP:8000`，`/admin` 用 `.env` 里的密码登录。

## 只用 docker run

```bash
docker run -d --name qingdeng --restart unless-stopped \
  -p 8000:8000 \
  --env-file .env \
  -e NOVEL_DATA_DIR=/data -e NOVEL_DB_PATH=/data/novel.db \
  -v /srv/qingdeng/data:/data \
  bxacc123/qingdeng:latest
```

## 连不上 TG：配置代理

在**后台「TG 频道」页 → 出站代理**里填，保存即生效，不用重启。
旁边有「测试连接」，会实际拨号到 Telegram 的 DC 地址，告诉你通不通、卡在哪。

支持 `socks5://` 与 `http://`（Clash 的 Mixed 端口两者都认），留空直连。
**代理配置不在 `.env` 里**，只存数据库——两个来源并存时最难排障。

### Docker 下的地址是最大的坑

容器里的 `127.0.0.1` 指向容器自己，不是宿主机。你的代理跑在宿主机 `127.0.0.1:7890`，
所以**不能直接填 `127.0.0.1:7890`**。三种正确写法：

| 你的情况 | 填什么 |
| --- | --- |
| 代理在宿主机，用 `docker-compose.prod.yml` | `socks5://host.docker.internal:7890` |
| 代理在另一个容器里 | `socks5://那个容器的名字:7890` |
| 代理只监听 127.0.0.1 且改不了 | 改用 `docker-compose.prod-host.yml`（host 网络），才能填 `socks5://127.0.0.1:7890` |

`docker-compose.prod.yml` 已经加了 `extra_hosts: host.docker.internal:host-gateway`，
Linux 上也能解析这个名字。

代理自己如果是容器，必须监听 `0.0.0.0` 而不是 `127.0.0.1`。
Clash 系面板要打开「允许局域网」，否则只监听 `127.0.0.1`。

### 检测结果对照

| 提示 | 该做什么 |
| --- | --- |
| 连接 Telegram 成功 | 链路通，去发验证码 |
| 代理拒绝连接 | 确认代理在跑、端口对 |
| 代理可能没放行 Telegram | 代理通了但出口到不了 TG，换节点 |
| 解析不到 host.docker.internal | 用仓库里的 compose（含 extra_hosts） |
| 代理握手失败 | 换 `http://` 或 `socks5://` 前缀 |
| 连接超时：直连 Telegram 不通 | 还没配代理 |

### host 网络方案

```bash
IMAGE=bxacc123/qingdeng:latest   docker compose -f docker-compose.prod-host.yml up -d
```

然后在后台把代理填成 `socks5://127.0.0.1:7890`。

代价是端口映射失效、直接占宿主机 8000，且与宿主其它进程共享网络命名空间。

## 前置：nginx + HTTPS

`docker-compose.prod.yml` 默认只绑 `127.0.0.1`，靠前面的 nginx 出去。
想直接暴露公网就在 `.env` 里写 `BIND_ADDR=0.0.0.0`——但那之前请先读 README 里的风险说明，
至少把后台加一层 basic auth。

```nginx
server {
    listen 443 ssl;
    server_name 你的域名;
    client_max_body_size 600m;
    proxy_read_timeout 300s;

    location / {
        proxy_pass http://127.0.0.1:8000;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }
}
```

## 数据

容器里只有 `/data` 需要持久化：`novel.db`、WAL、`tg.session`、批量导入日志。
换机器就是把这个目录拷过去。

想在服务不停的情况下导一份一致的快照：

```bash
docker exec qingdeng /usr/local/bin/qingdeng -check-sqlite >/dev/null   # 探活
python3 -c "import sqlite3;sqlite3.connect('/srv/qingdeng/data/novel.db').execute(\"VACUUM INTO '/srv/qingdeng/data/snapshot.db'\")"
```

## 升级

```bash
docker compose -f docker-compose.prod.yml pull
docker compose -f docker-compose.prod.yml up -d
```

库在挂载卷里，重建容器不影响数据。

## 关于 scratch 镜像

这个镜像的根文件系统是空的（`FROM scratch`），只有：

- `/usr/local/bin/qingdeng`（静态编译，不依赖 glibc）
- `/app/templates`、`/app/static`、`/app/legado`、`/app/schema.sql`
- `/usr/share/zoneinfo`（时区）与 `/etc/ssl/certs/ca-certificates.crt`（HTTPS 出站）

所以：

- **没有 shell**，`docker exec sh` 用不了；排障看 `docker logs`。
- **没有 curl/wget**，健康检查要由宿主机或编排层打（compose 里的 `healthcheck` 已经用
  `/dev/tcp` 实现，不依赖容器内工具）。
- 证书与时区是构建时从构建机拷进来的，一年左右建议重建一次镜像以更新 CA。

## 自己构建镜像

如果你改过代码，想在本地重新构建并推送：

```bash
# 1. 交叉编译 Linux 静态二进制
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -trimpath -ldflags="-s -w" -o build/qingdeng-linux-amd64 ./cmd/server

# 2. 构建（scratch 镜像，不依赖拉任何基础镜像）
docker build -f Dockerfile.runtime -t bxacc123/qingdeng:新标签 .

# 3. 推送
docker push bxacc123/qingdeng:新标签
```

需要 ARM 服务器（如 Oracle 免费机 / 树莓派）：

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 \
  go build -trimpath -ldflags="-s -w" -o build/qingdeng-linux-arm64 ./cmd/server
```

然后把 `Dockerfile.runtime` 里的二进制路径改成 arm64 那份再构建。

## 完整镜像（含 Go 构建链）

`Dockerfile`（不带 `.runtime`）是多阶段构建，适合在能访问 Docker Hub 的 CI 里用。
本机访问 Hub 受限时用 `Dockerfile.runtime`。
