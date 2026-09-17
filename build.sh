#!/bin/bash
# 构建 Go 后端并把产物打包到 release/。
#
# 与 Rust 版的差别：编译 Go 而不是 cargo，并把内嵌前端从仓库根的 dist/
# 同步进 go/internal/web/dist/（//go:embed 不能引用模块目录之外的文件）。
#
# 用法：
#   ./build.sh                 # 构建前端 + 后端
#   SKIP_FRONTEND=1 ./build.sh # 复用已有的 dist/，只重新编译后端
#
# 旧的 Rust 版本保留为 ./build-rust.sh。
set -e

PROJECT_DIR="$(cd "$(dirname "$0")" && pwd)"
OUTPUT_DIR="$PROJECT_DIR/release"

echo "=== Model Bridge 打包 (Go) ==="
echo ""

if [ "${SKIP_FRONTEND:-0}" = "1" ]; then
    BUILD_FRONTEND=0
    echo "[1/3] 跳过前端构建（SKIP_FRONTEND=1），复用已有 dist/"
else
    BUILD_FRONTEND=1
    echo "[1/3] 构建前端..."
fi

echo "[2/3] 构建后端 (Go, 内嵌前端)..."
cd "$PROJECT_DIR/go"
BUILD_FRONTEND="$BUILD_FRONTEND" ./build.sh
cd "$PROJECT_DIR"
echo "  后端构建完成"
echo ""

# 3. 输出产物
echo "[3/3] 输出产物..."
rm -rf "$OUTPUT_DIR"
mkdir -p "$OUTPUT_DIR"

# 先写临时名再 rename：服务正在运行时直接 cp 覆盖会被 ETXTBSY（Text file busy）拒绝，
# 而 rename 只是换目录项，运行中的进程仍持有旧 inode，不受影响。
cp "$PROJECT_DIR/go/bin/model-bridge" "$OUTPUT_DIR/model-bridge.new"
mv -f "$OUTPUT_DIR/model-bridge.new" "$OUTPUT_DIR/model-bridge"
cp "$PROJECT_DIR/config.example.yaml" "$OUTPUT_DIR/"

BINARY_SIZE=$(du -sh "$OUTPUT_DIR/model-bridge" | cut -f1)

echo ""
echo "=== 打包完成 ==="
echo ""
echo "  输出目录: $OUTPUT_DIR/"
echo "  ├── model-bridge          ($BINARY_SIZE) ← 单文件，内含前端"
echo "  └── config.example.yaml"
echo ""
echo "使用方式:"
echo "  ./release/model-bridge        # 前台运行"
echo "  ./restart.sh                  # 后台重启（停止旧进程 → 构建 → 启动）"
echo ""
echo "  管理界面: http://localhost:8080"
echo "  代理地址: http://localhost:8080/v1/chat/completions"
echo "  配置文件: ~/.model-bridge/config.yaml"
