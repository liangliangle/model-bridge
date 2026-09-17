//! 回归测试：流式响应的 usage 必须真实下发到客户端。
//!
//! 背景（本轮修复的两个问题）：
//! 1. Anthropic 形态的流式输出里，`message_start` / `message_delta` 的 usage
//!    此前**被写死为 0**，客户端（如 Claude Code）看到的 token 数恒为 0。
//! 2. 网关从不向上游请求 `stream_options.include_usage`，而 OpenAI Chat 默认
//!    不在流里返回 usage，导致 Chat 上游的流式请求 token 统计无处可取。

use model_bridge_lib::converter::common::new_ns_reverse_map;
use model_bridge_lib::converter::stream::{self, StreamConverter};
use model_bridge_lib::converter::ApiFormat;
use serde_json::Value;

fn drain(conv: Box<dyn StreamConverter>, chunks: &[&[u8]]) -> String {
    let mut out: Vec<Vec<u8>> = Vec::new();
    let mut conv = conv;
    for c in chunks {
        out.extend(conv.process_chunk(c));
    }
    out.extend(conv.finalize());
    out.into_iter()
        .map(|b| String::from_utf8_lossy(&b).to_string())
        .collect()
}

/// 取某类事件的 data JSON 列表
fn event_data(text: &str, want: &str) -> Vec<Value> {
    let mut res = Vec::new();
    for block in text.split("\n\n") {
        let mut name: Option<&str> = None;
        let mut data: Option<&str> = None;
        for line in block.lines() {
            if let Some(n) = line.strip_prefix("event: ") {
                name = Some(n.trim());
            }
            if let Some(d) = line.strip_prefix("data: ") {
                data = Some(d.trim());
            }
        }
        if name == Some(want) {
            if let Some(d) = data {
                if let Ok(v) = serde_json::from_str::<Value>(d) {
                    res.push(v);
                }
            }
        }
    }
    res
}

// ==================== Chat 上游 → Anthropic 客户端 ====================

/// OpenAI 在开启 include_usage 后，usage 位于 finish_reason 之后、[DONE] 之前的
/// 一个 `choices: []` chunk 里。收尾必须推迟到该 chunk 之后才能带上 usage。
#[test]
fn chat_stream_usage_reaches_anthropic_message_delta() {
    let conv = stream::build(ApiFormat::Anthropic, ApiFormat::OpenAIChat, new_ns_reverse_map());
    let text = drain(
        conv,
        &[
            b"data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}]}\n\n",
            b"data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n",
            b"data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[],\"usage\":{\"prompt_tokens\":123,\"completion_tokens\":45,\"total_tokens\":168}}\n\n",
            b"data: [DONE]\n\n",
        ],
    );
    let deltas = event_data(&text, "message_delta");
    assert_eq!(deltas.len(), 1, "应恰好一个 message_delta");
    assert_eq!(deltas[0]["usage"]["input_tokens"], 123);
    assert_eq!(deltas[0]["usage"]["output_tokens"], 45);
    assert!(text.contains("event: message_stop"), "必须补齐 message_stop");
}

/// 部分兼容实现把 usage 直接塞在带 finish_reason 的同一个 chunk 里，也必须取到。
#[test]
fn chat_stream_usage_in_finish_chunk_is_captured() {
    let conv = stream::build(ApiFormat::Anthropic, ApiFormat::OpenAIChat, new_ns_reverse_map());
    let text = drain(
        conv,
        &[
            b"data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"x\"},\"finish_reason\":null}]}\n\n",
            b"data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"length\"}],\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":7}}\n\n",
            b"data: [DONE]\n\n",
        ],
    );
    let d = &event_data(&text, "message_delta")[0];
    assert_eq!(d["usage"]["input_tokens"], 9);
    assert_eq!(d["usage"]["output_tokens"], 7);
    assert_eq!(d["delta"]["stop_reason"], "max_tokens");
}

/// Chat 的 prompt_tokens 含缓存命中，Anthropic 的 input_tokens 不含：
/// 必须扣除后再下发，否则下游把两项相加会重复计数。
#[test]
fn chat_stream_cache_tokens_use_anthropic_convention() {
    let conv = stream::build(ApiFormat::Anthropic, ApiFormat::OpenAIChat, new_ns_reverse_map());
    let text = drain(
        conv,
        &[
            b"data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"x\"},\"finish_reason\":null}]}\n\n",
            b"data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":5,\"prompt_tokens_details\":{\"cached_tokens\":40}}}\n\n",
            b"data: [DONE]\n\n",
        ],
    );
    let u = &event_data(&text, "message_delta")[0]["usage"];
    assert_eq!(u["input_tokens"], 60, "应扣除缓存命中部分");
    assert_eq!(u["cache_read_input_tokens"], 40);
    assert_eq!(u["output_tokens"], 5);
}

/// 工具调用场景：stop_reason 仍要为 tool_use（推后收尾不应影响结束原因判定）。
#[test]
fn chat_stream_tool_stop_reason_survives_deferred_finish() {
    let conv = stream::build(ApiFormat::Anthropic, ApiFormat::OpenAIChat, new_ns_reverse_map());
    let text = drain(
        conv,
        &[
            b"data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"t1\",\"function\":{\"name\":\"f\",\"arguments\":\"{}\"}}]},\"finish_reason\":null}]}\n\n",
            b"data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n",
            b"data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":3}}\n\n",
            b"data: [DONE]\n\n",
        ],
    );
    let d = &event_data(&text, "message_delta")[0];
    assert_eq!(d["delta"]["stop_reason"], "tool_use");
    assert_eq!(d["usage"]["input_tokens"], 11);
}

/// 上游完全不给 usage 时优雅退化：仍要补齐 message_delta / message_stop。
#[test]
fn chat_stream_without_usage_still_finishes() {
    let conv = stream::build(ApiFormat::Anthropic, ApiFormat::OpenAIChat, new_ns_reverse_map());
    let text = drain(
        conv,
        &[
            b"data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"x\"},\"finish_reason\":null}]}\n\n",
            b"data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n",
            b"data: [DONE]\n\n",
        ],
    );
    let d = &event_data(&text, "message_delta")[0];
    assert_eq!(d["usage"]["input_tokens"], 0);
    assert_eq!(d["usage"]["output_tokens"], 0);
    assert!(text.contains("event: message_stop"));
}

/// 上游不发 [DONE] 直接断流时，finalize() 也必须收尾。
#[test]
fn chat_stream_finalize_without_done_closes_stream() {
    let mut conv = stream::build(ApiFormat::Anthropic, ApiFormat::OpenAIChat, new_ns_reverse_map());
    let _ = conv.process_chunk(
        b"data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"x\"},\"finish_reason\":null}]}\n\n",
    );
    let tail: String = conv
        .finalize()
        .into_iter()
        .map(|b| String::from_utf8_lossy(&b).to_string())
        .collect();
    assert!(tail.contains("event: message_delta"), "finalize 必须补齐 message_delta");
    assert!(tail.contains("event: message_stop"), "finalize 必须补齐 message_stop");
}

// ==================== Responses 上游 → Anthropic 客户端 ====================

#[test]
fn responses_stream_usage_reaches_anthropic_message_delta() {
    let conv = stream::build(ApiFormat::Anthropic, ApiFormat::OpenAIResponses, new_ns_reverse_map());
    let text = drain(
        conv,
        &[
            b"event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"r1\",\"model\":\"m\",\"usage\":null}}\n\n",
            b"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n",
            b"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r1\",\"model\":\"m\",\"status\":\"completed\",\"usage\":{\"input_tokens\":200,\"output_tokens\":30,\"total_tokens\":230}}}\n\n",
        ],
    );
    let d = &event_data(&text, "message_delta")[0];
    assert_eq!(d["usage"]["input_tokens"], 200);
    assert_eq!(d["usage"]["output_tokens"], 30);
    assert_eq!(d["delta"]["stop_reason"], "end_turn");
    assert!(text.contains("event: message_stop"));
}

// ==================== 各种上游 → Responses 客户端 ====================

/// 取 response.completed 事件里的 response 对象
fn completed_response(text: &str) -> Value {
    event_data(text, "response.completed")
        .into_iter()
        .find_map(|v| v.get("response").cloned())
        .expect("必须有 response.completed 事件")
}

/// Chat 上游：usage 在 finish_reason 之后到达，收尾必须推迟才能带上它。
#[test]
fn chat_upstream_usage_reaches_responses_completed() {
    let conv = stream::build(ApiFormat::OpenAIResponses, ApiFormat::OpenAIChat, new_ns_reverse_map());
    let text = drain(
        conv,
        &[
            b"data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}]}\n\n",
            b"data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n",
            b"data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[],\"usage\":{\"prompt_tokens\":150,\"completion_tokens\":25,\"total_tokens\":175}}\n\n",
            b"data: [DONE]\n\n",
        ],
    );
    let resp = completed_response(&text);
    assert_eq!(resp["status"], "completed");
    assert_eq!(resp["usage"]["input_tokens"], 150);
    assert_eq!(resp["usage"]["output_tokens"], 25);
    assert_eq!(resp["usage"]["total_tokens"], 175);
}

/// Responses 的 input_tokens 与 Chat 的 prompt_tokens 同为「含缓存」口径，
/// 跨协议往返后数值必须回到原样。
#[test]
fn chat_upstream_cache_tokens_roundtrip_to_responses() {
    let conv = stream::build(ApiFormat::OpenAIResponses, ApiFormat::OpenAIChat, new_ns_reverse_map());
    let text = drain(
        conv,
        &[
            b"data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"x\"},\"finish_reason\":null}]}\n\n",
            b"data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":5,\"prompt_tokens_details\":{\"cached_tokens\":40}}}\n\n",
            b"data: [DONE]\n\n",
        ],
    );
    let u = &completed_response(&text)["usage"];
    assert_eq!(u["input_tokens"], 100, "含缓存的输入总量应回到原值");
    assert_eq!(u["input_tokens_details"]["cached_tokens"], 40);
    assert_eq!(u["total_tokens"], 105);
}

/// Anthropic 上游：输入来自 message_start、累积输出来自 message_delta，
/// 两者都先于 message_stop，因此无需推迟收尾。
#[test]
fn anthropic_upstream_usage_reaches_responses_completed() {
    let conv = stream::build(ApiFormat::OpenAIResponses, ApiFormat::Anthropic, new_ns_reverse_map());
    let text = drain(
        conv,
        &[
            b"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude\",\"content\":[],\"usage\":{\"input_tokens\":300,\"output_tokens\":0}}}\n\n",
            b"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
            b"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n",
            b"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
            b"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":42}}\n\n",
            b"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
        ],
    );
    let u = &completed_response(&text)["usage"];
    assert_eq!(u["input_tokens"], 300);
    assert_eq!(u["output_tokens"], 42);
    assert_eq!(u["total_tokens"], 342);
}

/// Anthropic 的缓存读取量与 Responses 同为「输入的子集」，直接搬运。
#[test]
fn anthropic_upstream_cache_tokens_reach_responses() {
    let conv = stream::build(ApiFormat::OpenAIResponses, ApiFormat::Anthropic, new_ns_reverse_map());
    let text = drain(
        conv,
        &[
            b"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"model\":\"claude\",\"content\":[],\"usage\":{\"input_tokens\":60,\"cache_read_input_tokens\":40,\"output_tokens\":0}}}\n\n",
            b"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":7}}\n\n",
            b"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
        ],
    );
    let u = &completed_response(&text)["usage"];
    // Anthropic 的 input_tokens 不含缓存，Responses 的 input_tokens 含，故相加
    assert_eq!(u["input_tokens"], 100);
    assert_eq!(u["input_tokens_details"]["cached_tokens"], 40);
    assert_eq!(u["output_tokens"], 7);
}

/// Responses 规范里 usage 是 response 对象的必填字段：上游不给也要给零值对象，
/// 不能省略字段、也不能填 null（按类型解析的客户端会在必填字段上失败）。
#[test]
fn responses_completed_always_carries_usage_object() {
    // Chat 上游，不带 usage chunk
    let chat_conv =
        stream::build(ApiFormat::OpenAIResponses, ApiFormat::OpenAIChat, new_ns_reverse_map());
    let chat_text = drain(
        chat_conv,
        &[
            b"data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}]}\n\n",
            b"data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n",
            b"data: [DONE]\n\n",
        ],
    );
    assert_zero_usage_object(&completed_response(&chat_text), "chat upstream");

    // Anthropic 上游，usage 事件里没有 token 字段
    let ant_conv =
        stream::build(ApiFormat::OpenAIResponses, ApiFormat::Anthropic, new_ns_reverse_map());
    let ant_text = drain(
        ant_conv,
        &[
            b"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"model\":\"c\",\"content\":[]}}\n\n",
            b"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n",
            b"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
        ],
    );
    assert_zero_usage_object(&completed_response(&ant_text), "anthropic upstream");
}

fn assert_zero_usage_object(resp: &Value, label: &str) {
    let usage = resp
        .get("usage")
        .unwrap_or_else(|| panic!("{label}: response.completed 必须带 usage 字段"));
    assert!(usage.is_object(), "{label}: usage 不能是 null");
    assert_eq!(usage["input_tokens"], 0, "{label}");
    assert_eq!(usage["output_tokens"], 0, "{label}");
    assert_eq!(usage["total_tokens"], 0, "{label}");
}

/// Responses 的 input_tokens 同样含缓存命中，口径需与 Chat 一致。
#[test]
fn responses_stream_cache_tokens_use_anthropic_convention() {
    let conv = stream::build(ApiFormat::Anthropic, ApiFormat::OpenAIResponses, new_ns_reverse_map());
    let text = drain(
        conv,
        &[
            b"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n",
            b"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r1\",\"model\":\"m\",\"status\":\"completed\",\"usage\":{\"input_tokens\":500,\"output_tokens\":10,\"input_tokens_details\":{\"cached_tokens\":128}}}}\n\n",
        ],
    );
    let u = &event_data(&text, "message_delta")[0]["usage"];
    assert_eq!(u["input_tokens"], 372);
    assert_eq!(u["cache_read_input_tokens"], 128);
}
