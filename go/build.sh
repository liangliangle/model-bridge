#!/usr/bin/env bash
# 构建 Go 后端。
#
# 前端产物必须先落到 go/internal/web/dist，因为 //go:embed 无法引用模块目录之外
# 的文件（见规划 Deviations）。
#
# 用法：
#   ./build.sh                 # 复用仓库根目录已有的 dist/，直接编译
#   BUILD_FRONTEND=1 ./build.sh  # 先构建前端再编译
set -euo pipefail

cd "$(dirname "$0")"
REPO_ROOT="$(cd .. && pwd)"

if [ "${BUILD_FRONTEND:-0}" = "1" ]; then
  echo "==> 构建前端"
  (cd "$REPO_ROOT" && pnpm install --frozen-lockfile && pnpm build)
fi

if [ ! -d "$REPO_ROOT/dist" ]; then
  echo "错误：找不到 $REPO_ROOT/dist，请先执行 pnpm build" >&2
  exit 1
fi

echo "==> 同步前端产物到 internal/web/dist"
rm -rf internal/web/dist
cp -R "$REPO_ROOT/dist" internal/web/dist

echo "==> 编译 Go 后端"
mkdir -p bin
# 串行编译：本机可用内存有限（见规划 Risks）。
go build -p 1 -o bin/model-bridge ./cmd/model-bridge

echo "==> 完成：$(pwd)/bin/model-bridge"
