//! MCP 工具黑名单过滤逻辑：
//! - tools/list 响应：从 result.tools[] 删除黑名单工具
//! - tools/call 请求：调用黑名单工具时直接返回 JSON-RPC error

use axum::http::StatusCode;
use axum::response::{IntoResponse, Response};
use axum::Json;
use serde_json::{json, Value};

/// 构造拒绝调用黑名单工具的 JSON-RPC error 响应（HTTP 200 + error body）。
/// id 原样回显（可能是数字/字符串/null）。
pub fn reject_tool_call(req_id: &Value, tool_name: &str) -> Response {
    let body = json!({
        "jsonrpc": "2.0",
        "id": req_id,
        "error": {
            "code": -32602,
            "message": format!("Unknown tool: {}", tool_name),
        }
    });
    (StatusCode::OK, Json(body)).into_response()
}

/// 从 tools/list 的 JSON-RPC response 中过滤掉黑名单工具。
/// 直接在传入的 Value 上原地修改 result.tools[]，保留 nextCursor 等其它字段。
pub fn filter_tools_in_response(rpc: &mut Value, blocked: &[String]) {
    if let Some(tools) = rpc
        .pointer_mut("/result/tools")
        .and_then(|v| v.as_array_mut())
    {
        tools.retain(|t| {
            let name = t.get("name").and_then(|n| n.as_str()).unwrap_or("");
            !blocked.iter().any(|b| b == name)
        });
    }
}

/// 从上游响应文本中提取目标 JSON-RPC response。
/// 兼容两种上游响应形态：
/// 1. application/json：整个 body 就是一个 JSON-RPC response
/// 2. text/event-stream：SSE 帧，response 嵌在某个 event 的 data: 行里
///
/// 优先返回 id 匹配 req_id 且含 "result" 或 "error" 的那条；
/// 解析失败则返回 None（调用方应回退到原样透传）。
pub fn extract_jsonrpc_response(body: &str, req_id: &Value) -> Option<Value> {
    // 先尝试整体作为单个 JSON 解析（application/json 情况）
    if let Ok(v) = serde_json::from_str::<Value>(body.trim()) {
        if is_target_response(&v, req_id) {
            return Some(v);
        }
    }

    // 回退：按 SSE 帧解析，逐个 event 的 data 负载尝试
    for payload in iter_sse_data_payloads(body) {
        if let Ok(v) = serde_json::from_str::<Value>(&payload) {
            if is_target_response(&v, req_id) {
                return Some(v);
            }
        }
    }

    None
}

/// 判断一个 JSON 值是否是我们要找的 JSON-RPC response：
/// 含 result 或 error，且 id 与请求 id 相等。
fn is_target_response(v: &Value, req_id: &Value) -> bool {
    let has_payload = v.get("result").is_some() || v.get("error").is_some();
    if !has_payload {
        return false;
    }
    // id 相等即匹配；若请求 id 缺失/为 null 则放宽（只要是带 result 的 response 即可）
    match v.get("id") {
        Some(id) => id == req_id,
        None => req_id.is_null(),
    }
}

/// 从 SSE 文本中提取每个 event 的 data 负载（多行 data: 拼接为一个负载）。
/// SSE 规则：以空行分隔 event；event 内 "data:" 开头的行去前缀后按换行拼接。
fn iter_sse_data_payloads(body: &str) -> Vec<String> {
    let mut payloads = Vec::new();
    let mut current = String::new();
    let mut has_data = false;

    for line in body.lines() {
        if line.is_empty() {
            // event 边界
            if has_data {
                payloads.push(std::mem::take(&mut current));
            }
            current.clear();
            has_data = false;
            continue;
        }
        if let Some(rest) = line.strip_prefix("data:") {
            // data: 后可能有一个可选空格
            let rest = rest.strip_prefix(' ').unwrap_or(rest);
            if has_data {
                current.push('\n');
            }
            current.push_str(rest);
            has_data = true;
        }
        // 其它字段（event:/id:/retry:/注释行）忽略
    }
    // 最后一个 event 没有以空行结尾的情况
    if has_data {
        payloads.push(current);
    }

    payloads
}
