#!/usr/bin/env bash
# 启动检查：连续两次用真实二进制启动后端，断言
#   1) 进程持续监听配置端口；
#   2) 管理端点的**响应体**是结构合理的 JSON（不是仅断言 HTTP 200）；
#   3) GET / 返回内嵌前端页面（HTML 且含脚本引用），不是 404 或空体；
#   4) 两次启动的响应结构一致。
#
# 证据写入 $SCRATCH：startup-1.log / startup-1.json / startup-2.json / startup-root.html
set -euo pipefail

cd "$(dirname "$0")/.."
GO_DIR="$(pwd)"
SCRATCH="${SCRATCH:-/tmp/codel-goal-0e25eade7804/implementer}"
mkdir -p "$SCRATCH"

PORT="${PORT:-18099}"
BASE="http://127.0.0.1:${PORT}"

if [ ! -x bin/model-bridge ]; then
  echo "==> 未找到 bin/model-bridge，先执行 build.sh"
  ./build.sh
fi

run_once() {
  local run="$1"
  local home="$SCRATCH/home-run${run}"
  rm -rf "$home"
  mkdir -p "$home/.model-bridge"
  cat > "$home/.model-bridge/config.yaml" <<YAML
listen_port: ${PORT}
listen_host: "127.0.0.1"
channels: []
failover:
  max_failover_channels: 3
  retry_timeout_ms: 5000
  circuit_breaker:
    failure_threshold: 3
    recovery_interval_sec: 30
    probe_requests: 2
auth:
  proxy_tokens: []
YAML

  echo "==> 第 ${run} 次启动"
  HOME="$home" ./bin/model-bridge > "$SCRATCH/startup-${run}.log" 2>&1 &
  local pid=$!

  # 等待端口可用（最多 10 秒）
  local ready=0
  for _ in $(seq 1 50); do
    if curl -fsS "${BASE}/api/auth/status" -o /dev/null 2>/dev/null; then ready=1; break; fi
    sleep 0.2
  done
  if [ "$ready" != "1" ]; then
    echo "启动失败：端口 ${PORT} 未就绪" >&2
    cat "$SCRATCH/startup-${run}.log" >&2
    kill "$pid" 2>/dev/null || true
    return 1
  fi

  # 断言进程仍在监听（不是启动后立刻退出）
  if ! kill -0 "$pid" 2>/dev/null; then
    echo "启动失败：进程已退出" >&2
    cat "$SCRATCH/startup-${run}.log" >&2
    return 1
  fi

  curl -fsS "${BASE}/api/auth/status" -o "$SCRATCH/startup-${run}.json"
  curl -fsS "${BASE}/" -o "$SCRATCH/startup-root.html"
  # 再取一次管理端点，证明不是偶发成功
  curl -fsS "${BASE}/api/channels" >> "$SCRATCH/startup-${run}.json"

  kill "$pid" 2>/dev/null || true
  wait "$pid" 2>/dev/null || true
  echo "==> 第 ${run} 次启动成功"
}

run_once 1
run_once 2

echo "==> 校验响应结构"
python3 - "$SCRATCH" <<'PY'
import json, sys, pathlib
scratch = pathlib.Path(sys.argv[1])
one = (scratch / 'startup-1.json').read_text()
two = (scratch / 'startup-2.json').read_text()

def parse_two_objects(text):
    dec = json.JSONDecoder()
    idx, out = 0, []
    while idx < len(text):
        while idx < len(text) and text[idx].isspace():
            idx += 1
        if idx >= len(text):
            break
        obj, end = dec.raw_decode(text, idx)
        out.append(obj)
        idx = end
    return out

a, b = parse_two_objects(one), parse_two_objects(two)
assert len(a) == 2 and len(b) == 2, f"应各有两个 JSON 响应体，实际 {len(a)} / {len(b)}"
status, channels = a
assert 'required' in status and 'valid' in status, f"/api/auth/status 缺字段: {status}"
assert isinstance(channels, list), f"/api/channels 应为数组，实际 {type(channels)}"
assert a == b, "两次启动的响应结构不一致"

html = (scratch / 'startup-root.html').read_text()
assert '<script' in html, "GET / 返回的 HTML 不含脚本引用"
assert '/assets/' in html, "GET / 返回的 HTML 未引用构建产物"

print("启动检查通过：")
print("  /api/auth/status ->", json.dumps(status, ensure_ascii=False))
print("  /api/channels    ->", json.dumps(channels, ensure_ascii=False))
print("  两次启动响应一致 ✔")
print("  GET / 返回内嵌前端 HTML（含 <script> 与 /assets/ 引用）✔")
PY
