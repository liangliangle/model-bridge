#!/bin/bash
# 重启 Go 后端：停止旧进程 → 编译后端 → 后台启动并记录 PID。
#
# 用法：
#   ./restart.sh          # 只编译 Go 后端（秒级，带构建缓存）
#   ./build.sh            # 需要连前端一起重新打包时用这个
#
# 旧的 Rust 版本保留为 ./restart-rust.sh。
set -e

PROJECT_DIR="$(cd "$(dirname "$0")" && pwd)"
BINARY_NAME="model-bridge"
PID_FILE="$PROJECT_DIR/.model-bridge.pid"
BINARY="$PROJECT_DIR/release/$BINARY_NAME"
LOG_FILE="$PROJECT_DIR/model-bridge.log"

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
    # 兜底：按可执行文件名精确匹配（-x），不要用 -f。
    # `pgrep -f model-bridge` 会匹配任何命令行里含该字符串的进程——包括
    # 在仓库目录下执行本脚本的那个 shell（argv 里就有 /path/to/model-bridge），
    # 结果是把调用者自己杀掉。
    PIDS=$(pgrep -x "$BINARY_NAME" 2>/dev/null || true)
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
#
# 只编译 Go 后端，不重新打包（不重新构建前端、不复制 dist），与旧 Rust 版的
# `cargo build --release` 语义一致，且构建缓存命中时只需一两秒。
# 需要连前端一起更新时用 ./build.sh，或重启后单独跑它。
echo "[2/3] 构建后端 (Go)..."
if ! command -v go > /dev/null 2>&1; then
    echo "  找不到 go 命令，请先安装 Go 工具链（1.23+）" >&2
    exit 1
fi
mkdir -p "$PROJECT_DIR/release"
cd "$PROJECT_DIR/go"
# 刻意不吞掉构建输出：失败时要能直接看到原因。
go build -p 1 -o "$BINARY" ./cmd/model-bridge
cd "$PROJECT_DIR"
echo "  构建完成 → $BINARY"

# 3. 启动
echo "[3/3] 启动服务..."
nohup "$BINARY" >> "$LOG_FILE" 2>&1 < /dev/null &
NEW_PID=$!
echo "$NEW_PID" > "$PID_FILE"

# 等端口真正可用，避免脚本报「已启动」而进程其实起不来。
# 端口从「服务端将要读的那份配置」里取，保证探测的端口与实际监听一致。
CONFIG_FILE="$HOME/.model-bridge/config.yaml"
PORT=$(grep -E '^listen_port:' "$CONFIG_FILE" 2>/dev/null | head -1 | awk '{print $2}')
PORT="${PORT:-8080}"
echo "  配置: $CONFIG_FILE (端口 $PORT)"
# 注意 -m：不给超时的 curl 会在端口不响应时挂住整个脚本。
for _ in $(seq 1 30); do
    if curl -fsS -m 2 "http://127.0.0.1:${PORT}/api/auth/status" -o /dev/null 2>/dev/null; then
        echo "  已启动 (PID: $NEW_PID)"
        echo ""
        echo "  管理界面: http://localhost:${PORT}"
        echo "  代理地址: http://localhost:${PORT}/v1/chat/completions"
        echo "  日志查看: tail -f $LOG_FILE"
        exit 0
    fi
    if ! kill -0 "$NEW_PID" 2>/dev/null; then
        echo "  启动失败：进程已退出，日志末尾：" >&2
        tail -n 20 "$LOG_FILE" >&2 || true
        rm -f "$PID_FILE"
        exit 1
    fi
    sleep 0.2
done

echo "启动失败：端口 ${PORT} 在 10 秒内未就绪，日志末尾：" >&2
tail -n 20 "$LOG_FILE" >&2 || true
exit 1
