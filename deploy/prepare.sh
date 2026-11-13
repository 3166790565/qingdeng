#!/bin/sh
# 准备 Dockerfile.runtime 需要、但不适合入库的构建输入：
# 静态资源副本、CA 证书、时区数据、Linux 静态二进制。
#
#   sh deploy/prepare.sh [amd64|arm64]
#
# 在没有 Docker Hub 访问的机器上构建镜像时用这个脚本，避免依赖基础镜像。
set -eu

ARCH="${1:-amd64}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

echo "==> 交叉编译 linux/$ARCH 静态二进制"
mkdir -p build
CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" \
  go build -trimpath -ldflags="-s -w" -o build/qingdeng-linux ./cmd/server

echo "==> 复制静态资源"
rm -rf deploy/static-root
mkdir -p deploy/static-root
cp -r templates static legado schema.sql deploy/static-root/

echo "==> 准备 CA 证书"
mkdir -p deploy/rootfs
CA=""
for candidate in \
  /etc/ssl/certs/ca-certificates.crt \
  /etc/pki/tls/certs/ca-bundle.crt \
  "/c/Program Files/Git/usr/ssl/certs/ca-bundle.crt" \
  "/c/Program Files/Git/mingw64/etc/ssl/certs/ca-bundle.crt"
do
  if [ -f "$candidate" ]; then CA="$candidate"; break; fi
done
if [ -z "$CA" ]; then
  echo "找不到 CA 证书包，请手动放到 deploy/rootfs/ca-certificates.crt" >&2
  exit 1
fi
cp "$CA" deploy/rootfs/ca-certificates.crt

echo "==> 准备时区数据"
ZONE_SRC=""
for candidate in /usr/share/zoneinfo "/c/Program Files/Go/lib/time/zoneinfo.zip"; do
  if [ -e "$candidate" ]; then ZONE_SRC="$candidate"; break; fi
done
rm -rf deploy/rootfs/zoneinfo
mkdir -p deploy/rootfs/zoneinfo
case "$ZONE_SRC" in
  *.zip)
    # Windows 上 Go 发行版自带 zoneinfo.zip，用 python 解出需要的那几个
    python - "$ZONE_SRC" <<'PY'
import os, sys, zipfile
src, dst = sys.argv[1], "deploy/rootfs/zoneinfo"
want = {"Asia/Shanghai", "UTC", "Etc/UTC", "PRC", "Asia/Chongqing"}
with zipfile.ZipFile(src) as z:
    for info in z.infolist():
        if info.filename in want:
            out = os.path.join(dst, info.filename)
            os.makedirs(os.path.dirname(out), exist_ok=True)
            with open(out, "wb") as f:
                f.write(z.read(info))
PY
    ;;
  "")
    echo "找不到时区数据，请手动放到 deploy/rootfs/zoneinfo/" >&2
    exit 1
    ;;
  *)
    cp "$ZONE_SRC/Asia/Shanghai" deploy/rootfs/zoneinfo/ 2>/dev/null || true
    mkdir -p deploy/rootfs/zoneinfo/Etc
    cp "$ZONE_SRC/UTC" deploy/rootfs/zoneinfo/UTC 2>/dev/null || true
    cp "$ZONE_SRC/Etc/UTC" deploy/rootfs/zoneinfo/Etc/UTC 2>/dev/null || true
    ;;
esac

echo "==> 完成。接下来："
echo "    docker build -f Dockerfile.runtime -t bxacc123/qingdeng:latest ."
