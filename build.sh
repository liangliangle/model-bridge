#!/bin/bash
set -e

PROJECT_DIR="$(cd "$(dirname "$0")" && pwd)"
OUTPUT_DIR="$PROJECT_DIR/release"

echo "=== Model Bridge 打包 ==="
echo ""

# 1. 构建前端
echo "[1/3] 构建前端..."
cd "$PROJECT_DIR"
pnpm build
echo "  前端构建完成 → dist/"
echo ""

# 2. 构建后端（前端已内嵌到二进制中）
echo "[2/3] 构建后端 (release, 内嵌前端)..."
cd "$PROJECT_DIR/src-tauri"
cargo build --release
echo "  后端构建完成"
echo ""

# 3. 输出产物
echo "[3/3] 输出产物..."
rm -rf "$OUTPUT_DIR"
mkdir -p "$OUTPUT_DIR"

cp "$PROJECT_DIR/src-tauri/target/release/model-bridge" "$OUTPUT_DIR/"
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
echo "  ./release/model-bridge"
echo ""
echo "  管理界面: http://localhost:8080"
echo "  代理地址: http://localhost:8080/v1/chat/completions"
echo "  配置文件: ~/.model-bridge/config.yaml"
