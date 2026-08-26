//! 响应体转换：provider 响应 ↔ Chat（canonical）↔ 入口格式。
//! 所有函数签名统一 `(&Value, &str) -> Value`，返回新 Value。

use serde_json::{json, Map, Value};

use super::common::{
    anthropic_stop_to_finish, anthropic_usage_to_chat, chat_usage_to_anthropic,
    chat_usage_to_responses, extract_text, finish_reason_to_status, finish_to_anthropic_stop,
    gen_id, new_ns_reverse_map, responses_usage_to_chat, status_to_finish_reason,
    unwrap_custom_tool_arguments, CUSTOM_TOOL_NAMESPACE_MARKER, NsReverseMap,
};

fn obj() -> Map<String, Value> {
    Map::new()
}

// ==================== → Responses（自动判别输入）====================

/// 转换为 Responses 响应（向后兼容，不做 namespace 还原）。
pub fn to_responses(resp: &Value, model: &str) -> Value {
    let dummy = new_ns_reverse_map();
    to_responses_with_ns(resp, model, &dummy)
}

/// 转换为 Responses 响应，查 ns_reverse 还原 namespace 结构。
pub fn to_responses_with_ns(resp: &Value, model: &str, ns_reverse: &NsReverseMap) -> Value {
    if resp.get("choices").is_some() {
        chat_resp_to_responses_with_ns(resp, model, ns_reverse)
    } else if resp.get("content").is_some() {
        let chat = anthropic_to_openai_response(resp, model);
        chat_resp_to_responses_with_ns(&chat, model, ns_reverse)
    } else {
        chat_resp_to_responses_with_ns(resp, model, ns_reverse)
    }
}

fn chat_resp_to_responses_with_ns(
    resp: &Value,
    model: &str,
    ns_reverse: &NsReverseMap,
) -> Value {
    let choice = resp
        .get("choices")
        .and_then(|c| c.as_array())
        .and_then(|a| a.first());
    let message = choice.and_then(|c| c.get("message"));
    let finish_reason = choice
        .and_then(|c| c.get("finish_reason"))
        .and_then(|v| v.as_str());

    let id = resp
        .get("id")
        .and_then(|v| v.as_str())
        .map(|s| s.to_string())
        .unwrap_or_else(|| gen_id("resp_"));
    let msg_id = gen_id("msg_");

    let status = finish_reason_to_status(finish_reason);

    let mut output: Vec<Value> = Vec::new();

    let mut content_parts: Vec<Value> = Vec::new();

    if let Some(msg) = message {
        if let Some(rc) = msg.get("reasoning_content").and_then(|v| v.as_str()) {
            if !rc.is_empty() {
                content_parts.push(json!({
                    "type": "output_text",
                    "text": rc,
                    "annotations": [{"type": "reasoning"}]
                }));
            }
        }
        let text = msg.get("content").map(extract_text).unwrap_or_default();
        if !text.is_empty() {
            content_parts.push(json!({
                "type": "output_text",
                "text": text,
                "annotations": []
            }));
        }
    }

    if !content_parts.is_empty() {
        output.push(json!({
            "type": "message",
            "id": msg_id,
            "status": status,
            "role": "assistant",
            "content": content_parts
        }));
    }

    // tool_calls → function_call items，查 ns_reverse 还原 namespace
    let ns_map = ns_reverse.read();
    if let Some(tcs) = message.and_then(|m| m.get("tool_calls")).and_then(|v| v.as_array()) {
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
            let mut item = json!({
                "type": "function_call",
                "id": gen_id("fc_"),
                "call_id": call_id,
                "name": name,
                "arguments": args,
                "status": "completed"
            });
            if let Some((ns, sub)) = ns_map.get(name) {
                if ns == CUSTOM_TOOL_NAMESPACE_MARKER {
                    item["type"] = json!("custom_tool_call");
                    item["id"] = json!(call_id);
                    item["input"] = json!(unwrap_custom_tool_arguments(args));
                    item.as_object_mut().unwrap().remove("arguments");
                } else {
                    item["name"] = json!(sub);
                    item["namespace"] = json!(ns);
                }
            }
            output.push(item);
        }
    }
    drop(ns_map);

    let mut out = obj();
    out.insert("id".to_string(), json!(id));
    out.insert("object".to_string(), json!("response"));
    out.insert("created_at".to_string(), json!(0));
    out.insert("model".to_string(), json!(model));
    out.insert("status".to_string(), json!(status));
    out.insert("output".to_string(), json!(output));
    if let Some(usage) = resp.get("usage") {
        out.insert("usage".to_string(), chat_usage_to_responses(usage));
    }
    Value::Object(out)
}

// ==================== Anthropic 响应 → Chat 响应 ====================

/// Anthropic 响应 → Chat 响应。
pub fn anthropic_to_openai_response(resp: &Value, model: &str) -> Value {
    let id = resp
        .get("id")
        .and_then(|v| v.as_str())
        .map(|s| s.to_string())
        .unwrap_or_else(|| gen_id("chatcmpl-"));

    let mut text_content = String::new();
    let mut reasoning = String::new();
    let mut tool_calls: Vec<Value> = Vec::new();

    if let Some(blocks) = resp.get("content").and_then(|v| v.as_array()) {
        for block in blocks {
            let btype = block.get("type").and_then(|v| v.as_str()).unwrap_or("");
            match btype {
                "text" => {
                    if let Some(t) = block.get("text").and_then(|v| v.as_str()) {
                        text_content.push_str(t);
                    }
                }
                "thinking" => {
                    if let Some(t) = block.get("thinking").and_then(|v| v.as_str()) {
                        reasoning.push_str(t);
                    }
                }
                "tool_use" | "server_tool_use" => {
                    let tid = block.get("id").and_then(|v| v.as_str()).unwrap_or("");
                    let name = block.get("name").and_then(|v| v.as_str()).unwrap_or("");
                    let input = block.get("input").cloned().unwrap_or_else(|| json!({}));
                    let args = serde_json::to_string(&input).unwrap_or_else(|_| "{}".to_string());
                    tool_calls.push(json!({
                        "id": tid,
                        "type": "function",
                        "function": {"name": name, "arguments": args}
                    }));
                }
                _ => {}
            }
        }
    }

    let stop_reason = resp.get("stop_reason").and_then(|v| v.as_str());
    let finish_reason = anthropic_stop_to_finish(stop_reason);

    let mut message = obj();
    message.insert("role".to_string(), json!("assistant"));
    message.insert(
        "content".to_string(),
        if text_content.is_empty() {
            Value::Null
        } else {
            json!(text_content)
        },
    );
    if !reasoning.is_empty() {
        message.insert("reasoning_content".to_string(), json!(reasoning));
    }
    if !tool_calls.is_empty() {
        message.insert("tool_calls".to_string(), json!(tool_calls));
    }

    let mut out = obj();
    out.insert("id".to_string(), json!(id));
    out.insert("object".to_string(), json!("chat.completion"));
    out.insert("created".to_string(), json!(0));
    out.insert("model".to_string(), json!(model));
    out.insert(
        "choices".to_string(),
        json!([{
            "index": 0,
            "message": Value::Object(message),
            "finish_reason": finish_reason
        }]),
    );
    if let Some(usage) = resp.get("usage") {
        out.insert("usage".to_string(), anthropic_usage_to_chat(usage));
    }
    Value::Object(out)
}

// ==================== Chat 响应 → Anthropic 响应 ====================

/// Chat 响应 → Anthropic 响应。
pub fn openai_to_anthropic_response(resp: &Value, model: &str) -> Value {
    let id = resp
        .get("id")
        .and_then(|v| v.as_str())
        .map(|s| s.to_string())
        .unwrap_or_else(|| gen_id("msg_"));

    let choice = resp
        .get("choices")
        .and_then(|c| c.as_array())
        .and_then(|a| a.first());
    let message = choice.and_then(|c| c.get("message"));
    let finish_reason = choice
        .and_then(|c| c.get("finish_reason"))
        .and_then(|v| v.as_str());

    let mut blocks: Vec<Value> = Vec::new();

    if let Some(msg) = message {
        // reasoning_content → thinking（放最前）
        if let Some(rc) = msg.get("reasoning_content").and_then(|v| v.as_str()) {
            if !rc.is_empty() {
                blocks.push(json!({"type": "thinking", "thinking": rc}));
            }
        }
        // content → text
        let text = msg.get("content").map(extract_text).unwrap_or_default();
        if !text.is_empty() {
            blocks.push(json!({"type": "text", "text": text}));
        }
        // tool_calls → tool_use
        if let Some(tcs) = msg.get("tool_calls").and_then(|v| v.as_array()) {
            for tc in tcs {
                let tid = tc.get("id").and_then(|v| v.as_str()).unwrap_or("");
                let func = tc.get("function");
                let name = func
                    .and_then(|f| f.get("name"))
                    .and_then(|v| v.as_str())
                    .unwrap_or("");
                let args = func
                    .and_then(|f| f.get("arguments"))
                    .and_then(|v| v.as_str())
                    .unwrap_or("{}");
                let input: Value = serde_json::from_str(args).unwrap_or_else(|_| json!({}));
                blocks.push(json!({
                    "type": "tool_use",
                    "id": tid,
                    "name": name,
                    "input": input
                }));
            }
        }
    }

    let stop_reason = finish_to_anthropic_stop(finish_reason);

    let mut out = obj();
    out.insert("id".to_string(), json!(id));
    out.insert("type".to_string(), json!("message"));
    out.insert("role".to_string(), json!("assistant"));
    out.insert("model".to_string(), json!(model));
    out.insert("content".to_string(), json!(blocks));
    out.insert("stop_reason".to_string(), json!(stop_reason));
    out.insert("stop_sequence".to_string(), Value::Null);
    if let Some(usage) = resp.get("usage") {
        out.insert("usage".to_string(), chat_usage_to_anthropic(usage));
    }
    Value::Object(out)
}

// ==================== Responses 响应 → Chat 响应 ====================

/// Responses 响应 → Chat 响应。
pub fn responses_to_openai_response(resp: &Value, model: &str) -> Value {
    let id = resp
        .get("id")
        .and_then(|v| v.as_str())
        .map(|s| s.to_string())
        .unwrap_or_else(|| gen_id("chatcmpl-"));

    let mut text_content = String::new();
    let mut reasoning = String::new();
    let mut tool_calls: Vec<Value> = Vec::new();

    if let Some(output) = resp.get("output").and_then(|v| v.as_array()) {
        for item in output {
            let itype = item.get("type").and_then(|v| v.as_str()).unwrap_or("");
            match itype {
                "message" => {
                    if let Some(parts) = item.get("content").and_then(|v| v.as_array()) {
                        for part in parts {
                            let is_reasoning = part
                                .get("annotations")
                                .and_then(|a| a.as_array())
                                .map(|arr| arr.iter().any(|x| x.get("type").and_then(|t| t.as_str()) == Some("reasoning")))
                                .unwrap_or(false);
                            let text = part.get("text").and_then(|v| v.as_str()).unwrap_or("");
                            if is_reasoning {
                                reasoning.push_str(text);
                            } else {
                                text_content.push_str(text);
                            }
                        }
                    }
                }
                "reasoning" => {
                    // reasoning item：content[].text 或 summary[].text
                    if let Some(parts) = item.get("content").and_then(|v| v.as_array()) {
                        for part in parts {
                            if let Some(t) = part.get("text").and_then(|v| v.as_str()) {
                                reasoning.push_str(t);
                            }
                        }
                    }
                    if let Some(parts) = item.get("summary").and_then(|v| v.as_array()) {
                        for part in parts {
                            if let Some(t) = part.get("text").and_then(|v| v.as_str()) {
                                reasoning.push_str(t);
                            }
                        }
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
                        let input = item
                            .get("input")
                            .and_then(|v| v.as_str())
                            .unwrap_or("");
                        serde_json::to_string(&json!({"content": input}))
                            .unwrap_or_else(|_| "{}".to_string())
                    } else {
                        item.get("arguments").and_then(|v| v.as_str()).unwrap_or("{}").to_string()
                    };
                    tool_calls.push(json!({
                        "id": call_id,
                        "type": "function",
                        "function": {"name": name, "arguments": args}
                    }));
                }
                "refusal" => {
                    if let Some(text) = item.get("refusal").and_then(|v| v.as_str())
                        .or_else(|| item.get("text").and_then(|v| v.as_str()))
                    {
                        text_content.push_str(text);
                    }
                }
                _ => {}
            }
        }
    }

    let status = resp.get("status").and_then(|v| v.as_str()).unwrap_or("completed");
    let finish_reason = if !tool_calls.is_empty() {
        "tool_calls"
    } else if resp.get("output").and_then(|v| v.as_array()).map(|items| items.iter().any(|i| i.get("type").and_then(|v| v.as_str()) == Some("refusal"))).unwrap_or(false) {
        "content_filter"
    } else {
        status_to_finish_reason(status)
    };

    let mut message = obj();
    message.insert("role".to_string(), json!("assistant"));
    message.insert(
        "content".to_string(),
        if text_content.is_empty() {
            Value::Null
        } else {
            json!(text_content)
        },
    );
    if !reasoning.is_empty() {
        message.insert("reasoning_content".to_string(), json!(reasoning));
    }
    if !tool_calls.is_empty() {
        message.insert("tool_calls".to_string(), json!(tool_calls));
    }

    let mut out = obj();
    out.insert("id".to_string(), json!(id));
    out.insert("object".to_string(), json!("chat.completion"));
    out.insert("created".to_string(), json!(0));
    out.insert("model".to_string(), json!(model));
    out.insert(
        "choices".to_string(),
        json!([{
            "index": 0,
            "message": Value::Object(message),
            "finish_reason": finish_reason
        }]),
    );
    if let Some(usage) = resp.get("usage") {
        out.insert("usage".to_string(), responses_usage_to_chat(usage));
    }
    Value::Object(out)
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn responses_refusal_becomes_content_filter() {
        let result = responses_to_openai_response(&json!({
            "id":"resp_1",
            "status":"completed",
            "output":[{"type":"refusal","refusal":"cannot comply"}]
        }), "m");
        assert_eq!(result["choices"][0]["finish_reason"], "content_filter");
        assert_eq!(result["choices"][0]["message"]["content"], "cannot comply");
    }
}
