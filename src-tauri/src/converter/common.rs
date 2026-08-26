//! 协议转换共享工具：结束原因映射、usage 换算、id 生成、工具名脱敏、
//! 多模态图片转换、文本提取等纯函数，供 request/response/stream 复用。

use rand::Rng;
use serde_json::{json, Map, Value};
use std::collections::HashMap;

// ==================== 结束原因映射 ====================

/// Chat finish_reason → Responses status
pub fn finish_reason_to_status(fr: Option<&str>) -> &'static str {
    match fr {
        Some("length") | Some("content_filter") | Some("refusal") => "incomplete",
        _ => "completed",
    }
}

/// Responses status → Chat finish_reason
pub fn status_to_finish_reason(status: &str) -> &'static str {
    match status {
        "incomplete" => "length",
        _ => "stop",
    }
}

/// Anthropic stop_reason → Chat finish_reason
pub fn anthropic_stop_to_finish(stop: Option<&str>) -> &'static str {
    match stop {
        Some("max_tokens") | Some("compaction") => "length",
        Some("tool_use") => "tool_calls",
        Some("refusal") => "content_filter",
        _ => "stop",
    }
}

/// Chat finish_reason → Anthropic stop_reason
pub fn finish_to_anthropic_stop(fr: Option<&str>) -> &'static str {
    match fr {
        Some("length") => "max_tokens",
        Some("tool_calls") | Some("function_call") => "tool_use",
        Some("content_filter") => "refusal",
        _ => "end_turn",
    }
}

// ==================== usage 换算 ====================

fn as_u64(v: &Value, key: &str) -> Option<u64> {
    v.get(key).and_then(|x| x.as_u64())
}

/// Chat usage → Responses usage
pub fn chat_usage_to_responses(usage: &Value) -> Value {
    let input = as_u64(usage, "prompt_tokens").unwrap_or(0);
    let output = as_u64(usage, "completion_tokens").unwrap_or(0);
    let total = as_u64(usage, "total_tokens").unwrap_or(input + output);
    let mut out = json!({
        "input_tokens": input,
        "output_tokens": output,
        "total_tokens": total,
    });
    if let Some(cached) = usage
        .get("prompt_tokens_details")
        .and_then(|d| d.get("cached_tokens"))
        .and_then(|v| v.as_u64())
    {
        out["input_tokens_details"] = json!({ "cached_tokens": cached });
    }
    if let Some(reasoning) = usage
        .get("completion_tokens_details")
        .and_then(|d| d.get("reasoning_tokens"))
        .and_then(|v| v.as_u64())
    {
        out["output_tokens_details"] = json!({ "reasoning_tokens": reasoning });
    }
    out
}

/// Responses usage → Chat usage
pub fn responses_usage_to_chat(usage: &Value) -> Value {
    let input = as_u64(usage, "input_tokens").unwrap_or(0);
    let output = as_u64(usage, "output_tokens").unwrap_or(0);
    let total = as_u64(usage, "total_tokens").unwrap_or(input + output);
    let mut out = json!({
        "prompt_tokens": input,
        "completion_tokens": output,
        "total_tokens": total,
    });
    if let Some(cached) = usage
        .get("input_tokens_details")
        .and_then(|d| d.get("cached_tokens"))
        .and_then(|v| v.as_u64())
    {
        out["prompt_tokens_details"] = json!({ "cached_tokens": cached });
    }
    if let Some(reasoning) = usage
        .get("output_tokens_details")
        .and_then(|d| d.get("reasoning_tokens"))
        .and_then(|v| v.as_u64())
    {
        out["completion_tokens_details"] = json!({ "reasoning_tokens": reasoning });
    }
    out
}

/// Anthropic usage → Chat usage
pub fn anthropic_usage_to_chat(usage: &Value) -> Value {
    let input = as_u64(usage, "input_tokens").unwrap_or(0);
    let cache_read = as_u64(usage, "cache_read_input_tokens").unwrap_or(0);
    let cache_creation = as_u64(usage, "cache_creation_input_tokens").unwrap_or(0);
    let output = as_u64(usage, "output_tokens").unwrap_or(0);
    let prompt = input + cache_read + cache_creation;
    let mut out = json!({
        "prompt_tokens": prompt,
        "completion_tokens": output,
        "total_tokens": prompt + output,
    });
    if cache_read > 0 || cache_creation > 0 {
        let mut details = Map::new();
        if cache_read > 0 { details.insert("cached_tokens".to_string(), json!(cache_read)); }
        if cache_creation > 0 { details.insert("cache_creation_tokens".to_string(), json!(cache_creation)); }
        out["prompt_tokens_details"] = Value::Object(details);
    }
    out
}

/// Chat usage → Anthropic usage
pub fn chat_usage_to_anthropic(usage: &Value) -> Value {
    let input = as_u64(usage, "prompt_tokens").unwrap_or(0);
    let output = as_u64(usage, "completion_tokens").unwrap_or(0);
    let cache_read = usage
        .get("prompt_tokens_details")
        .and_then(|d| d.get("cached_tokens"))
        .and_then(|v| v.as_u64())
        .unwrap_or(0);
    let real_input = input.saturating_sub(cache_read);
    let mut out = json!({
        "input_tokens": real_input,
        "output_tokens": output,
    });
    if cache_read > 0 {
        out["cache_read_input_tokens"] = json!(cache_read);
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn anthropic_cache_creation_usage_is_preserved() {
        let chat = anthropic_usage_to_chat(&json!({
            "input_tokens": 10,
            "cache_read_input_tokens": 3,
            "cache_creation_input_tokens": 7,
            "output_tokens": 2
        }));
        assert_eq!(chat["prompt_tokens"], 20);
        assert_eq!(chat["prompt_tokens_details"]["cached_tokens"], 3);
        assert_eq!(chat["prompt_tokens_details"]["cache_creation_tokens"], 7);
    }
}

// ==================== id 生成 ====================

/// 生成带前缀的随机 id（用 rand，不引入新依赖）。
pub fn gen_id(prefix: &str) -> String {
    const HEX: &[u8] = b"0123456789abcdef";
    let mut rng = rand::thread_rng();
    let hex: String = (0..24)
        .map(|_| HEX[rng.gen_range(0..16)] as char)
        .collect();
    format!("{}{}", prefix, hex)
}

// ==================== 工具名脱敏 ====================

/// 将工具名脱敏为 Anthropic 允许的 `^[a-zA-Z0-9_-]{1,128}$`。
pub fn sanitize_tool_name(name: &str) -> String {
    let mut out: String = name
        .chars()
        .map(|c| if c.is_ascii_alphanumeric() || c == '_' || c == '-' { c } else { '_' })
        .collect();
    if out.is_empty() {
        out.push('_');
    }
    if out.len() > 128 {
        out.truncate(128);
    }
    out
}

/// 工具名正/反向映射表。
#[derive(Debug, Clone, Default)]
pub struct ToolNameMap {
    pub fwd: HashMap<String, String>, // 原始名 → 脱敏名
    pub rev: HashMap<String, String>, // 脱敏名 → 原始名
}

/// 依据工具列表构建正/反向映射，数字后缀消歧。
pub fn build_tool_name_maps(tools: &[Value]) -> ToolNameMap {
    let mut map = ToolNameMap::default();
    let mut used: HashMap<String, u32> = HashMap::new();
    for tool in tools {
        // 支持 Chat（function.name）与 Anthropic/Responses（name）两种形态
        let orig = tool
            .get("function")
            .and_then(|f| f.get("name"))
            .or_else(|| tool.get("name"))
            .and_then(|v| v.as_str());
        let orig = match orig {
            Some(n) => n,
            None => continue,
        };
        if map.fwd.contains_key(orig) {
            continue;
        }
        let base = sanitize_tool_name(orig);
        let mut candidate = base.clone();
        while map.rev.contains_key(&candidate) {
            let n = used.entry(base.clone()).or_insert(1);
            *n += 1;
            candidate = format!("{}_{}", base, n);
        }
        map.fwd.insert(orig.to_string(), candidate.clone());
        map.rev.insert(candidate, orig.to_string());
    }
    map
}

// ==================== 文本提取 ====================

/// 从 content（string 或数组）中提取纯文本。数组内取各 text/*_text 字段拼接。
pub fn extract_text(content: &Value) -> String {
    match content {
        Value::String(s) => s.clone(),
        Value::Array(arr) => {
            let mut out = String::new();
            for part in arr {
                if let Some(t) = part.get("text").and_then(|v| v.as_str()) {
                    out.push_str(t);
                } else if let Some(t) = part.as_str() {
                    out.push_str(t);
                }
            }
            out
        }
        _ => String::new(),
    }
}

// ==================== 多模态图片转换 ====================

/// OpenAI `image_url`（{url, detail}）→ Anthropic image source block。
/// data URI → base64 source；http(s) url → url source。
pub fn openai_image_to_anthropic(image_url: &Value) -> Value {
    let url = image_url
        .get("url")
        .and_then(|v| v.as_str())
        .or_else(|| image_url.as_str())
        .unwrap_or("");
    if let Some(rest) = url.strip_prefix("data:") {
        // data:<media_type>;base64,<data>
        if let Some((meta, data)) = rest.split_once(',') {
            let media_type = meta.split(';').next().unwrap_or("image/png");
            return json!({
                "type": "image",
                "source": {
                    "type": "base64",
                    "media_type": media_type,
                    "data": data,
                }
            });
        }
    }
    json!({
        "type": "image",
        "source": { "type": "url", "url": url }
    })
}

/// Anthropic image source → OpenAI `image_url`。
/// base64 → data URI；url → 直接 url。
pub fn anthropic_image_to_openai(source: &Value) -> Value {
    let stype = source.get("type").and_then(|v| v.as_str()).unwrap_or("");
    match stype {
        "base64" => {
            let media_type = source
                .get("media_type")
                .and_then(|v| v.as_str())
                .unwrap_or("image/png");
            let data = source.get("data").and_then(|v| v.as_str()).unwrap_or("");
            json!({
                "type": "image_url",
                "image_url": { "url": format!("data:{};base64,{}", media_type, data) }
            })
        }
        _ => {
            let url = source.get("url").and_then(|v| v.as_str()).unwrap_or("");
            json!({
                "type": "image_url",
                "image_url": { "url": url }
            })
        }
    }
}

// ==================== 杂项 ====================

/// 从 map 中移除所有 null 值字段。
pub fn strip_nulls(obj: &mut Map<String, Value>) {
    obj.retain(|_, v| !v.is_null());
}

/// reasoning_effort → thinking budget_tokens 分桶。
pub fn effort_to_budget(effort: &str) -> u64 {
    match effort {
        "minimal" => 1024,
        "low" => 1024,
        "medium" => 2048,
        "high" => 4096,
        "xhigh" => 8192,
        "max" => 16384,
        _ => 2048,
    }
}

// ==================== namespace 工具展平与反向映射 ====================

use parking_lot::RwLock;
use std::sync::Arc;

/// 请求级共享状态：flat_name → (namespace_name, subtool_name)。
/// 请求阶段写入，响应/流式阶段读取，用于还原 namespace 结构。
pub type NsReverseMap = Arc<RwLock<HashMap<String, (String, String)>>>;

pub fn new_ns_reverse_map() -> NsReverseMap {
    Arc::new(RwLock::new(HashMap::new()))
}

/// 将 namespace 工具内的 subtool 展平为 Chat function tool，命名 `{ns}__{sub}`，
/// 同时写入 ns_reverse 反向映射。冲突时加 `_2`/`_3` 后缀消歧。
pub fn flatten_namespace_subtool(
    ns_name: &str,
    subtool: &Value,
    ns_map: &mut HashMap<String, (String, String)>,
) -> Option<Value> {
    let sub_name = subtool.get("name").and_then(|v| v.as_str())?;
    let flat_raw = format!(
        "{}__{}",
        sanitize_tool_name(ns_name),
        sanitize_tool_name(sub_name)
    );
    let flat_base = sanitize_tool_name(&flat_raw);
    let mut candidate = flat_base.clone();
    let mut suffix = 2u32;
    while ns_map.contains_key(&candidate) {
        candidate = format!("{}_{}", flat_base, suffix);
        suffix += 1;
    }
    ns_map.insert(candidate.clone(), (ns_name.to_string(), sub_name.to_string()));

    let mut func = Map::new();
    func.insert("name".to_string(), json!(candidate));
    if let Some(desc) = subtool.get("description") {
        func.insert("description".to_string(), desc.clone());
    }
    let params = subtool
        .get("parameters")
        .cloned()
        .unwrap_or_else(|| json!({"type": "object"}));
    func.insert("parameters".to_string(), params);
    Some(json!({"type": "function", "function": Value::Object(func)}))
}

/// Marker used in `NsReverseMap` for Responses `custom` tools.  The existing
/// map is request-scoped and shared by the response/stream converters, so a
/// marker keeps custom-tool identity without introducing another state object.
pub const CUSTOM_TOOL_NAMESPACE_MARKER: &str = "__model_bridge_custom_tool__";

/// Responses custom tools are represented as Chat function tools while they
/// traverse the canonical representation.  Their original name is retained
/// in the reverse map so output can be emitted as `custom_tool_call` again.
pub fn register_custom_tool(
    name: &str,
    ns_map: &mut HashMap<String, (String, String)>,
) {
    if !name.is_empty() {
        ns_map.insert(
            name.to_string(),
            (CUSTOM_TOOL_NAMESPACE_MARKER.to_string(), name.to_string()),
        );
    }
}

/// Unwrap the Chat function-tool envelope used for Responses custom tools.
/// Invalid/non-object JSON is already a valid raw custom-tool payload and is
/// therefore returned unchanged.
pub fn unwrap_custom_tool_arguments(arguments: &str) -> String {
    if arguments.is_empty() {
        return String::new();
    }
    match serde_json::from_str::<Value>(arguments) {
        Ok(Value::Object(obj)) => obj
            .get("content")
            .and_then(|v| v.as_str())
            .map(str::to_string)
            .unwrap_or_else(|| arguments.to_string()),
        _ => arguments.to_string(),
    }
}
