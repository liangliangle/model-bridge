//! 请求体转换：入口格式 ↔ Chat（canonical）↔ 目标格式。
//! 所有函数签名统一 `(&Value, &str) -> Value`，返回新 Value。

use serde_json::{json, Map, Value};

use super::common::{
    self, anthropic_image_to_openai, effort_to_budget, extract_text, flatten_namespace_subtool,
    new_ns_reverse_map, openai_image_to_anthropic, register_custom_tool,
    NsReverseMap,
};

const DEFAULT_ANTHROPIC_MAX_TOKENS: i64 = 4096;

fn obj() -> Map<String, Value> {
    Map::new()
}

// ==================== Responses → Chat ====================

/// Responses 请求 → Chat 请求（向后兼容，不填充 ns_reverse）。
pub fn responses_to_openai(req: &Value, model: &str) -> Value {
    let dummy = new_ns_reverse_map();
    responses_to_openai_with_ns(req, model, &dummy)
}

/// Validate Responses state before converting to a provider without native
/// Responses continuation semantics. Silently dropping these fields makes a
/// request appear successful while breaking the next turn.
pub fn validate_responses_cross_format(req: &Value) -> Result<(), String> {
    for key in ["previous_response_id", "conversation"] {
        if req.get(key).map(|v| !v.is_null()).unwrap_or(false) {
            return Err(format!("Responses field '{}' cannot be continued across protocol conversion", key));
        }
    }
    // `include: ["reasoning.encrypted_content"]` only controls an optional
    // Responses response field. It is intentionally omitted by the target
    // protocol conversion; rejecting the whole request would make otherwise
    // portable Responses calls fail before reaching the provider.
    if let Some(items) = req.get("input").and_then(|v| v.as_array()) {
        for item in items {
            if item.get("type").and_then(|v| v.as_str()) == Some("reasoning")
                && item.get("encrypted_content").is_some()
                && item.get("summary").and_then(|v| v.as_array()).map(|a| a.is_empty()).unwrap_or(true)
            {
                return Err("Responses encrypted reasoning has no portable summary for protocol conversion".to_string());
            }
        }
    }
    Ok(())
}

/// Responses 请求 → Chat 请求，同时展平 namespace 工具并填充 ns_reverse 反向映射。
pub fn responses_to_openai_with_ns(req: &Value, model: &str, ns_reverse: &NsReverseMap) -> Value {
    let mut out = obj();
    out.insert("model".to_string(), json!(model));

    let mut messages: Vec<Value> = Vec::new();

    if let Some(instr) = req.get("instructions").and_then(|v| v.as_str()) {
        if !instr.is_empty() {
            messages.push(json!({"role": "system", "content": instr}));
        }
    }

    match req.get("input") {
        Some(Value::String(s)) => {
            messages.push(json!({"role": "user", "content": s}));
        }
        Some(Value::Array(items)) => {
            responses_input_items_to_messages(items, &mut messages);
        }
        _ => {}
    }

    out.insert("messages".to_string(), json!(messages));

    if let Some(mt) = req.get("max_output_tokens").and_then(|v| v.as_i64()) {
        out.insert("max_tokens".to_string(), json!(mt));
    }

    // Tools may be declared either at the Responses top level or inside an
    // `additional_tools` input item (Codex uses the latter for its control
    // tools).  Flatten both locations into the canonical Chat tool list.
    let tools = collect_responses_tools(req);
    if !tools.is_empty() {
        let mut converted: Vec<Value> = Vec::new();
        let mut ns_map = ns_reverse.write();
        for tool in &tools {
            let ttype = tool.get("type").and_then(|v| v.as_str()).unwrap_or("function");
            match ttype {
                "function" => {
                    if let Some(ct) = responses_tool_to_chat(tool) {
                        converted.push(ct);
                    }
                }
                "custom" => {
                    if let Some(ct) = responses_custom_tool_to_chat(tool) {
                        if let Some(name) = tool.get("name").and_then(|v| v.as_str()) {
                            register_custom_tool(name, &mut ns_map);
                        }
                        converted.push(ct);
                    }
                }
                "namespace" => {
                    let ns_name = tool.get("name").and_then(|v| v.as_str()).unwrap_or("");
                    if let Some(subtools) = tool.get("tools").and_then(|v| v.as_array()) {
                        for sub in subtools {
                            if let Some(ct) =
                                flatten_namespace_subtool(ns_name, sub, &mut ns_map)
                            {
                                converted.push(ct);
                            }
                        }
                    }
                }
                "web_search" | "web_search_preview" => {
                    converted.push(json!({"type": "web_search"}));
                }
                "mcp" => {
                    converted.push(tool.clone());
                }
                _ => {}
            }
        }
        drop(ns_map);
        if !converted.is_empty() {
            out.insert("tools".to_string(), json!(converted));
        }
    }

    if let Some(tc) = req.get("tool_choice") {
        out.insert("tool_choice".to_string(), normalize_tool_choice_to_chat(tc));
    }

    if let Some(fmt) = req.get("text").and_then(|t| t.get("format")) {
        if let Some(rf) = text_format_to_response_format(fmt) {
            out.insert("response_format".to_string(), rf);
        }
    }

    if let Some(effort) = req
        .get("reasoning")
        .and_then(|r| r.get("effort"))
        .and_then(|v| v.as_str())
    {
        out.insert("reasoning_effort".to_string(), json!(effort));
    }

    for key in ["temperature", "top_p", "user"] {
        if let Some(v) = req.get(key) {
            if !v.is_null() {
                out.insert(key.to_string(), v.clone());
            }
        }
    }

    // Request-scoped fields that are meaningful across providers.
    for key in ["metadata", "store", "service_tier"] {
        if let Some(v) = req.get(key) {
            if !v.is_null() {
                out.insert(key.to_string(), v.clone());
            }
        }
    }

    common::strip_nulls(&mut out);
    Value::Object(out)
}

/// Collect Responses tools from both supported locations.  `additional_tools`
/// is an input item rather than a top-level request field, so a top-level-only
/// converter silently drops these tools and makes goal/control calls impossible.
fn collect_responses_tools(req: &Value) -> Vec<Value> {
    let mut tools = Vec::new();
    if let Some(top_level) = req.get("tools").and_then(|v| v.as_array()) {
        tools.extend(top_level.iter().cloned());
    }
    if let Some(items) = req.get("input").and_then(|v| v.as_array()) {
        for item in items {
            if item.get("type").and_then(|v| v.as_str()) != Some("additional_tools") {
                continue;
            }
            if let Some(additional) = item.get("tools").and_then(|v| v.as_array()) {
                tools.extend(additional.iter().cloned());
            }
        }
    }
    tools
}

fn responses_input_items_to_messages(items: &[Value], messages: &mut Vec<Value>) {
    for item in items {
        let itype = item.get("type").and_then(|v| v.as_str()).unwrap_or("message");
        match itype {
            "reasoning" => {
                // Responses reasoning items are part of the assistant turn. Preserve
                // readable summaries/content so an Anthropic continuation retains the
                // model's prior reasoning context. Encrypted content is provider-bound
                // and cannot be re-emitted to Anthropic, so it is intentionally skipped.
                let mut text = String::new();
                for key in ["content", "summary"] {
                    if let Some(parts) = item.get(key).and_then(|v| v.as_array()) {
                        for part in parts {
                            if let Some(t) = part.get("text").and_then(|v| v.as_str()) {
                                text.push_str(t);
                            }
                        }
                    }
                }
                if !text.is_empty() {
                    if let Some(last) = messages.last_mut() {
                        if last.get("role").and_then(|v| v.as_str()) == Some("assistant") {
                            last["reasoning_content"] = json!(text);
                            continue;
                        }
                    }
                    messages.push(json!({
                        "role": "assistant",
                        "content": Value::Null,
                        "reasoning_content": text
                    }));
                }
            }
            "function_call" | "custom_tool_call" => {
                let call_id = item
                    .get("call_id")
                    .or_else(|| item.get("id"))
                    .and_then(|v| v.as_str())
                    .unwrap_or("");
                let name = item.get("name").and_then(|v| v.as_str()).unwrap_or("");
                let args = if itype == "custom_tool_call" {
                    // Chat/Anthropic only have function arguments.  LiteLLM's
                    // Responses adapter uses the same envelope when forwarding
                    // custom tools, then unwraps `content` on the way back.
                    let input = item
                        .get("input")
                        .or_else(|| item.get("arguments"))
                        .map(|v| v.as_str().map(str::to_string).unwrap_or_else(|| v.to_string()))
                        .unwrap_or_default();
                    serde_json::to_string(&json!({"content": input}))
                        .unwrap_or_else(|_| "{}".to_string())
                } else {
                    item.get("arguments")
                        .or_else(|| item.get("input"))
                        .map(|v| if let Some(s) = v.as_str() { s.to_string() } else { v.to_string() })
                        .unwrap_or_else(|| "{}".to_string())
                };
                let tool_call = json!({
                    "id": call_id,
                    "type": "function",
                    "function": {"name": name, "arguments": args}
                });
                // 合并到上一条 assistant（若相邻）
                if let Some(last) = messages.last_mut() {
                    if last.get("role").and_then(|v| v.as_str()) == Some("assistant") {
                        if last.get("tool_calls").is_none() {
                            last["tool_calls"] = json!([]);
                        }
                        last["tool_calls"].as_array_mut().unwrap().push(tool_call);
                        continue;
                    }
                }
                messages.push(json!({
                    "role": "assistant",
                    "content": Value::Null,
                    "tool_calls": [tool_call]
                }));
            }
            "function_call_output" | "custom_tool_call_output" => {
                let call_id = item.get("call_id").and_then(|v| v.as_str()).unwrap_or("");
                let output = match item.get("output").or_else(|| item.get("content")) {
                    Some(Value::String(s)) => s.clone(),
                    Some(v) => extract_text(v),
                    None => String::new(),
                };
                messages.push(json!({
                    "role": "tool",
                    "tool_call_id": call_id,
                    "content": output
                }));
            }
            "agent_message" => {
                let content = item.get("content").or_else(|| item.get("text"));
                if let Some(content) = content {
                    messages.push(json!({"role": "assistant", "content": extract_text(content)}));
                }
            }
            _ => {
                // message / 带 content
                let role = item.get("role").and_then(|v| v.as_str()).unwrap_or("user");
                let content = match item.get("content") {
                    Some(Value::String(s)) => json!(s),
                    Some(Value::Array(parts)) => responses_content_parts_to_chat(parts),
                    _ => continue,
                };
                messages.push(json!({"role": role, "content": content}));
            }
        }
    }
}

fn responses_content_parts_to_chat(parts: &[Value]) -> Value {
    let mut out: Vec<Value> = Vec::new();
    for part in parts {
        let ptype = part.get("type").and_then(|v| v.as_str()).unwrap_or("");
        match ptype {
            "input_text" | "output_text" | "text" => {
                if let Some(t) = part.get("text").and_then(|v| v.as_str()) {
                    out.push(json!({"type": "text", "text": t}));
                }
            }
            "input_image" => {
                let url = part
                    .get("image_url")
                    .and_then(|v| v.as_str())
                    .or_else(|| part.get("image_url").and_then(|iu| iu.get("url")).and_then(|v| v.as_str()))
                    .unwrap_or("");
                out.push(json!({"type": "image_url", "image_url": {"url": url}}));
            }
            "refusal" => {
                if let Some(t) = part.get("refusal").and_then(|v| v.as_str()) {
                    out.push(json!({"type": "text", "text": t}));
                }
            }
            "input_file" | "input_audio" => {
                // Preserve unsupported multimodal inputs as textual placeholders rather
                // than silently dropping the user's content during cross-provider routing.
                out.push(json!({"type": "text", "text": part.to_string()}));
            }
            _ => {}
        }
    }
    // 若全是文本，合并成单字符串更贴近 Chat 习惯
    if out.iter().all(|p| p.get("type").and_then(|v| v.as_str()) == Some("text")) {
        let joined: String = out
            .iter()
            .filter_map(|p| p.get("text").and_then(|v| v.as_str()))
            .collect();
        return json!(joined);
    }
    json!(out)
}

fn responses_tool_to_chat(tool: &Value) -> Option<Value> {
    let ttype = tool.get("type").and_then(|v| v.as_str()).unwrap_or("function");
    if ttype != "function" {
        return None;
    }
    let name = tool.get("name").and_then(|v| v.as_str())?;
    let mut func = obj();
    func.insert("name".to_string(), json!(name));
    if let Some(desc) = tool.get("description") {
        func.insert("description".to_string(), desc.clone());
    }
    let params = tool.get("parameters").cloned().unwrap_or_else(|| json!({"type": "object"}));
    func.insert("parameters".to_string(), params);
    Some(json!({"type": "function", "function": func}))
}

/// Convert a Responses `custom` (free-form/grammar) tool to the Chat function
/// shape understood by Anthropic and OpenAI-compatible providers.
fn responses_custom_tool_to_chat(tool: &Value) -> Option<Value> {
    if tool.get("type").and_then(|v| v.as_str()) != Some("custom") {
        return None;
    }
    let name = tool.get("name").and_then(|v| v.as_str())?;
    let mut description = tool
        .get("description")
        .and_then(|v| v.as_str())
        .unwrap_or("")
        .to_string();
    if let Some(format) = tool.get("format").and_then(|v| v.as_object()) {
        let syntax = format.get("syntax").and_then(|v| v.as_str()).unwrap_or("");
        let definition = format
            .get("definition")
            .and_then(|v| v.as_str())
            .unwrap_or("");
        if !definition.is_empty() {
            description.push_str(&format!("\n\nFormat:\n```{}\n{}\n```", syntax, definition));
        }
    }
    let content_description = format!("The {} content following the specified format", name);
    let parameters = json!({
        "type": "object",
        "properties": {"content": {"type": "string", "description": content_description}},
        "required": ["content"]
    });
    let mut function = json!({"name": name, "parameters": parameters});
    if !description.is_empty() {
        function["description"] = json!(description);
    }
    Some(json!({"type": "function", "function": function}))
}

// ==================== Anthropic → Chat ====================

/// Anthropic 请求 → Chat 请求。
pub fn anthropic_to_openai(req: &Value, model: &str) -> Value {
    let mut out = obj();
    out.insert("model".to_string(), json!(model));

    let mut messages: Vec<Value> = Vec::new();

    // system → 前置 system 消息
    if let Some(system) = req.get("system") {
        match system {
            Value::String(s) => {
                if !s.is_empty() {
                    messages.push(json!({"role": "system", "content": s}));
                }
            }
            Value::Array(_) => {
                if let Some(content) = anthropic_system_to_chat_content(system) {
                    messages.push(json!({"role": "system", "content": content}));
                }
            }
            _ => {}
        }
    }

    // messages
    if let Some(arr) = req.get("messages").and_then(|v| v.as_array()) {
        for msg in arr {
            anthropic_message_to_chat(msg, &mut messages);
        }
    }

    out.insert("messages".to_string(), json!(messages));

    // max_tokens
    if let Some(mt) = req.get("max_tokens").and_then(|v| v.as_i64()) {
        out.insert("max_tokens".to_string(), json!(mt));
    }

    // 同名透传
    for key in ["temperature", "top_p", "top_k"] {
        if let Some(v) = req.get(key) {
            if !v.is_null() {
                out.insert(key.to_string(), v.clone());
            }
        }
    }

    if let Some(metadata) = req.get("metadata") {
        if !metadata.is_null() {
            out.insert("metadata".to_string(), metadata.clone());
        }
    }

    // stop_sequences → stop
    if let Some(stop) = req.get("stop_sequences") {
        out.insert("stop".to_string(), stop.clone());
    }

    // tools：Anthropic → Chat function tool
    if let Some(tools) = req.get("tools").and_then(|v| v.as_array()) {
        let converted: Vec<Value> = tools.iter().filter_map(anthropic_tool_to_chat).collect();
        if !converted.is_empty() {
            out.insert("tools".to_string(), json!(converted));
        }
    }

    // tool_choice
    if let Some(tc) = req.get("tool_choice") {
        out.insert("tool_choice".to_string(), anthropic_tool_choice_to_chat(tc));
    }

    common::strip_nulls(&mut out);
    Value::Object(out)
}

/// Anthropic system 数组 → Chat content。
/// 无 cache_control 时退化为纯字符串；有则用数组形式的 content part 承载 cache_control。
fn anthropic_system_to_chat_content(system: &Value) -> Option<Value> {
    let arr = system.as_array()?;
    let mut parts: Vec<Value> = Vec::new();
    let mut has_cache_control = false;
    for block in arr {
        if block.get("type").and_then(|v| v.as_str()) != Some("text") {
            continue;
        }
        let text = block.get("text").and_then(|v| v.as_str()).unwrap_or("");
        let mut part = json!({"type": "text", "text": text});
        if block.get("cache_control").is_some() {
            has_cache_control = true;
            attach_cache_control(&mut part, block);
        }
        parts.push(part);
    }
    if parts.is_empty() {
        return None;
    }
    if has_cache_control {
        Some(json!(parts))
    } else {
        let joined: String = parts
            .iter()
            .filter_map(|p| p.get("text").and_then(|v| v.as_str()))
            .collect();
        if joined.is_empty() {
            None
        } else {
            Some(json!(joined))
        }
    }
}

fn anthropic_message_to_chat(msg: &Value, messages: &mut Vec<Value>) {
    let role = msg.get("role").and_then(|v| v.as_str()).unwrap_or("user");
    let content = msg.get("content");

    // string content
    if let Some(Value::String(s)) = content {
        messages.push(json!({"role": role, "content": s}));
        return;
    }

    let blocks = match content.and_then(|v| v.as_array()) {
        Some(b) => b,
        None => {
            messages.push(json!({"role": role, "content": ""}));
            return;
        }
    };

    let mut text_parts: Vec<Value> = Vec::new();
    let mut tool_calls: Vec<Value> = Vec::new();
    let mut reasoning = String::new();
    let mut tool_results: Vec<Value> = Vec::new();

    for block in blocks {
        let btype = block.get("type").and_then(|v| v.as_str()).unwrap_or("");
        match btype {
            "text" => {
                if let Some(t) = block.get("text").and_then(|v| v.as_str()) {
                    let mut part = json!({"type": "text", "text": t});
                    attach_cache_control(&mut part, block);
                    text_parts.push(part);
                }
            }
            "image" => {
                if let Some(source) = block.get("source") {
                    let mut part = anthropic_image_to_openai(source);
                    attach_cache_control(&mut part, block);
                    text_parts.push(part);
                }
            }
            "tool_use" => {
                let id = block.get("id").and_then(|v| v.as_str()).unwrap_or("");
                let name = block.get("name").and_then(|v| v.as_str()).unwrap_or("");
                let input = block.get("input").cloned().unwrap_or_else(|| json!({}));
                let args = serde_json::to_string(&input).unwrap_or_else(|_| "{}".to_string());
                tool_calls.push(json!({
                    "id": id,
                    "type": "function",
                    "function": {"name": name, "arguments": args}
                }));
            }
            "tool_result" => {
                let tool_use_id = block.get("tool_use_id").and_then(|v| v.as_str()).unwrap_or("");
                let rc = block.get("content");
                let text = match rc {
                    Some(Value::String(s)) => s.clone(),
                    Some(v) => extract_text(v),
                    None => String::new(),
                };
                tool_results.push(json!({
                    "role": "tool",
                    "tool_call_id": tool_use_id,
                    "content": text
                }));
            }
            "thinking" => {
                if let Some(t) = block.get("thinking").and_then(|v| v.as_str()) {
                    reasoning.push_str(t);
                }
            }
            _ => {}
        }
    }

    // tool_result 作为独立 tool 消息（Chat 侧）
    if !tool_results.is_empty() {
        for tr in tool_results {
            messages.push(tr);
        }
    }

    // 是否有正文/工具调用
    let has_body = !text_parts.is_empty() || !tool_calls.is_empty() || !reasoning.is_empty();
    if has_body {
        let mut m = obj();
        m.insert("role".to_string(), json!(role));
        // content
        let content_val = if text_parts.is_empty() {
            Value::Null
        } else if text_parts.len() == 1
            && text_parts[0].get("type").and_then(|v| v.as_str()) == Some("text")
            && text_parts[0].get("cache_control").is_none()
        {
            json!(text_parts[0]["text"].as_str().unwrap_or(""))
        } else {
            json!(text_parts)
        };
        m.insert("content".to_string(), content_val);
        if !reasoning.is_empty() {
            m.insert("reasoning_content".to_string(), json!(reasoning));
        }
        if !tool_calls.is_empty() {
            m.insert("tool_calls".to_string(), json!(tool_calls));
        }
        messages.push(Value::Object(m));
    }
}

fn anthropic_tool_to_chat(tool: &Value) -> Option<Value> {
    let name = tool.get("name").and_then(|v| v.as_str())?;
    let mut func = obj();
    func.insert("name".to_string(), json!(name));
    if let Some(desc) = tool.get("description") {
        func.insert("description".to_string(), desc.clone());
    }
    let params = tool
        .get("input_schema")
        .cloned()
        .unwrap_or_else(|| json!({"type": "object"}));
    func.insert("parameters".to_string(), params);
    let mut tool_out = json!({"type": "function", "function": func});
    attach_cache_control(&mut tool_out, tool);
    Some(tool_out)
}

fn anthropic_tool_choice_to_chat(tc: &Value) -> Value {
    let ttype = tc.get("type").and_then(|v| v.as_str()).unwrap_or("auto");
    match ttype {
        "auto" => json!("auto"),
        "any" => json!("required"),
        "none" => json!("none"),
        "tool" => {
            let name = tc.get("name").and_then(|v| v.as_str()).unwrap_or("");
            json!({"type": "function", "function": {"name": name}})
        }
        _ => json!("auto"),
    }
}

// ==================== Chat → Anthropic ====================

/// Chat 请求 → Anthropic 请求。
pub fn openai_to_anthropic(req: &Value, model: &str) -> Value {
    let mut out = obj();
    out.insert("model".to_string(), json!(model));

    let mut system_texts: Vec<Value> = Vec::new();
    let mut messages: Vec<Value> = Vec::new();

    if let Some(arr) = req.get("messages").and_then(|v| v.as_array()) {
        for msg in arr {
            let role = msg.get("role").and_then(|v| v.as_str()).unwrap_or("user");
            match role {
                "system" => {
                    if let Some(c) = msg.get("content") {
                        chat_system_content_to_anthropic_texts(c, &mut system_texts);
                    }
                }
                "tool" => {
                    // tool 消息 → tool_result，合并进 user 消息
                    let tool_call_id = msg.get("tool_call_id").and_then(|v| v.as_str()).unwrap_or("");
                    let content = msg.get("content").map(extract_text).unwrap_or_default();
                    let block = json!({
                        "type": "tool_result",
                        "tool_use_id": tool_call_id,
                        "content": content
                    });
                    push_anthropic_block(&mut messages, "user", block);
                }
                "assistant" => {
                    let mut blocks: Vec<Value> = Vec::new();
                    // reasoning_content → thinking（放最前）
                    if let Some(rc) = msg.get("reasoning_content").and_then(|v| v.as_str()) {
                        if !rc.is_empty() {
                            blocks.push(json!({"type": "thinking", "thinking": rc}));
                        }
                    }
                    // content 文本
                    if let Some(c) = msg.get("content") {
                        chat_content_to_anthropic_blocks(c, &mut blocks);
                    }
                    // tool_calls → tool_use
                    if let Some(tcs) = msg.get("tool_calls").and_then(|v| v.as_array()) {
                        for tc in tcs {
                            let id = tc.get("id").and_then(|v| v.as_str()).unwrap_or("");
                            let func = tc.get("function");
                            let name = func
                                .and_then(|f| f.get("name"))
                                .and_then(|v| v.as_str())
                                .unwrap_or("");
                            let args_str = func
                                .and_then(|f| f.get("arguments"))
                                .and_then(|v| v.as_str())
                                .unwrap_or("{}");
                            let input: Value =
                                serde_json::from_str(args_str).unwrap_or_else(|_| json!({}));
                            blocks.push(json!({
                                "type": "tool_use",
                                "id": id,
                                "name": name,
                                "input": input
                            }));
                        }
                    }
                    if blocks.is_empty() {
                        blocks.push(json!({"type": "text", "text": ""}));
                    }
                    messages.push(json!({"role": "assistant", "content": collapse_blocks(blocks)}));
                }
                _ => {
                    // user
                    let mut blocks: Vec<Value> = Vec::new();
                    if let Some(c) = msg.get("content") {
                        chat_content_to_anthropic_blocks(c, &mut blocks);
                    }
                    if blocks.is_empty() {
                        blocks.push(json!({"type": "text", "text": ""}));
                    }
                    // 若已有末尾 user（tool_result 合并场景），追加
                    if let Some(last) = messages.last_mut() {
                        if last.get("role").and_then(|v| v.as_str()) == Some("user") {
                            if let Some(existing) = last.get_mut("content").and_then(|c| c.as_array_mut()) {
                                existing.extend(blocks);
                                continue;
                            }
                        }
                    }
                    messages.push(json!({"role": "user", "content": collapse_blocks(blocks)}));
                }
            }
        }
    }

    if !system_texts.is_empty() {
        out.insert("system".to_string(), json!(system_texts));
    }
    out.insert("messages".to_string(), json!(messages));

    // max_tokens 必填，兜底 4096
    let max_tokens = req
        .get("max_tokens")
        .or_else(|| req.get("max_completion_tokens"))
        .and_then(|v| v.as_i64())
        .unwrap_or(DEFAULT_ANTHROPIC_MAX_TOKENS);
    out.insert("max_tokens".to_string(), json!(max_tokens));

    // 同名透传
    for key in ["temperature", "top_p", "top_k"] {
        if let Some(v) = req.get(key) {
            if !v.is_null() {
                out.insert(key.to_string(), v.clone());
            }
        }
    }

    // stop → stop_sequences
    if let Some(stop) = req.get("stop") {
        let seqs = match stop {
            Value::String(s) => json!([s]),
            Value::Array(_) => stop.clone(),
            _ => Value::Null,
        };
        if !seqs.is_null() {
            out.insert("stop_sequences".to_string(), seqs);
        }
    }

    // tools：Chat function tool / web_search → Anthropic
    if let Some(tools) = req.get("tools").and_then(|v| v.as_array()) {
        let mut converted: Vec<Value> = Vec::new();
        for tool in tools {
            let ttype = tool.get("type").and_then(|v| v.as_str()).unwrap_or("function");
            match ttype {
                "function" => {
                    if let Some(at) = chat_tool_to_anthropic(tool) {
                        converted.push(at);
                    }
                }
                "web_search" => {
                    converted.push(json!({
                        "type": "web_search_20250305",
                        "name": "web_search",
                        "max_uses": 5
                    }));
                }
                _ => {}
            }
        }
        if !converted.is_empty() {
            out.insert("tools".to_string(), json!(converted));
        }
    }

    // tool_choice
    if let Some(tc) = req.get("tool_choice") {
        out.insert("tool_choice".to_string(), chat_tool_choice_to_anthropic(tc));
    }

    // reasoning_effort → thinking
    if let Some(effort) = req.get("reasoning_effort").and_then(|v| v.as_str()) {
        out.insert(
            "thinking".to_string(),
            json!({"type": "enabled", "budget_tokens": effort_to_budget(effort)}),
        );
    }

    common::strip_nulls(&mut out);
    Value::Object(out)
}

fn push_anthropic_block(messages: &mut Vec<Value>, role: &str, block: Value) {
    if let Some(last) = messages.last_mut() {
        if last.get("role").and_then(|v| v.as_str()) == Some(role) {
            if let Some(arr) = last.get_mut("content").and_then(|c| c.as_array_mut()) {
                arr.push(block);
                return;
            }
        }
    }
    messages.push(json!({"role": role, "content": [block]}));
}

/// 若 blocks 恰为单个 text block，折叠为纯字符串 content（Anthropic 两种都接受）。
/// 但若该块带 cache_control，则保留数组形式，避免丢失缓存断点。
fn collapse_blocks(blocks: Vec<Value>) -> Value {
    if blocks.len() == 1
        && blocks[0].get("type").and_then(|v| v.as_str()) == Some("text")
        && blocks[0].get("cache_control").is_none()
    {
        return json!(blocks[0]["text"].as_str().unwrap_or(""));
    }
    json!(blocks)
}

fn chat_content_to_anthropic_blocks(content: &Value, blocks: &mut Vec<Value>) {
    match content {
        Value::String(s) => {
            blocks.push(json!({"type": "text", "text": s}));
        }
        Value::Array(parts) => {
            for part in parts {
                let ptype = part.get("type").and_then(|v| v.as_str()).unwrap_or("");
                match ptype {
                    "text" => {
                        if let Some(t) = part.get("text").and_then(|v| v.as_str()) {
                            let mut block = json!({"type": "text", "text": t});
                            attach_cache_control(&mut block, part);
                            blocks.push(block);
                        }
                    }
                    "image_url" => {
                        if let Some(iu) = part.get("image_url") {
                            let mut block = openai_image_to_anthropic(iu);
                            attach_cache_control(&mut block, part);
                            blocks.push(block);
                        }
                    }
                    _ => {}
                }
            }
        }
        _ => {}
    }
}

/// 若源对象带 cache_control，则透传到目标 Anthropic block/tool 上。
fn attach_cache_control(target: &mut Value, source: &Value) {
    if let Some(cc) = source.get("cache_control") {
        if let Some(m) = target.as_object_mut() {
            m.insert("cache_control".to_string(), cc.clone());
        }
    }
}

/// Chat system content（字符串或数组）→ Anthropic system text 块数组，保留 cache_control。
fn chat_system_content_to_anthropic_texts(content: &Value, system_texts: &mut Vec<Value>) {
    match content {
        Value::String(s) => {
            if !s.is_empty() {
                system_texts.push(json!({"type": "text", "text": s}));
            }
        }
        Value::Array(parts) => {
            for part in parts {
                let ptype = part.get("type").and_then(|v| v.as_str()).unwrap_or("");
                if ptype != "text" {
                    continue;
                }
                if let Some(t) = part.get("text").and_then(|v| v.as_str()) {
                    let mut block = json!({"type": "text", "text": t});
                    attach_cache_control(&mut block, part);
                    system_texts.push(block);
                }
            }
        }
        _ => {}
    }
}

fn chat_tool_to_anthropic(tool: &Value) -> Option<Value> {
    let func = tool.get("function")?;
    let name = func.get("name").and_then(|v| v.as_str())?;
    let mut schema = func
        .get("parameters")
        .cloned()
        .unwrap_or_else(|| json!({"type": "object"}));
    // input_schema 强制补 type:object
    if let Some(m) = schema.as_object_mut() {
        if !m.contains_key("type") {
            m.insert("type".to_string(), json!("object"));
        }
    } else {
        schema = json!({"type": "object"});
    }
    let mut out = obj();
    out.insert("name".to_string(), json!(name));
    if let Some(desc) = func.get("description") {
        out.insert("description".to_string(), desc.clone());
    }
    out.insert("input_schema".to_string(), schema);
    let mut tool_out = Value::Object(out);
    attach_cache_control(&mut tool_out, tool);
    Some(tool_out)
}

fn chat_tool_choice_to_anthropic(tc: &Value) -> Value {
    match tc {
        Value::String(s) => match s.as_str() {
            "auto" => json!({"type": "auto"}),
            "required" => json!({"type": "any"}),
            "none" => json!({"type": "none"}),
            _ => json!({"type": "auto"}),
        },
        Value::Object(_) => {
            let name = tc
                .get("function")
                .and_then(|f| f.get("name"))
                .and_then(|v| v.as_str())
                .unwrap_or("");
            json!({"type": "tool", "name": name})
        }
        _ => json!({"type": "auto"}),
    }
}

// ==================== Chat → Responses ====================

/// Chat 请求 → Responses 请求。
pub fn openai_to_responses(req: &Value, model: &str) -> Value {
    let mut out = obj();
    out.insert("model".to_string(), json!(model));

    let mut instructions = String::new();
    let mut input_items: Vec<Value> = Vec::new();

    if let Some(arr) = req.get("messages").and_then(|v| v.as_array()) {
        for msg in arr {
            let role = msg.get("role").and_then(|v| v.as_str()).unwrap_or("user");
            match role {
                "system" => {
                    let text = msg.get("content").map(extract_text).unwrap_or_default();
                    if !instructions.is_empty() {
                        instructions.push(' ');
                    }
                    instructions.push_str(&text);
                }
                "tool" => {
                    let call_id = msg.get("tool_call_id").and_then(|v| v.as_str()).unwrap_or("");
                    let output = msg.get("content").map(extract_text).unwrap_or_default();
                    input_items.push(json!({
                        "type": "function_call_output",
                        "call_id": call_id,
                        "output": output
                    }));
                }
                "assistant" => {
                    if let Some(rc) = msg.get("reasoning_content").and_then(|v| v.as_str()) {
                        if !rc.is_empty() {
                            input_items.push(json!({
                                "type": "reasoning",
                                "summary": [{"type": "summary_text", "text": rc}]
                            }));
                        }
                    }
                    if let Some(tcs) = msg.get("tool_calls").and_then(|v| v.as_array()) {
                        for tc in tcs {
                            let call_id = tc.get("id").and_then(|v| v.as_str()).unwrap_or("");
                            let func = tc.get("function");
                            let name = func
                                .and_then(|f| f.get("name"))
                                .and_then(|v| v.as_str())
                                .unwrap_or("");
                            let args = func
                                .and_then(|f| f.get("arguments"))
                                .and_then(|v| v.as_str())
                                .unwrap_or("{}");
                            input_items.push(json!({
                                "type": "function_call",
                                "call_id": call_id,
                                "name": name,
                                "arguments": args
                            }));
                        }
                    }
                    let text = msg.get("content").map(extract_text).unwrap_or_default();
                    if !text.is_empty() {
                        input_items.push(json!({
                            "type": "message",
                            "role": "assistant",
                            "content": [{"type": "output_text", "text": text}]
                        }));
                    }
                }
                _ => {
                    // user
                    let content = chat_content_to_responses_parts(
                        msg.get("content").unwrap_or(&Value::Null),
                        "input_text",
                    );
                    input_items.push(json!({
                        "type": "message",
                        "role": "user",
                        "content": content
                    }));
                }
            }
        }
    }

    if !instructions.is_empty() {
        out.insert("instructions".to_string(), json!(instructions));
    }

    // Responses 拒绝空 input：若无 items 但有 instructions，把 instructions 作为 system input
    if input_items.is_empty() {
        if !instructions.is_empty() {
            input_items.push(json!({
                "type": "message",
                "role": "system",
                "content": [{"type": "input_text", "text": instructions}]
            }));
        }
    }
    out.insert("input".to_string(), json!(input_items));

    // max_tokens → max_output_tokens
    if let Some(mt) = req
        .get("max_tokens")
        .or_else(|| req.get("max_completion_tokens"))
        .and_then(|v| v.as_i64())
    {
        out.insert("max_output_tokens".to_string(), json!(mt));
    }

    // tools：Chat function tool → Responses function tool
    if let Some(tools) = req.get("tools").and_then(|v| v.as_array()) {
        let converted: Vec<Value> = tools.iter().filter_map(chat_tool_to_responses).collect();
        if !converted.is_empty() {
            out.insert("tools".to_string(), json!(converted));
        }
    }

    // tool_choice
    if let Some(tc) = req.get("tool_choice") {
        out.insert("tool_choice".to_string(), normalize_tool_choice_to_responses(tc));
    }

    // response_format → text.format
    if let Some(rf) = req.get("response_format") {
        if let Some(fmt) = response_format_to_text_format(rf) {
            out.insert("text".to_string(), json!({"format": fmt}));
        }
    }

    // reasoning_effort → reasoning
    if let Some(effort) = req.get("reasoning_effort").and_then(|v| v.as_str()) {
        out.insert("reasoning".to_string(), json!({"effort": effort}));
    }

    // 同名透传
    for key in ["temperature", "top_p", "user"] {
        if let Some(v) = req.get(key) {
            if !v.is_null() {
                out.insert(key.to_string(), v.clone());
            }
        }
    }

    for key in ["metadata", "store", "service_tier"] {
        if let Some(v) = req.get(key) {
            if !v.is_null() {
                out.insert(key.to_string(), v.clone());
            }
        }
    }

    common::strip_nulls(&mut out);
    Value::Object(out)
}

fn chat_content_to_responses_parts(content: &Value, text_type: &str) -> Value {
    match content {
        Value::String(s) => json!([{"type": text_type, "text": s}]),
        Value::Array(parts) => {
            let mut out: Vec<Value> = Vec::new();
            for part in parts {
                let ptype = part.get("type").and_then(|v| v.as_str()).unwrap_or("");
                match ptype {
                    "text" => {
                        if let Some(t) = part.get("text").and_then(|v| v.as_str()) {
                            out.push(json!({"type": text_type, "text": t}));
                        }
                    }
                    "image_url" => {
                        let url = part
                            .get("image_url")
                            .and_then(|iu| iu.get("url"))
                            .and_then(|v| v.as_str())
                            .unwrap_or("");
                        out.push(json!({"type": "input_image", "image_url": url}));
                    }
                    _ => {}
                }
            }
            json!(out)
        }
        _ => json!([{"type": text_type, "text": ""}]),
    }
}

fn chat_tool_to_responses(tool: &Value) -> Option<Value> {
    let func = tool.get("function")?;
    let name = func.get("name").and_then(|v| v.as_str())?;
    let mut out = obj();
    out.insert("type".to_string(), json!("function"));
    out.insert("name".to_string(), json!(name));
    if let Some(desc) = func.get("description") {
        out.insert("description".to_string(), desc.clone());
    }
    let mut params = func
        .get("parameters")
        .cloned()
        .unwrap_or_else(|| json!({"type": "object"}));
    if let Some(m) = params.as_object_mut() {
        if !m.contains_key("type") {
            m.insert("type".to_string(), json!("object"));
        }
    }
    out.insert("parameters".to_string(), params);
    Some(Value::Object(out))
}

// ==================== Chat → Chat（finalize）====================

/// Chat → Chat：仅覆盖 model 字段。
pub fn finalize_openai(chat: Value, model: &str) -> Value {
    let mut v = chat;
    if let Some(m) = v.as_object_mut() {
        m.insert("model".to_string(), json!(model));
    }
    v
}

// ==================== tool_choice / response_format 归一化 ====================

fn normalize_tool_choice_to_chat(tc: &Value) -> Value {
    match tc {
        Value::String(_) => tc.clone(),
        Value::Object(_) => {
            let ttype = tc.get("type").and_then(|v| v.as_str()).unwrap_or("");
            match ttype {
                "auto" => json!("auto"),
                "none" => json!("none"),
                "required" | "any" => json!("required"),
                "function" => {
                    let name = tc.get("name").and_then(|v| v.as_str()).unwrap_or("");
                    json!({"type": "function", "function": {"name": name}})
                }
                _ => tc.clone(),
            }
        }
        _ => json!("auto"),
    }
}

fn normalize_tool_choice_to_responses(tc: &Value) -> Value {
    match tc {
        Value::String(s) => json!({"type": s}),
        Value::Object(_) => {
            if let Some(name) = tc
                .get("function")
                .and_then(|f| f.get("name"))
                .and_then(|v| v.as_str())
            {
                json!({"type": "function", "name": name})
            } else {
                tc.clone()
            }
        }
        _ => json!("auto"),
    }
}

fn text_format_to_response_format(fmt: &Value) -> Option<Value> {
    let ftype = fmt.get("type").and_then(|v| v.as_str())?;
    match ftype {
        "json_schema" => {
            let name = fmt.get("name").and_then(|v| v.as_str()).unwrap_or("response_schema");
            let schema = fmt.get("schema").cloned().unwrap_or_else(|| json!({}));
            let strict = fmt.get("strict").and_then(|v| v.as_bool()).unwrap_or(false);
            Some(json!({
                "type": "json_schema",
                "json_schema": {"name": name, "schema": schema, "strict": strict}
            }))
        }
        "json_object" => Some(json!({"type": "json_object"})),
        _ => None,
    }
}

fn response_format_to_text_format(rf: &Value) -> Option<Value> {
    let rtype = rf.get("type").and_then(|v| v.as_str())?;
    match rtype {
        "json_schema" => {
            let js = rf.get("json_schema");
            let name = js
                .and_then(|j| j.get("name"))
                .and_then(|v| v.as_str())
                .unwrap_or("response_schema");
            let schema = js
                .and_then(|j| j.get("schema"))
                .cloned()
                .unwrap_or_else(|| json!({}));
            let strict = js
                .and_then(|j| j.get("strict"))
                .and_then(|v| v.as_bool())
                .unwrap_or(false);
            Some(json!({
                "type": "json_schema",
                "name": name,
                "schema": schema,
                "strict": strict
            }))
        }
        "json_object" => Some(json!({"type": "json_object"})),
        _ => None,
    }
}

// ==================== Anthropic prompt caching 断点自动注入 ====================

fn ephemeral() -> Value {
    json!({"type": "ephemeral"})
}

/// 递归检查 Value 中是否出现 cache_control 键。
fn value_has_cache_control(v: &Value) -> bool {
    match v {
        Value::Object(m) => {
            if m.contains_key("cache_control") {
                return true;
            }
            m.values().any(value_has_cache_control)
        }
        Value::Array(arr) => arr.iter().any(value_has_cache_control),
        _ => false,
    }
}

/// 扫描 Anthropic 请求体的 system / messages / tools 是否已含任何 cache_control。
fn body_has_cache_control(body: &Value) -> bool {
    for key in ["system", "messages", "tools"] {
        if let Some(v) = body.get(key) {
            if value_has_cache_control(v) {
                return true;
            }
        }
    }
    false
}

/// 把消息最后一个 content 块打上 cache_control（content 为字符串时先转成单元素 text 数组）。
/// 成功打点返回 true。
fn set_cache_control_on_last_block_of_message(msg: &mut Value) -> bool {
    let content = match msg.get_mut("content") {
        Some(c) => c,
        None => return false,
    };
    match content {
        Value::String(s) => {
            let text = s.clone();
            *content = json!([{"type": "text", "text": text, "cache_control": ephemeral()}]);
            true
        }
        Value::Array(arr) => {
            if let Some(last) = arr.last_mut() {
                if let Some(m) = last.as_object_mut() {
                    m.insert("cache_control".to_string(), ephemeral());
                    return true;
                }
            }
            false
        }
        _ => false,
    }
}

/// 对已是 Anthropic 格式的请求体自动注入 prompt caching 断点。
/// 幂等：请求体中已存在任何 cache_control 时不做任何改动。
/// 注入策略（最多 4 个断点）：system 末块、tools 末个、最后一条消息末块、
/// 消息数≥2 时倒数第二条 user 消息末块。
pub fn inject_anthropic_cache_breakpoints(body: &mut Value) {
    if !body.is_object() || body_has_cache_control(body) {
        return;
    }

    let mut budget: u8 = 4;

    // system：末块
    if budget > 0 {
        if let Some(system) = body.get_mut("system") {
            match system {
                Value::String(s) => {
                    if !s.is_empty() {
                        let text = s.clone();
                        *system =
                            json!([{"type": "text", "text": text, "cache_control": ephemeral()}]);
                        budget -= 1;
                    }
                }
                Value::Array(arr) => {
                    if let Some(last) = arr
                        .iter_mut()
                        .rev()
                        .find(|b| b.get("type").and_then(|v| v.as_str()) == Some("text"))
                    {
                        if let Some(m) = last.as_object_mut() {
                            m.insert("cache_control".to_string(), ephemeral());
                            budget -= 1;
                        }
                    }
                }
                _ => {}
            }
        }
    }

    // tools：末个
    if budget > 0 {
        if let Some(tools) = body.get_mut("tools").and_then(|v| v.as_array_mut()) {
            if let Some(last) = tools.last_mut() {
                if let Some(m) = last.as_object_mut() {
                    m.insert("cache_control".to_string(), ephemeral());
                    budget -= 1;
                }
            }
        }
    }

    // messages：最后一条末块；消息数≥2 时倒数第二条 user 消息末块
    if budget > 0 {
        if let Some(messages) = body.get_mut("messages").and_then(|v| v.as_array_mut()) {
            let len = messages.len();
            if len >= 2 {
                if let Some(idx) = messages
                    .iter()
                    .take(len - 1)
                    .enumerate()
                    .filter(|(_, m)| m.get("role").and_then(|v| v.as_str()) == Some("user"))
                    .map(|(i, _)| i)
                    .last()
                {
                    if budget > 0 && set_cache_control_on_last_block_of_message(&mut messages[idx]) {
                        budget -= 1;
                    }
                }
            }
            if budget > 0 {
                if let Some(last) = messages.last_mut() {
                    if set_cache_control_on_last_block_of_message(last) {
                        budget -= 1;
                    }
                }
            }
        }
    }

    let _ = budget;
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn responses_reasoning_and_custom_tools_are_preserved() {
        let req = json!({
            "input": [
                {"type":"message", "role":"assistant", "content":[{"type":"output_text","text":"done"}]},
                {"type":"reasoning", "summary":[{"type":"summary_text","text":"think"}]},
                {"type":"custom_tool_call", "call_id":"c1", "name":"run", "input":{"x":1}},
                {"type":"custom_tool_call_output", "call_id":"c1", "output":"ok"}
            ]
        });
        let chat = responses_to_openai(&req, "m");
        let messages = chat["messages"].as_array().unwrap();
        assert!(messages.iter().any(|m| m.get("reasoning_content").is_some()));
        assert!(messages.iter().any(|m| m.get("tool_calls").is_some()));
        assert!(messages.iter().any(|m| m["role"] == "tool"));
    }

    #[test]
    fn chat_reasoning_is_emitted_as_responses_item() {
        let req = json!({"messages":[{"role":"assistant","reasoning_content":"think","content":"done"}]});
        let responses = openai_to_responses(&req, "m");
        assert_eq!(responses["input"][0]["type"], "reasoning");
    }

    #[test]
    fn continuation_fields_are_rejected_for_cross_format_conversion() {
        let req = json!({"previous_response_id":"resp_1", "input":"next"});
        assert!(validate_responses_cross_format(&req).is_err());
        let encrypted = json!({"input":[{"type":"reasoning","encrypted_content":"secret","summary":[]}]});
        assert!(validate_responses_cross_format(&encrypted).is_err());
        let include = json!({"include":["reasoning.encrypted_content"],"input":"next"});
        assert!(validate_responses_cross_format(&include).is_ok());
    }
}
