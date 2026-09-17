#!/bin/bash
set -e

PROJECT_DIR="$(cd "$(dirname "$0")" && pwd)"
BINARY_NAME="model-bridge"
PID_FILE="$PROJECT_DIR/.model-bridge.pid"

# 1. 停止旧进程
if [ -f "$PID_FILE" ]; then
    OLD_PID=$(cat "$PID_FILE")
    if kill -0 "$OLD_PID" 2>/dev/null; then
        echo "[1/3] 停止旧进程 (PID: $OLD_PID)..."
        kill "$OLD_PID"
        sleep 1
        # 如果还没退出，强制杀掉
        if kill -0 "$OLD_PID" 2>/dev/null; then
            kill -9 "$OLD_PID"
        fi
        echo "  已停止"
    else
        echo "[1/3] 旧进程已不在运行"
    fi
    rm -f "$PID_FILE"
else
    # 兜底：按进程名查找并杀掉
    PIDS=$(pgrep -f "$BINARY_NAME" 2>/dev/null || true)
    if [ -n "$PIDS" ]; then
        echo "[1/3] 停止运行中的进程 (PID: $PIDS)..."
        echo "$PIDS" | xargs kill 2>/dev/null
        sleep 1
        echo "$PIDS" | xargs kill -9 2>/dev/null || true
        echo "  已停止"
    else
        echo "[1/3] 无运行中的进程"
    fi
fi

# 2. 构建后端
echo "[2/3] 构建后端 (release)..."
cd "$PROJECT_DIR/src-tauri"
cargo build --release
echo "  构建完成"

# 3. 启动
echo "[3/3] 启动服务..."
cd "$PROJECT_DIR"
"$PROJECT_DIR/src-tauri/target/release/$BINARY_NAME" &
NEW_PID=$!
echo "$NEW_PID" > "$PID_FILE"
echo "  已启动 (PID: $NEW_PID)"
echo ""
echo "  管理界面: http://localhost:8080"
echo "  代理地址: http://localhost:8080/v1/chat/completions"
echo "  日志查看: tail -f $PROJECT_DIR/model-bridge.log"
