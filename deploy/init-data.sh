#!/bin/sh
# 备好 ./data 并交给容器里那个 uid（1000:1000）。
#
# 为什么需要：镜像以非 root 运行（scratch + USER 1000:1000），
# bind mount 的目录属主不跟着变。目录是 root 的话，SQLite 建不了库，
# 程序会直接退出并提示执行 chown。
#
#   sh deploy/init-data.sh [数据目录，默认 ./data]
set -eu

DIR="${1:-./data}"
UID_GID="1000:1000"

mkdir -p "$DIR"
# 容器里也可能已经写过文件，-R 保证全部对齐
if command -v sudo >/dev/null 2>&1 && [ "$(id -u)" -ne 0 ]; then
  sudo chown -R "$UID_GID" "$DIR"
else
  chown -R "$UID_GID" "$DIR"
fi
chmod 755 "$DIR"

echo "已把 $DIR 归属改成 $UID_GID"
ls -ld "$DIR"
