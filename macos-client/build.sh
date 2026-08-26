#!/bin/bash
set -e

cd "$(dirname "$0")"

echo "🔨 正在编译并打包 Model Bridge 菜单栏 APP 与 DMG..."
./package.sh

echo ""
echo "🚀 启动菜单栏 APP:"
echo "open \"$(dirname "$0")/../release/Model Bridge MenuBar.app\""
