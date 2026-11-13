# 青灯书斋运行镜像：单进程 HTTP 服务，可变数据全在 /data（由外面挂载进来）。
# 镜像里不含 .env 与库文件，换库只换挂载卷里的文件。
#
# 用 modernc.org/sqlite（纯 Go 实现），所以 CGO_ENABLED=0 也能带 FTS5，
# 构建阶段和运行阶段都不需要系统 sqlite 库。
FROM golang:1.26-alpine AS build

ARG GOPROXY=https://goproxy.cn,direct
ENV CGO_ENABLED=0 GOOS=linux GOPROXY=${GOPROXY} GOSUMDB=off

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# -trimpath 去掉构建机路径；-s -w 去掉调试符号，镜像小一半
RUN go build -trimpath -ldflags="-s -w" -o /out/qingdeng ./cmd/server

# 构建期就验证 FTS5 + trigram 可用，免得等有人搜索时才发现问题
RUN /out/qingdeng -root /src -check-sqlite && echo "fts5 + trigram OK"

FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata wget \
    && addgroup -g 1000 novel && adduser -D -u 1000 -G novel novel

ENV TZ=Asia/Shanghai \
    NOVEL_DATA_DIR=/data \
    NOVEL_DB_PATH=/data/novel.db \
    HOST=0.0.0.0 \
    PORT=8000

WORKDIR /app
COPY --from=build /out/qingdeng /usr/local/bin/qingdeng
COPY schema.sql ./
COPY templates ./templates
COPY static ./static
COPY legado ./legado

RUN mkdir -p /data && chown -R novel:novel /data /app
USER novel

EXPOSE 8000

HEALTHCHECK --interval=30s --timeout=10s --start-period=20s --retries=3 \
    CMD wget -qO- http://127.0.0.1:8000/healthz >/dev/null || exit 1

# -root /app：模板与 schema 都在这里，不依赖当前工作目录
CMD ["qingdeng", "-root", "/app"]
