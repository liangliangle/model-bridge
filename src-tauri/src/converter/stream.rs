//! 流式转换：SSE 解析 + 各方向状态机。
//! 每个下游事件编码为 `event: <type>\ndata: <json>\n\n`，Responses 事件的 data 带自增 sequence_number。

use serde_json::{json, Value};

use super::common::{
    anthropic_stop_to_finish, finish_reason_to_status, finish_to_anthropic_stop, gen_id,
    new_ns_reverse_map, status_to_finish_reason, unwrap_custom_tool_arguments,
    CUSTOM_TOOL_NAMESPACE_MARKER, NsReverseMap,
};
use super::ApiFormat;

/// 统一流式转换器接口。约束 Send 以便在 async stream 中跨 await 持有。
pub trait StreamConverter: Send {
    /// 输入原始 SSE 字节（可能跨事件边界），输出若干条已编码的下游 SSE 字节块。
    fn process_chunk(&mut self, chunk: &[u8]) -> Vec<Vec<u8>>;
    /// 流结束时的收尾（可选）。
    fn finalize(&mut self) -> Vec<Vec<u8>> {
        Vec::new()
    }
}

/// 工厂：依据 (入口格式 from, provider 格式 to) 返回对应状态机。
/// 流方向是 provider → 入口。
pub fn build(from: ApiFormat, to: ApiFormat, ns_reverse: NsReverseMap) -> Box<dyn StreamConverter> {
    use ApiFormat::*;
    match (from, to) {
        (OpenAIResponses, OpenAIChat) => {
            Box::new(ChatCompletionsToResponsesStream::new_with_ns(ns_reverse))
        }
        (OpenAIResponses, Anthropic) => {
            Box::new(AnthropicToResponsesStream::new_with_ns(ns_reverse))
        }
        (OpenAIChat, Anthropic) => Box::new(AnthropicToChatStream::new()),
        (OpenAIChat, OpenAIResponses) => Box::new(ResponsesToChatStream::new()),
        (Anthropic, OpenAIChat) => Box::new(ChatToAnthropicStream::new()),
        (Anthropic, OpenAIResponses) => Box::new(ResponsesToAnthropicStream::new()),
        _ => Box::new(PassthroughStream::new()),
    }
}

// ==================== SSE 解析工具 ====================

/// 累积字节，按 `\n\n` 切完整事件。跨 chunk 边界健壮。
#[derive(Default)]
pub struct SseBuffer {
    buf: Vec<u8>,
}

/// 单个 SSE 事件解析结果。
pub struct SseEvent {
    pub event: Option<String>,
    pub data: String,
}

impl SseBuffer {
    pub fn new() -> Self {
        Self { buf: Vec::new() }
    }

    pub fn push(&mut self, chunk: &[u8]) {
        self.buf.extend_from_slice(chunk);
    }

    /// 提取完整事件（以 `\n\n` 分隔），剩余不完整部分留在 buffer。
    pub fn drain_events(&mut self) -> Vec<SseEvent> {
        let mut events = Vec::new();
        loop {
            let pos = find_double_newline(&self.buf);
            match pos {
                Some(p) => {
                    let block: Vec<u8> = self.buf[..p].to_vec();
                    // 移除已处理块 + 分隔符
                    let sep_len = double_newline_len(&self.buf, p);
                    self.buf.drain(..p + sep_len);
                    let block_str = String::from_utf8_lossy(&block).to_string();
                    if let Some(ev) = parse_sse_block(&block_str) {
                        events.push(ev);
                    }
                }
                None => break,
            }
        }
        events
    }
}

fn find_double_newline(buf: &[u8]) -> Option<usize> {
    // 优先匹配 \n\n，兼容 \r\n\r\n
    let mut i = 0;
    while i + 1 < buf.len() {
        if buf[i] == b'\n' && buf[i + 1] == b'\n' {
            return Some(i);
        }
        if i + 3 < buf.len()
            && buf[i] == b'\r'
            && buf[i + 1] == b'\n'
            && buf[i + 2] == b'\r'
            && buf[i + 3] == b'\n'
        {
            return Some(i);
        }
        i += 1;
    }
    None
}

fn double_newline_len(buf: &[u8], pos: usize) -> usize {
    if pos + 3 < buf.len()
        && buf[pos] == b'\r'
        && buf[pos + 1] == b'\n'
        && buf[pos + 2] == b'\r'
        && buf[pos + 3] == b'\n'
    {
        4
    } else {
        2
    }
}

fn parse_sse_block(block: &str) -> Option<SseEvent> {
    let mut event: Option<String> = None;
    let mut data_lines: Vec<String> = Vec::new();
    for line in block.lines() {
        let line = line.trim_end_matches('\r');
        if let Some(rest) = line.strip_prefix("event:") {
            event = Some(rest.trim().to_string());
        } else if let Some(rest) = line.strip_prefix("data:") {
            data_lines.push(rest.trim().to_string());
        }
    }
    if data_lines.is_empty() && event.is_none() {
        return None;
    }
    Some(SseEvent {
        event,
        data: data_lines.join("\n"),
    })
}

// ==================== Responses 事件发射器（共享）====================

#[derive(Debug, Clone, Copy, PartialEq)]
enum PartKind {
    Text,
    Reasoning,
}

/// 流式 function_call 累积状态。
struct ToolCallState {
    call_id: String,
    name: String,
    namespace: Option<String>,
    original_name: Option<String>,
    custom: bool,
    output_index: i64,
    args: String,
    done: bool,
}

/// 生成 Responses 事件的共享状态机，供 Chat→Responses 与 Anthropic→Responses 复用。
struct ResponsesEmitter {
    response_id: String,
    item_id: String,
    model: String,
    status: String,
    seq: u64,
    sent_created: bool,
    sent_item_added: bool,
    any_part_opened: bool,
    content_index: i64,
    part_open: Option<PartKind>,
    current_text: String,
    accumulated_text: String,
    accumulated_reasoning: String,
    tools: Vec<ToolCallState>,
    next_output_index: i64,
    finished: bool,
    ns_reverse: NsReverseMap,
}

impl ResponsesEmitter {
    #[allow(dead_code)]
    fn new() -> Self {
        Self::new_with_ns(new_ns_reverse_map())
    }

    fn new_with_ns(ns_reverse: NsReverseMap) -> Self {
        Self {
            response_id: gen_id("resp_"),
            item_id: gen_id("msg_"),
            model: String::new(),
            status: "completed".to_string(),
            seq: 0,
            sent_created: false,
            sent_item_added: false,
            any_part_opened: false,
            content_index: 0,
            part_open: None,
            current_text: String::new(),
            accumulated_text: String::new(),
            accumulated_reasoning: String::new(),
            tools: Vec::new(),
            next_output_index: 1,
            finished: false,
            ns_reverse,
        }
    }

    fn emit(&mut self, event_type: &str, mut data: Value) -> Vec<u8> {
        if let Some(m) = data.as_object_mut() {
            m.insert("type".to_string(), json!(event_type));
            m.insert("sequence_number".to_string(), json!(self.seq));
        }
        self.seq += 1;
        format!(
            "event: {}\ndata: {}\n\n",
            event_type,
            serde_json::to_string(&data).unwrap_or_default()
        )
        .into_bytes()
    }

    fn base_response(&self, status: &str, output: Value) -> Value {
        json!({
            "id": self.response_id,
            "object": "response",
            "created_at": 0,
            "model": self.model,
            "status": status,
            "output": output,
        })
    }

    fn ensure_started(&mut self, out: &mut Vec<Vec<u8>>) {
        if self.sent_created {
            return;
        }
        self.sent_created = true;
        let resp = self.base_response("in_progress", json!([]));
        out.push(self.emit("response.created", json!({ "response": resp.clone() })));
        out.push(self.emit("response.in_progress", json!({ "response": resp })));
    }

    fn ensure_item_added(&mut self, out: &mut Vec<Vec<u8>>) {
        if self.sent_item_added {
            return;
        }
        self.sent_item_added = true;
        out.push(self.emit(
            "response.output_item.added",
            json!({
                "output_index": 0,
                "item": {
                    "type": "message",
                    "id": self.item_id,
                    "status": "in_progress",
                    "role": "assistant",
                    "content": []
                }
            }),
        ));
    }

    fn part_value(kind: PartKind) -> Value {
        match kind {
            PartKind::Text => json!({"type": "output_text", "text": "", "annotations": []}),
            PartKind::Reasoning => {
                json!({"type": "output_text", "text": "", "annotations": [{"type": "reasoning"}]})
            }
        }
    }

    fn open_part(&mut self, kind: PartKind, out: &mut Vec<Vec<u8>>) {
        if self.any_part_opened {
            self.content_index += 1;
        }
        self.any_part_opened = true;
        self.current_text.clear();
        let part = Self::part_value(kind);
        let ci = self.content_index;
        let item_id = self.item_id.clone();
        out.push(self.emit(
            "response.content_part.added",
            json!({
                "item_id": item_id,
                "output_index": 0,
                "content_index": ci,
                "part": part
            }),
        ));
        self.part_open = Some(kind);
    }

    fn close_part(&mut self, out: &mut Vec<Vec<u8>>) {
        let kind = match self.part_open {
            Some(k) => k,
            None => return,
        };
        let ci = self.content_index;
        let item_id = self.item_id.clone();
        let text = self.current_text.clone();
        out.push(self.emit(
            "response.output_text.done",
            json!({
                "item_id": item_id,
                "output_index": 0,
                "content_index": ci,
                "text": text
            }),
        ));
        let mut part = Self::part_value(kind);
        part["text"] = json!(self.current_text);
        let item_id = self.item_id.clone();
        out.push(self.emit(
            "response.content_part.done",
            json!({
                "item_id": item_id,
                "output_index": 0,
                "content_index": ci,
                "part": part
            }),
        ));
        self.part_open = None;
    }

    fn ensure_part(&mut self, kind: PartKind, out: &mut Vec<Vec<u8>>) {
        if self.part_open == Some(kind) {
            return;
        }
        if self.part_open.is_some() {
            self.close_part(out);
        }
        self.ensure_item_added(out);
        self.open_part(kind, out);
    }

    fn text_delta(&mut self, kind: PartKind, delta: &str, out: &mut Vec<Vec<u8>>) {
        self.ensure_started(out);
        self.ensure_part(kind, out);
        self.current_text.push_str(delta);
        match kind {
            PartKind::Text => self.accumulated_text.push_str(delta),
            PartKind::Reasoning => self.accumulated_reasoning.push_str(delta),
        }
        let ci = self.content_index;
        let item_id = self.item_id.clone();
        out.push(self.emit(
            "response.output_text.delta",
            json!({
                "item_id": item_id,
                "output_index": 0,
                "content_index": ci,
                "delta": delta
            }),
        ));
    }

    fn tool_added(&mut self, call_id: &str, name: &str, out: &mut Vec<Vec<u8>>) {
        self.ensure_started(out);
        if self.part_open.is_some() {
            self.close_part(out);
        }
        let output_index = self.next_output_index;
        self.next_output_index += 1;
        let ns_map = self.ns_reverse.read();
        let (ns_info, emit_name, custom) = if let Some((ns, sub)) = ns_map.get(name) {
            if ns == CUSTOM_TOOL_NAMESPACE_MARKER {
                ((None, None), sub.clone(), true)
            } else {
                ((Some(ns.clone()), Some(sub.clone())), sub.clone(), false)
            }
        } else {
            ((None, None), name.to_string(), false)
        };
        drop(ns_map);
        self.tools.push(ToolCallState {
            call_id: call_id.to_string(),
            name: name.to_string(),
            namespace: ns_info.0,
            original_name: ns_info.1,
            custom,
            output_index,
            args: String::new(),
            done: false,
        });
        let mut item = if custom {
            json!({
                "type": "custom_tool_call",
                "id": call_id,
                "call_id": call_id,
                "name": emit_name,
                "input": "",
                "status": "in_progress"
            })
        } else {
            json!({
                "type": "function_call",
                "id": call_id,
                "call_id": call_id,
                "name": emit_name,
                "arguments": "",
                "status": "in_progress"
            })
        };
        if let Some(ns) = self.tools.last().and_then(|t| t.namespace.clone()) {
            item["namespace"] = json!(ns);
        }
        out.push(self.emit(
            "response.output_item.added",
            json!({
                "output_index": output_index,
                "item": item
            }),
        ));
    }

    fn tool_args_delta(&mut self, call_id: &str, delta: &str, out: &mut Vec<Vec<u8>>) {
        let (output_index, cid) = match self.tools.iter_mut().find(|t| t.call_id == call_id) {
            Some(t) => {
                t.args.push_str(delta);
                (t.output_index, t.call_id.clone())
            }
            None => return,
        };
        out.push(self.emit(
            "response.function_call_arguments.delta",
            json!({
                "item_id": cid,
                "output_index": output_index,
                "delta": delta
            }),
        ));
    }

    fn tool_done(&mut self, call_id: &str, out: &mut Vec<Vec<u8>>) {
        let idx = match self
            .tools
            .iter()
            .position(|t| t.call_id == call_id && !t.done)
        {
            Some(i) => i,
            None => return,
        };
        self.tools[idx].done = true;
        let (cid, name, namespace, original_name, output_index, args) = {
            let t = &self.tools[idx];
            let args = if t.args.is_empty() {
                "{}".to_string()
            } else {
                t.args.clone()
            };
            (
                t.call_id.clone(),
                t.name.clone(),
                t.namespace.clone(),
                t.original_name.clone(),
                t.output_index,
                args,
            )
        };
        let emit_name = original_name.as_deref().unwrap_or(&name);
        out.push(self.emit(
            "response.function_call_arguments.done",
            json!({
                "item_id": cid,
                "output_index": output_index,
                "arguments": args
            }),
        ));
        let mut item = if self.tools[idx].custom {
            json!({
                "type": "custom_tool_call",
                "id": cid,
                "call_id": cid,
                "name": emit_name,
                "input": unwrap_custom_tool_arguments(&args),
                "status": "completed"
            })
        } else {
            json!({
                "type": "function_call",
                "id": cid,
                "call_id": cid,
                "name": emit_name,
                "arguments": args,
                "status": "completed"
            })
        };
        if let Some(ns) = &namespace {
            item["namespace"] = json!(ns);
        }
        out.push(self.emit(
            "response.output_item.done",
            json!({
                "output_index": output_index,
                "item": item
            }),
        ));
    }

    fn build_final_output(&self) -> Value {
        let mut items: Vec<Value> = Vec::new();
        let mut parts: Vec<Value> = Vec::new();
        if !self.accumulated_reasoning.is_empty() {
            parts.push(json!({
                "type": "output_text",
                "text": self.accumulated_reasoning,
                "annotations": [{"type": "reasoning"}]
            }));
        }
        if !self.accumulated_text.is_empty() {
            parts.push(json!({
                "type": "output_text",
                "text": self.accumulated_text,
                "annotations": []
            }));
        }
        if !parts.is_empty() {
            items.push(json!({
                "type": "message",
                "id": self.item_id,
                "status": "completed",
                "role": "assistant",
                "content": parts
            }));
        }
        for t in &self.tools {
            let args = if t.args.is_empty() {
                "{}".to_string()
            } else {
                t.args.clone()
            };
            let emit_name = t.original_name.as_deref().unwrap_or(&t.name);
            let mut item = if t.custom {
                json!({
                    "type": "custom_tool_call",
                    "id": t.call_id,
                    "call_id": t.call_id,
                    "name": emit_name,
                    "input": unwrap_custom_tool_arguments(&args),
                    "status": "completed"
                })
            } else {
                json!({
                    "type": "function_call",
                    "id": t.call_id,
                    "call_id": t.call_id,
                    "name": emit_name,
                    "arguments": args,
                    "status": "completed"
                })
            };
            if let Some(ns) = &t.namespace {
                item["namespace"] = json!(ns);
            }
            items.push(item);
        }
        json!(items)
    }

    fn finish(&mut self, out: &mut Vec<Vec<u8>>) {
        if self.finished {
            return;
        }
        self.finished = true;
        self.ensure_started(out);
        if self.part_open.is_some() {
            self.close_part(out);
        }
        let pending: Vec<String> = self
            .tools
            .iter()
            .filter(|t| !t.done)
            .map(|t| t.call_id.clone())
            .collect();
        for cid in pending {
            self.tool_done(&cid, out);
        }
        let output = self.build_final_output();
        if self.sent_item_added {
            if let Some(item) = output
                .as_array()
                .and_then(|a| a.iter().find(|it| it["type"] == "message"))
                .cloned()
            {
                out.push(self.emit(
                    "response.output_item.done",
                    json!({"output_index": 0, "item": item}),
                ));
            }
        }
        let status = self.status.clone();
        let resp = self.base_response(&status, output);
        out.push(self.emit("response.completed", json!({ "response": resp })));
    }
}

// ==================== ChatCompletionsToResponsesStream ====================

/// provider 返回 Chat 流 → 入口 Responses 事件流。
pub struct ChatCompletionsToResponsesStream {
    sse: SseBuffer,
    emitter: ResponsesEmitter,
    tool_index_to_call_id: std::collections::HashMap<i64, String>,
}

impl ChatCompletionsToResponsesStream {
    pub fn new() -> Self {
        Self::new_with_ns(new_ns_reverse_map())
    }

    pub fn new_with_ns(ns_reverse: NsReverseMap) -> Self {
        Self {
            sse: SseBuffer::new(),
            emitter: ResponsesEmitter::new_with_ns(ns_reverse),
            tool_index_to_call_id: std::collections::HashMap::new(),
        }
    }

    pub fn process_chunk(&mut self, chunk: &[u8]) -> Vec<Vec<u8>> {
        self.sse.push(chunk);
        let mut out: Vec<Vec<u8>> = Vec::new();
        for ev in self.sse.drain_events() {
            if ev.data == "[DONE]" {
                self.emitter.finish(&mut out);
                continue;
            }
            let json: Value = match serde_json::from_str(&ev.data) {
                Ok(v) => v,
                Err(_) => continue,
            };
            if self.emitter.model.is_empty() {
                if let Some(m) = json.get("model").and_then(|v| v.as_str()) {
                    self.emitter.model = m.to_string();
                }
            }
            self.emitter.ensure_started(&mut out);

            let choice = json
                .get("choices")
                .and_then(|c| c.as_array())
                .and_then(|a| a.first());
            let delta = choice.and_then(|c| c.get("delta"));

            if let Some(delta) = delta {
                if let Some(rc) = delta.get("reasoning_content").and_then(|v| v.as_str()) {
                    if !rc.is_empty() {
                        self.emitter.text_delta(PartKind::Reasoning, rc, &mut out);
                    }
                }
                if let Some(content) = delta.get("content").and_then(|v| v.as_str()) {
                    if !content.is_empty() {
                        self.emitter.text_delta(PartKind::Text, content, &mut out);
                    }
                }
                if let Some(tcs) = delta.get("tool_calls").and_then(|v| v.as_array()) {
                    for (i, tc) in tcs.iter().enumerate() {
                        let index = tc.get("index").and_then(|v| v.as_i64()).unwrap_or(i as i64);
                        let func = tc.get("function");
                        if let Some(id) = tc.get("id").and_then(|v| v.as_str()) {
                            if !id.is_empty() && !self.tool_index_to_call_id.contains_key(&index) {
                                let name = func
                                    .and_then(|f| f.get("name"))
                                    .and_then(|v| v.as_str())
                                    .unwrap_or("");
                                self.tool_index_to_call_id.insert(index, id.to_string());
                                self.emitter.tool_added(id, name, &mut out);
                            }
                        }
                        if let Some(call_id) = self.tool_index_to_call_id.get(&index).cloned() {
                            if let Some(args) = func
                                .and_then(|f| f.get("arguments"))
                                .and_then(|v| v.as_str())
                            {
                                if !args.is_empty() {
                                    self.emitter.tool_args_delta(&call_id, args, &mut out);
                                }
                            }
                        }
                    }
                }
            }

            if let Some(fr) = choice
                .and_then(|c| c.get("finish_reason"))
                .and_then(|v| v.as_str())
            {
                self.emitter.status = finish_reason_to_status(Some(fr)).to_string();
                self.emitter.finish(&mut out);
            }
        }
        out
    }

    pub fn finalize(&mut self) -> Vec<Vec<u8>> {
        let mut out = Vec::new();
        self.emitter.finish(&mut out);
        out
    }
}

impl Default for ChatCompletionsToResponsesStream {
    fn default() -> Self {
        Self::new()
    }
}

impl StreamConverter for ChatCompletionsToResponsesStream {
    fn process_chunk(&mut self, chunk: &[u8]) -> Vec<Vec<u8>> {
        ChatCompletionsToResponsesStream::process_chunk(self, chunk)
    }

    fn finalize(&mut self) -> Vec<Vec<u8>> {
        ChatCompletionsToResponsesStream::finalize(self)
    }
}

// ==================== AnthropicToResponsesStream ====================

/// provider 返回 Anthropic 流 → 入口 Responses 事件流。
pub struct AnthropicToResponsesStream {
    sse: SseBuffer,
    emitter: ResponsesEmitter,
    current_block: Option<PartKind>,
    tool_blocks: std::collections::HashMap<i64, String>,
    current_tool: Option<String>,
}

impl AnthropicToResponsesStream {
    pub fn new() -> Self {
        Self::new_with_ns(new_ns_reverse_map())
    }

    pub fn new_with_ns(ns_reverse: NsReverseMap) -> Self {
        Self {
            sse: SseBuffer::new(),
            emitter: ResponsesEmitter::new_with_ns(ns_reverse),
            current_block: None,
            tool_blocks: std::collections::HashMap::new(),
            current_tool: None,
        }
    }

    pub fn process_chunk(&mut self, chunk: &[u8]) -> Vec<Vec<u8>> {
        self.sse.push(chunk);
        let mut out: Vec<Vec<u8>> = Vec::new();
        for ev in self.sse.drain_events() {
            let json: Value = match serde_json::from_str(&ev.data) {
                Ok(v) => v,
                Err(_) => continue,
            };
            let etype = json
                .get("type")
                .and_then(|v| v.as_str())
                .or(ev.event.as_deref())
                .unwrap_or("");
            match etype {
                "message_start" => {
                    if let Some(m) = json
                        .get("message")
                        .and_then(|msg| msg.get("model"))
                        .and_then(|v| v.as_str())
                    {
                        self.emitter.model = m.to_string();
                    }
                    self.emitter.ensure_started(&mut out);
                }
                "content_block_start" => {
                    let btype = json
                        .get("content_block")
                        .and_then(|b| b.get("type"))
                        .and_then(|v| v.as_str())
                        .unwrap_or("text");
                    if btype == "tool_use" || btype == "server_tool_use" {
                        let index = json.get("index").and_then(|v| v.as_i64()).unwrap_or(0);
                        let cb = json.get("content_block");
                        let id = cb
                            .and_then(|b| b.get("id"))
                            .and_then(|v| v.as_str())
                            .map(|s| s.to_string())
                            .unwrap_or_else(|| gen_id("call_"));
                        let name = cb
                            .and_then(|b| b.get("name"))
                            .and_then(|v| v.as_str())
                            .unwrap_or("")
                            .to_string();
                        self.tool_blocks.insert(index, id.clone());
                        self.current_tool = Some(id.clone());
                        self.current_block = None;
                        self.emitter.tool_added(&id, &name, &mut out);
                    } else {
                        let kind = match btype {
                            "thinking" | "redacted_thinking" => PartKind::Reasoning,
                            _ => PartKind::Text,
                        };
                        self.current_tool = None;
                        self.current_block = Some(kind);
                        self.emitter.ensure_started(&mut out);
                        self.emitter.ensure_part(kind, &mut out);
                    }
                }
                "content_block_delta" => {
                    let delta = json.get("delta");
                    if let Some(delta) = delta {
                        let dtype = delta.get("type").and_then(|v| v.as_str()).unwrap_or("");
                        match dtype {
                            "thinking_delta" => {
                                if let Some(t) = delta.get("thinking").and_then(|v| v.as_str()) {
                                    self.emitter.text_delta(PartKind::Reasoning, t, &mut out);
                                }
                            }
                            "text_delta" => {
                                if let Some(t) = delta.get("text").and_then(|v| v.as_str()) {
                                    self.emitter.text_delta(PartKind::Text, t, &mut out);
                                }
                            }
                            "input_json_delta" => {
                                let index = json.get("index").and_then(|v| v.as_i64()).unwrap_or(0);
                                if let Some(call_id) = self.tool_blocks.get(&index).cloned() {
                                    if let Some(pj) =
                                        delta.get("partial_json").and_then(|v| v.as_str())
                                    {
                                        self.emitter.tool_args_delta(&call_id, pj, &mut out);
                                    }
                                }
                            }
                            _ => {}
                        }
                    }
                }
                "content_block_stop" => {
                    let index = json.get("index").and_then(|v| v.as_i64()).unwrap_or(0);
                    if let Some(call_id) = self.tool_blocks.get(&index).cloned() {
                        self.emitter.tool_done(&call_id, &mut out);
                        if self.current_tool.as_deref() == Some(call_id.as_str()) {
                            self.current_tool = None;
                        }
                    }
                }
                "message_delta" => {
                    if let Some(sr) = json
                        .get("delta")
                        .and_then(|d| d.get("stop_reason"))
                        .and_then(|v| v.as_str())
                    {
                        let fr = anthropic_stop_to_finish(Some(sr));
                        self.emitter.status = finish_reason_to_status(Some(fr)).to_string();
                    }
                }
                "message_stop" => {
                    self.emitter.finish(&mut out);
                }
                _ => {}
            }
        }
        out
    }

    pub fn finalize(&mut self) -> Vec<Vec<u8>> {
        let mut out = Vec::new();
        self.emitter.finish(&mut out);
        out
    }
}

impl Default for AnthropicToResponsesStream {
    fn default() -> Self {
        Self::new()
    }
}

impl StreamConverter for AnthropicToResponsesStream {
    fn process_chunk(&mut self, chunk: &[u8]) -> Vec<Vec<u8>> {
        AnthropicToResponsesStream::process_chunk(self, chunk)
    }

    fn finalize(&mut self) -> Vec<Vec<u8>> {
        AnthropicToResponsesStream::finalize(self)
    }
}

// ==================== AnthropicToChatStream ====================

/// provider Anthropic 流 → 入口 Chat 流。
pub struct AnthropicToChatStream {
    sse: SseBuffer,
    id: String,
    model: String,
    started: bool,
    done: bool,
    tool_index: i64,
    in_tool_block: bool,
    cur_tool_has_args: bool,
}

impl AnthropicToChatStream {
    pub fn new() -> Self {
        Self {
            sse: SseBuffer::new(),
            id: gen_id("chatcmpl-"),
            model: String::new(),
            started: false,
            done: false,
            tool_index: -1,
            in_tool_block: false,
            cur_tool_has_args: false,
        }
    }

    fn chunk(&self, delta: Value, finish_reason: Value) -> Vec<u8> {
        let json = json!({
            "id": self.id,
            "object": "chat.completion.chunk",
            "model": self.model,
            "choices": [{"index": 0, "delta": delta, "finish_reason": finish_reason}]
        });
        format!(
            "data: {}\n\n",
            serde_json::to_string(&json).unwrap_or_default()
        )
        .into_bytes()
    }
}

impl Default for AnthropicToChatStream {
    fn default() -> Self {
        Self::new()
    }
}

impl StreamConverter for AnthropicToChatStream {
    fn process_chunk(&mut self, chunk: &[u8]) -> Vec<Vec<u8>> {
        self.sse.push(chunk);
        let mut out: Vec<Vec<u8>> = Vec::new();
        for ev in self.sse.drain_events() {
            let json: Value = match serde_json::from_str(&ev.data) {
                Ok(v) => v,
                Err(_) => continue,
            };
            let etype = json.get("type").and_then(|v| v.as_str()).unwrap_or("");
            match etype {
                "message_start" => {
                    if let Some(m) = json
                        .get("message")
                        .and_then(|msg| msg.get("model"))
                        .and_then(|v| v.as_str())
                    {
                        self.model = m.to_string();
                    }
                    if !self.started {
                        self.started = true;
                        out.push(self.chunk(json!({"role": "assistant"}), Value::Null));
                    }
                }
                "content_block_start" => {
                    let btype = json
                        .get("content_block")
                        .and_then(|b| b.get("type"))
                        .and_then(|v| v.as_str())
                        .unwrap_or("text");
                    if btype == "tool_use" || btype == "server_tool_use" {
                        self.tool_index += 1;
                        self.in_tool_block = true;
                        self.cur_tool_has_args = false;
                        let cb = json.get("content_block");
                        let id = cb
                            .and_then(|b| b.get("id"))
                            .and_then(|v| v.as_str())
                            .map(|s| s.to_string())
                            .unwrap_or_else(|| gen_id("call_"));
                        let name = cb
                            .and_then(|b| b.get("name"))
                            .and_then(|v| v.as_str())
                            .unwrap_or("")
                            .to_string();
                        let ti = self.tool_index;
                        out.push(self.chunk(
                            json!({
                                "tool_calls": [{
                                    "index": ti,
                                    "id": id,
                                    "type": "function",
                                    "function": {"name": name, "arguments": ""}
                                }]
                            }),
                            Value::Null,
                        ));
                    } else {
                        self.in_tool_block = false;
                    }
                }
                "content_block_delta" => {
                    if let Some(delta) = json.get("delta") {
                        let dtype = delta.get("type").and_then(|v| v.as_str()).unwrap_or("");
                        match dtype {
                            "text_delta" => {
                                if let Some(t) = delta.get("text").and_then(|v| v.as_str()) {
                                    out.push(self.chunk(json!({"content": t}), Value::Null));
                                }
                            }
                            "thinking_delta" => {
                                if let Some(t) = delta.get("thinking").and_then(|v| v.as_str()) {
                                    out.push(
                                        self.chunk(json!({"reasoning_content": t}), Value::Null),
                                    );
                                }
                            }
                            "input_json_delta" => {
                                if self.in_tool_block {
                                    if let Some(pj) =
                                        delta.get("partial_json").and_then(|v| v.as_str())
                                    {
                                        self.cur_tool_has_args = true;
                                        let ti = self.tool_index;
                                        out.push(self.chunk(
                                            json!({
                                                "tool_calls": [{
                                                    "index": ti,
                                                    "function": {"arguments": pj}
                                                }]
                                            }),
                                            Value::Null,
                                        ));
                                    }
                                }
                            }
                            _ => {}
                        }
                    }
                }
                "content_block_stop" => {
                    if self.in_tool_block {
                        if !self.cur_tool_has_args {
                            let ti = self.tool_index;
                            out.push(self.chunk(
                                json!({
                                    "tool_calls": [{
                                        "index": ti,
                                        "function": {"arguments": "{}"}
                                    }]
                                }),
                                Value::Null,
                            ));
                        }
                        self.in_tool_block = false;
                    }
                }
                "message_delta" => {
                    if let Some(sr) = json
                        .get("delta")
                        .and_then(|d| d.get("stop_reason"))
                        .and_then(|v| v.as_str())
                    {
                        let fr = anthropic_stop_to_finish(Some(sr));
                        out.push(self.chunk(json!({}), json!(fr)));
                    }
                }
                "message_stop" => {
                    if !self.done {
                        self.done = true;
                        out.push(b"data: [DONE]\n\n".to_vec());
                    }
                }
                _ => {}
            }
        }
        out
    }

    fn finalize(&mut self) -> Vec<Vec<u8>> {
        let mut out = Vec::new();
        if !self.done {
            self.done = true;
            out.push(b"data: [DONE]\n\n".to_vec());
        }
        out
    }
}

// ==================== ResponsesToChatStream ====================

/// provider Responses 流 → 入口 Chat 流。
pub struct ResponsesToChatStream {
    sse: SseBuffer,
    id: String,
    model: String,
    started: bool,
    done: bool,
    tool_map: std::collections::HashMap<String, i64>,
    next_tool_index: i64,
    has_tool_calls: bool,
}

impl ResponsesToChatStream {
    pub fn new() -> Self {
        Self {
            sse: SseBuffer::new(),
            id: gen_id("chatcmpl-"),
            model: String::new(),
            started: false,
            done: false,
            tool_map: std::collections::HashMap::new(),
            next_tool_index: 0,
            has_tool_calls: false,
        }
    }

    fn chunk(&self, delta: Value, finish_reason: Value) -> Vec<u8> {
        let json = json!({
            "id": self.id,
            "object": "chat.completion.chunk",
            "model": self.model,
            "choices": [{"index": 0, "delta": delta, "finish_reason": finish_reason}]
        });
        format!(
            "data: {}\n\n",
            serde_json::to_string(&json).unwrap_or_default()
        )
        .into_bytes()
    }
}

impl Default for ResponsesToChatStream {
    fn default() -> Self {
        Self::new()
    }
}

impl StreamConverter for ResponsesToChatStream {
    fn process_chunk(&mut self, chunk: &[u8]) -> Vec<Vec<u8>> {
        self.sse.push(chunk);
        let mut out: Vec<Vec<u8>> = Vec::new();
        for ev in self.sse.drain_events() {
            if ev.data == "[DONE]" {
                continue;
            }
            let json: Value = match serde_json::from_str(&ev.data) {
                Ok(v) => v,
                Err(_) => continue,
            };
            let etype = json
                .get("type")
                .and_then(|v| v.as_str())
                .or(ev.event.as_deref())
                .unwrap_or("");

            if self.model.is_empty() {
                if let Some(m) = json
                    .get("response")
                    .and_then(|r| r.get("model"))
                    .and_then(|v| v.as_str())
                {
                    self.model = m.to_string();
                }
            }

            if !self.started {
                self.started = true;
                out.push(self.chunk(json!({"role": "assistant"}), Value::Null));
            }

            match etype {
                "response.output_text.delta" => {
                    // reasoning part 的 delta 也走这里，但 Chat 侧统一当 content
                    if let Some(d) = json.get("delta").and_then(|v| v.as_str()) {
                        out.push(self.chunk(json!({"content": d}), Value::Null));
                    }
                }
                "response.output_item.added" => {
                    let item = json.get("item");
                    let itype = item
                        .and_then(|i| i.get("type"))
                        .and_then(|v| v.as_str())
                        .unwrap_or("");
                    if itype == "function_call" {
                        self.has_tool_calls = true;
                        let call_id = item
                            .and_then(|i| i.get("call_id").or_else(|| i.get("id")))
                            .and_then(|v| v.as_str())
                            .map(|s| s.to_string())
                            .unwrap_or_else(|| gen_id("call_"));
                        let name = item
                            .and_then(|i| i.get("name"))
                            .and_then(|v| v.as_str())
                            .unwrap_or("")
                            .to_string();
                        let ti = self.next_tool_index;
                        self.next_tool_index += 1;
                        self.tool_map.insert(call_id.clone(), ti);
                        if let Some(item_id) = item.and_then(|i| i.get("id")).and_then(|v| v.as_str()) {
                            self.tool_map.insert(item_id.to_string(), ti);
                        }
                        out.push(self.chunk(
                            json!({
                                "tool_calls": [{
                                    "index": ti,
                                    "id": call_id,
                                    "type": "function",
                                    "function": {"name": name, "arguments": ""}
                                }]
                            }),
                            Value::Null,
                        ));
                    }
                }
                "response.function_call_arguments.delta" => {
                    let item_id = json.get("item_id").and_then(|v| v.as_str()).unwrap_or("");
                    if let Some(&ti) = self.tool_map.get(item_id) {
                        if let Some(d) = json.get("delta").and_then(|v| v.as_str()) {
                            out.push(self.chunk(
                                json!({
                                    "tool_calls": [{
                                        "index": ti,
                                        "function": {"arguments": d}
                                    }]
                                }),
                                Value::Null,
                            ));
                        }
                    }
                }
                "response.completed" | "response.incomplete" | "response.failed" => {
                    let status = json
                        .get("response")
                        .and_then(|r| r.get("status"))
                        .and_then(|v| v.as_str())
                        .unwrap_or("completed");
                    let fr = if self.has_tool_calls {
                        "tool_calls"
                    } else {
                        status_to_finish_reason(status)
                    };
                    out.push(self.chunk(json!({}), json!(fr)));
                    if !self.done {
                        self.done = true;
                        out.push(b"data: [DONE]\n\n".to_vec());
                    }
                }
                _ => {}
            }
        }
        out
    }

    fn finalize(&mut self) -> Vec<Vec<u8>> {
        let mut out = Vec::new();
        if !self.done {
            self.done = true;
            out.push(b"data: [DONE]\n\n".to_vec());
        }
        out
    }
}

// ==================== ChatToAnthropicStream ====================

/// provider Chat 流 → 入口 Anthropic 事件流。
pub struct ChatToAnthropicStream {
    sse: SseBuffer,
    id: String,
    model: String,
    started: bool,
    stopped: bool,
    next_index: i64,
    open_block: Option<i64>,
    text_index: Option<i64>,
    reasoning_index: Option<i64>,
    tool_blocks: std::collections::HashMap<i64, i64>,
    has_tool: bool,
}

impl ChatToAnthropicStream {
    pub fn new() -> Self {
        Self {
            sse: SseBuffer::new(),
            id: gen_id("msg_"),
            model: String::new(),
            started: false,
            stopped: false,
            next_index: 0,
            open_block: None,
            text_index: None,
            reasoning_index: None,
            tool_blocks: std::collections::HashMap::new(),
            has_tool: false,
        }
    }

    fn sse(event: &str, data: Value) -> Vec<u8> {
        format!(
            "event: {}\ndata: {}\n\n",
            event,
            serde_json::to_string(&data).unwrap_or_default()
        )
        .into_bytes()
    }

    fn ensure_started(&mut self, out: &mut Vec<Vec<u8>>) {
        if self.started {
            return;
        }
        self.started = true;
        out.push(Self::sse(
            "message_start",
            json!({
                "type": "message_start",
                "message": {
                    "id": self.id,
                    "type": "message",
                    "role": "assistant",
                    "model": self.model,
                    "content": [],
                    "stop_reason": Value::Null,
                    "usage": {"input_tokens": 0, "output_tokens": 0}
                }
            }),
        ));
    }

    fn close_open_block(&mut self, out: &mut Vec<Vec<u8>>) {
        if let Some(idx) = self.open_block.take() {
            out.push(Self::sse(
                "content_block_stop",
                json!({"type": "content_block_stop", "index": idx}),
            ));
        }
    }

    fn ensure_text_block(&mut self, out: &mut Vec<Vec<u8>>) {
        self.ensure_started(out);
        if self.open_block.is_some() && self.open_block == self.text_index {
            return;
        }
        self.close_open_block(out);
        let idx = match self.text_index {
            Some(i) => i,
            None => {
                let i = self.next_index;
                self.next_index += 1;
                self.text_index = Some(i);
                i
            }
        };
        out.push(Self::sse(
            "content_block_start",
            json!({
                "type": "content_block_start",
                "index": idx,
                "content_block": {"type": "text", "text": ""}
            }),
        ));
        self.open_block = Some(idx);
    }

    fn ensure_reasoning_block(&mut self, out: &mut Vec<Vec<u8>>) {
        self.ensure_started(out);
        if let Some(idx) = self.reasoning_index {
            if self.open_block == Some(idx) { return; }
        }
        self.close_open_block(out);
        let idx = self.reasoning_index.unwrap_or_else(|| {
            let i = self.next_index;
            self.next_index += 1;
            self.reasoning_index = Some(i);
            i
        });
        out.push(Self::sse("content_block_start", json!({
            "type":"content_block_start", "index":idx,
            "content_block":{"type":"thinking","thinking":""}
        })));
        self.open_block = Some(idx);
    }
}

impl Default for ChatToAnthropicStream {
    fn default() -> Self {
        Self::new()
    }
}

impl StreamConverter for ChatToAnthropicStream {
    fn process_chunk(&mut self, chunk: &[u8]) -> Vec<Vec<u8>> {
        self.sse.push(chunk);
        let mut out: Vec<Vec<u8>> = Vec::new();
        for ev in self.sse.drain_events() {
            if ev.data == "[DONE]" {
                self.finish_into(&mut out, "end_turn");
                continue;
            }
            let json: Value = match serde_json::from_str(&ev.data) {
                Ok(v) => v,
                Err(_) => continue,
            };
            if self.model.is_empty() {
                if let Some(m) = json.get("model").and_then(|v| v.as_str()) {
                    self.model = m.to_string();
                }
            }
            let choice = json
                .get("choices")
                .and_then(|c| c.as_array())
                .and_then(|a| a.first());
            let delta = choice.and_then(|c| c.get("delta"));
            if let Some(delta) = delta {
                if let Some(reasoning) = delta.get("reasoning_content").and_then(|v| v.as_str()) {
                    if !reasoning.is_empty() {
                        self.ensure_reasoning_block(&mut out);
                        let idx = self.reasoning_index.unwrap();
                        out.push(Self::sse("content_block_delta", json!({
                            "type":"content_block_delta", "index":idx,
                            "delta":{"type":"thinking_delta","thinking":reasoning}
                        })));
                    }
                }
                if let Some(content) = delta.get("content").and_then(|v| v.as_str()) {
                    if !content.is_empty() {
                        self.ensure_text_block(&mut out);
                        let idx = self.text_index.unwrap_or(0);
                        out.push(Self::sse(
                            "content_block_delta",
                            json!({
                                "type": "content_block_delta",
                                "index": idx,
                                "delta": {"type": "text_delta", "text": content}
                            }),
                        ));
                    }
                }
                if let Some(tcs) = delta.get("tool_calls").and_then(|v| v.as_array()) {
                    for tc in tcs {
                        let tc_index = tc.get("index").and_then(|v| v.as_i64()).unwrap_or(0);
                        if !self.tool_blocks.contains_key(&tc_index) {
                            self.close_open_block(&mut out);
                            self.ensure_started(&mut out);
                            let block_idx = self.next_index;
                            self.next_index += 1;
                            self.tool_blocks.insert(tc_index, block_idx);
                            self.has_tool = true;
                            let id = tc
                                .get("id")
                                .and_then(|v| v.as_str())
                                .map(|s| s.to_string())
                                .unwrap_or_else(|| gen_id("call_"));
                            let name = tc
                                .get("function")
                                .and_then(|f| f.get("name"))
                                .and_then(|v| v.as_str())
                                .unwrap_or("")
                                .to_string();
                            out.push(Self::sse(
                                "content_block_start",
                                json!({
                                    "type": "content_block_start",
                                    "index": block_idx,
                                    "content_block": {"type": "tool_use", "id": id, "name": name, "input": {}}
                                }),
                            ));
                            self.open_block = Some(block_idx);
                        }
                        if let Some(args) = tc
                            .get("function")
                            .and_then(|f| f.get("arguments"))
                            .and_then(|v| v.as_str())
                        {
                            if !args.is_empty() {
                                let block_idx = *self.tool_blocks.get(&tc_index).unwrap();
                                out.push(Self::sse(
                                    "content_block_delta",
                                    json!({
                                        "type": "content_block_delta",
                                        "index": block_idx,
                                        "delta": {"type": "input_json_delta", "partial_json": args}
                                    }),
                                ));
                            }
                        }
                    }
                }
            }
            if let Some(fr) = choice
                .and_then(|c| c.get("finish_reason"))
                .and_then(|v| v.as_str())
            {
                let stop = finish_to_anthropic_stop(Some(fr));
                self.finish_into(&mut out, stop);
            }
        }
        out
    }

    fn finalize(&mut self) -> Vec<Vec<u8>> {
        let mut out = Vec::new();
        self.finish_into(&mut out, "end_turn");
        out
    }
}

impl ChatToAnthropicStream {
    fn finish_into(&mut self, out: &mut Vec<Vec<u8>>, stop_reason: &str) {
        if self.stopped {
            return;
        }
        self.stopped = true;
        self.ensure_started(out);
        self.close_open_block(out);
        let stop = if self.has_tool && stop_reason == "end_turn" {
            "tool_use"
        } else {
            stop_reason
        };
        out.push(Self::sse(
            "message_delta",
            json!({
                "type": "message_delta",
                "delta": {"stop_reason": stop, "stop_sequence": Value::Null},
                "usage": {"output_tokens": 0}
            }),
        ));
        out.push(Self::sse("message_stop", json!({"type": "message_stop"})));
    }
}

// ==================== ResponsesToAnthropicStream ====================

/// provider Responses 流 → 入口 Anthropic 事件流。
pub struct ResponsesToAnthropicStream {
    sse: SseBuffer,
    id: String,
    model: String,
    started: bool,
    stopped: bool,
    next_index: i64,
    open_block: Option<i64>,
    text_index: Option<i64>,
    reasoning_index: Option<i64>,
    tool_blocks: std::collections::HashMap<String, i64>,
    has_tool: bool,
}

impl ResponsesToAnthropicStream {
    pub fn new() -> Self {
        Self {
            sse: SseBuffer::new(),
            id: gen_id("msg_"),
            model: String::new(),
            started: false,
            stopped: false,
            next_index: 0,
            open_block: None,
            text_index: None,
            reasoning_index: None,
            tool_blocks: std::collections::HashMap::new(),
            has_tool: false,
        }
    }

    fn sse(event: &str, data: Value) -> Vec<u8> {
        format!(
            "event: {}\ndata: {}\n\n",
            event,
            serde_json::to_string(&data).unwrap_or_default()
        )
        .into_bytes()
    }

    fn ensure_started(&mut self, out: &mut Vec<Vec<u8>>) {
        if self.started {
            return;
        }
        self.started = true;
        out.push(Self::sse(
            "message_start",
            json!({
                "type": "message_start",
                "message": {
                    "id": self.id,
                    "type": "message",
                    "role": "assistant",
                    "model": self.model,
                    "content": [],
                    "stop_reason": Value::Null,
                    "usage": {"input_tokens": 0, "output_tokens": 0}
                }
            }),
        ));
    }

    fn close_open_block(&mut self, out: &mut Vec<Vec<u8>>) {
        if let Some(idx) = self.open_block.take() {
            out.push(Self::sse(
                "content_block_stop",
                json!({"type": "content_block_stop", "index": idx}),
            ));
        }
    }

    fn ensure_text_block(&mut self, out: &mut Vec<Vec<u8>>) {
        self.ensure_started(out);
        if self.open_block.is_some() && self.open_block == self.text_index {
            return;
        }
        self.close_open_block(out);
        let idx = match self.text_index {
            Some(i) => i,
            None => {
                let i = self.next_index;
                self.next_index += 1;
                self.text_index = Some(i);
                i
            }
        };
        out.push(Self::sse(
            "content_block_start",
            json!({
                "type": "content_block_start",
                "index": idx,
                "content_block": {"type": "text", "text": ""}
            }),
        ));
        self.open_block = Some(idx);
    }

    fn ensure_reasoning_block(&mut self, out: &mut Vec<Vec<u8>>) {
        self.ensure_started(out);
        if let Some(idx) = self.reasoning_index {
            if self.open_block == Some(idx) { return; }
        }
        self.close_open_block(out);
        let idx = self.reasoning_index.unwrap_or_else(|| {
            let i = self.next_index;
            self.next_index += 1;
            self.reasoning_index = Some(i);
            i
        });
        out.push(Self::sse("content_block_start", json!({
            "type":"content_block_start", "index":idx,
            "content_block":{"type":"thinking","thinking":""}
        })));
        self.open_block = Some(idx);
    }

    fn finish_into(&mut self, out: &mut Vec<Vec<u8>>, stop_reason: &str) {
        if self.stopped {
            return;
        }
        self.stopped = true;
        self.ensure_started(out);
        self.close_open_block(out);
        let stop = if self.has_tool && stop_reason == "end_turn" {
            "tool_use"
        } else {
            stop_reason
        };
        out.push(Self::sse(
            "message_delta",
            json!({
                "type": "message_delta",
                "delta": {"stop_reason": stop, "stop_sequence": Value::Null},
                "usage": {"output_tokens": 0}
            }),
        ));
        out.push(Self::sse("message_stop", json!({"type": "message_stop"})));
    }
}

impl Default for ResponsesToAnthropicStream {
    fn default() -> Self {
        Self::new()
    }
}

impl StreamConverter for ResponsesToAnthropicStream {
    fn process_chunk(&mut self, chunk: &[u8]) -> Vec<Vec<u8>> {
        self.sse.push(chunk);
        let mut out: Vec<Vec<u8>> = Vec::new();
        for ev in self.sse.drain_events() {
            if ev.data == "[DONE]" {
                continue;
            }
            let json: Value = match serde_json::from_str(&ev.data) {
                Ok(v) => v,
                Err(_) => continue,
            };
            let etype = json
                .get("type")
                .and_then(|v| v.as_str())
                .or(ev.event.as_deref())
                .unwrap_or("");
            if self.model.is_empty() {
                if let Some(m) = json
                    .get("response")
                    .and_then(|r| r.get("model"))
                    .and_then(|v| v.as_str())
                {
                    self.model = m.to_string();
                }
            }
            match etype {
                "response.created" | "response.in_progress" => {
                    self.ensure_started(&mut out);
                }
                "response.reasoning_summary_text.delta" | "response.reasoning_text.delta" => {
                    if let Some(d) = json.get("delta").and_then(|v| v.as_str()) {
                        self.ensure_reasoning_block(&mut out);
                        let idx = self.reasoning_index.unwrap();
                        out.push(Self::sse("content_block_delta", json!({
                            "type":"content_block_delta", "index":idx,
                            "delta":{"type":"thinking_delta","thinking":d}
                        })));
                    }
                }
                "response.output_text.delta" => {
                    if let Some(d) = json.get("delta").and_then(|v| v.as_str()) {
                        self.ensure_text_block(&mut out);
                        let idx = self.text_index.unwrap_or(0);
                        out.push(Self::sse(
                            "content_block_delta",
                            json!({
                                "type": "content_block_delta",
                                "index": idx,
                                "delta": {"type": "text_delta", "text": d}
                            }),
                        ));
                    }
                }
                "response.output_item.added" => {
                    let item = json.get("item");
                    let itype = item
                        .and_then(|i| i.get("type"))
                        .and_then(|v| v.as_str())
                        .unwrap_or("");
                    if itype == "function_call" {
                        self.close_open_block(&mut out);
                        self.ensure_started(&mut out);
                        let call_id = item
                            .and_then(|i| i.get("call_id").or_else(|| i.get("id")))
                            .and_then(|v| v.as_str())
                            .map(|s| s.to_string())
                            .unwrap_or_else(|| gen_id("call_"));
                        let name = item
                            .and_then(|i| i.get("name"))
                            .and_then(|v| v.as_str())
                            .unwrap_or("")
                            .to_string();
                        let block_idx = self.next_index;
                        self.next_index += 1;
                        self.tool_blocks.insert(call_id.clone(), block_idx);
                        if let Some(item_id) = item.and_then(|i| i.get("id")).and_then(|v| v.as_str()) {
                            self.tool_blocks.insert(item_id.to_string(), block_idx);
                        }
                        self.has_tool = true;
                        out.push(Self::sse(
                            "content_block_start",
                            json!({
                                "type": "content_block_start",
                                "index": block_idx,
                                "content_block": {"type": "tool_use", "id": call_id, "name": name, "input": {}}
                            }),
                        ));
                        self.open_block = Some(block_idx);
                    }
                }
                "response.function_call_arguments.delta" => {
                    let item_id = json.get("item_id").and_then(|v| v.as_str()).unwrap_or("");
                    if let Some(&block_idx) = self.tool_blocks.get(item_id) {
                        if let Some(d) = json.get("delta").and_then(|v| v.as_str()) {
                            out.push(Self::sse(
                                "content_block_delta",
                                json!({
                                    "type": "content_block_delta",
                                    "index": block_idx,
                                    "delta": {"type": "input_json_delta", "partial_json": d}
                                }),
                            ));
                        }
                    }
                }
                "response.completed" => self.finish_into(&mut out, "end_turn"),
                "response.incomplete" => self.finish_into(&mut out, "max_tokens"),
                "response.failed" => self.finish_into(&mut out, "end_turn"),
                _ => {}
            }
        }
        out
    }

    fn finalize(&mut self) -> Vec<Vec<u8>> {
        let mut out = Vec::new();
        self.finish_into(&mut out, "end_turn");
        out
    }
}

// ==================== PassthroughStream ====================

/// 原样返回输入字节（工厂兜底，正常不会走到）。
pub struct PassthroughStream;

impl PassthroughStream {
    pub fn new() -> Self {
        Self
    }
}

impl Default for PassthroughStream {
    fn default() -> Self {
        Self::new()
    }
}

impl StreamConverter for PassthroughStream {
    fn process_chunk(&mut self, chunk: &[u8]) -> Vec<Vec<u8>> {
        vec![chunk.to_vec()]
    }
}

// ==================== 非流式响应降级重放为 SSE 流 ====================

/// 当上游对流式请求返回了非 SSE 的完整 JSON 时的降级路径：
/// 把一个**已转成入口格式**的完整响应对象，组装成该入口格式的完整 SSE 事件流字节。
/// entry 为入口格式（调用方期望的输出格式）。
pub fn replay_response_as_sse(entry: ApiFormat, full_response: &Value) -> Vec<u8> {
    match entry {
        ApiFormat::OpenAIResponses => replay_responses(full_response),
        ApiFormat::OpenAIChat => replay_chat(full_response),
        ApiFormat::Anthropic => replay_anthropic(full_response),
    }
}

fn sse_line(event: &str, data: &Value) -> String {
    format!(
        "event: {}\ndata: {}\n\n",
        event,
        serde_json::to_string(data).unwrap_or_default()
    )
}

/// 入口 Responses：full_response 是 Responses 格式（object=response, output=[...]）。
fn replay_responses(resp: &Value) -> Vec<u8> {
    let mut seq: u64 = 0;
    let mut out = String::new();
    let mut emit = |event: &str, mut data: Value, seq: &mut u64| {
        if let Some(m) = data.as_object_mut() {
            m.insert("type".to_string(), json!(event));
            m.insert("sequence_number".to_string(), json!(*seq));
        }
        *seq += 1;
        out.push_str(&sse_line(event, &data));
    };

    let response_id = resp
        .get("id")
        .and_then(|v| v.as_str())
        .map(|s| s.to_string())
        .unwrap_or_else(|| gen_id("resp_"));
    let model = resp.get("model").and_then(|v| v.as_str()).unwrap_or("");
    let status = resp
        .get("status")
        .and_then(|v| v.as_str())
        .unwrap_or("completed");

    let in_progress = json!({
        "response": {
            "id": response_id, "object": "response", "created_at": 0,
            "model": model, "status": "in_progress", "output": []
        }
    });
    emit("response.created", in_progress.clone(), &mut seq);
    emit("response.in_progress", in_progress, &mut seq);

    let empty: Vec<Value> = Vec::new();
    let output = resp
        .get("output")
        .and_then(|v| v.as_array())
        .unwrap_or(&empty);
    let mut output_index: i64 = 0;
    for item in output {
        let item_id = item
            .get("id")
            .and_then(|v| v.as_str())
            .map(|s| s.to_string())
            .unwrap_or_else(|| gen_id("msg_"));
        let item_type = item
            .get("type")
            .and_then(|v| v.as_str())
            .unwrap_or("message");

        if item_type == "message" {
            emit(
                "response.output_item.added",
                json!({
                    "output_index": output_index,
                    "item": {"type": "message", "id": item_id, "status": "in_progress", "role": "assistant", "content": []}
                }),
                &mut seq,
            );
            let parts = item
                .get("content")
                .and_then(|v| v.as_array())
                .cloned()
                .unwrap_or_default();
            for (content_index, part) in parts.iter().enumerate() {
                let ci = content_index as i64;
                let text = part.get("text").and_then(|v| v.as_str()).unwrap_or("");
                emit(
                    "response.content_part.added",
                    json!({"item_id": item_id, "output_index": output_index, "content_index": ci, "part": part}),
                    &mut seq,
                );
                if !text.is_empty() {
                    emit(
                        "response.output_text.delta",
                        json!({"item_id": item_id, "output_index": output_index, "content_index": ci, "delta": text}),
                        &mut seq,
                    );
                }
                emit(
                    "response.output_text.done",
                    json!({"item_id": item_id, "output_index": output_index, "content_index": ci, "text": text}),
                    &mut seq,
                );
                emit(
                    "response.content_part.done",
                    json!({"item_id": item_id, "output_index": output_index, "content_index": ci, "part": part}),
                    &mut seq,
                );
            }
            emit(
                "response.output_item.done",
                json!({"output_index": output_index, "item": item}),
                &mut seq,
            );
        } else {
            // function_call 等其它 item：整体作为 output_item 添加/完成
            emit(
                "response.output_item.added",
                json!({"output_index": output_index, "item": item}),
                &mut seq,
            );
            emit(
                "response.output_item.done",
                json!({"output_index": output_index, "item": item}),
                &mut seq,
            );
        }
        output_index += 1;
    }

    let mut completed = json!({
        "id": response_id, "object": "response", "created_at": 0,
        "model": model, "status": status, "output": output
    });
    if let Some(u) = resp.get("usage") {
        completed["usage"] = u.clone();
    }
    emit(
        "response.completed",
        json!({ "response": completed }),
        &mut seq,
    );

    out.into_bytes()
}

/// 入口 Chat：full_response 是 Chat 格式（choices[0].message）。
fn replay_chat(resp: &Value) -> Vec<u8> {
    let mut out = String::new();
    let id = resp
        .get("id")
        .and_then(|v| v.as_str())
        .map(|s| s.to_string())
        .unwrap_or_else(|| gen_id("chatcmpl-"));
    let model = resp.get("model").and_then(|v| v.as_str()).unwrap_or("");
    let created = resp.get("created").and_then(|v| v.as_i64()).unwrap_or(0);

    let choice = resp
        .get("choices")
        .and_then(|c| c.as_array())
        .and_then(|a| a.first());
    let message = choice.and_then(|c| c.get("message"));
    let finish_reason = choice
        .and_then(|c| c.get("finish_reason"))
        .and_then(|v| v.as_str())
        .unwrap_or("stop");

    let base = |delta: Value, fr: Value| -> Value {
        json!({
            "id": id, "object": "chat.completion.chunk", "created": created, "model": model,
            "choices": [{"index": 0, "delta": delta, "finish_reason": fr}]
        })
    };

    // role chunk
    out.push_str(&sse_line(
        "",
        &base(json!({"role": "assistant"}), Value::Null),
    ));

    if let Some(msg) = message {
        if let Some(rc) = msg.get("reasoning_content").and_then(|v| v.as_str()) {
            if !rc.is_empty() {
                out.push_str(&sse_line(
                    "",
                    &base(json!({"reasoning_content": rc}), Value::Null),
                ));
            }
        }
        if let Some(content) = msg.get("content").and_then(|v| v.as_str()) {
            if !content.is_empty() {
                out.push_str(&sse_line(
                    "",
                    &base(json!({"content": content}), Value::Null),
                ));
            }
        }
        if let Some(tool_calls) = msg.get("tool_calls").and_then(|v| v.as_array()) {
            if !tool_calls.is_empty() {
                out.push_str(&sse_line(
                    "",
                    &base(json!({"tool_calls": tool_calls}), Value::Null),
                ));
            }
        }
    }

    // finish chunk
    out.push_str(&sse_line("", &base(json!({}), json!(finish_reason))));
    out.push_str("data: [DONE]\n\n");
    out.into_bytes()
}

/// 入口 Anthropic：full_response 是 Anthropic 格式（type=message, content=[...]）。
fn replay_anthropic(resp: &Value) -> Vec<u8> {
    let mut out = String::new();
    let id = resp
        .get("id")
        .and_then(|v| v.as_str())
        .map(|s| s.to_string())
        .unwrap_or_else(|| gen_id("msg_"));
    let model = resp.get("model").and_then(|v| v.as_str()).unwrap_or("");
    let stop_reason = resp
        .get("stop_reason")
        .and_then(|v| v.as_str())
        .unwrap_or("end_turn");

    let msg_start = json!({
        "type": "message_start",
        "message": {
            "id": id, "type": "message", "role": "assistant", "model": model,
            "content": [], "stop_reason": Value::Null, "stop_sequence": Value::Null,
            "usage": resp.get("usage").cloned().unwrap_or_else(|| json!({"input_tokens": 0, "output_tokens": 0}))
        }
    });
    out.push_str(&sse_line("message_start", &msg_start));

    let empty: Vec<Value> = Vec::new();
    let content = resp
        .get("content")
        .and_then(|v| v.as_array())
        .unwrap_or(&empty);
    for (index, block) in content.iter().enumerate() {
        let idx = index as i64;
        let btype = block.get("type").and_then(|v| v.as_str()).unwrap_or("text");
        match btype {
            "text" => {
                let text = block.get("text").and_then(|v| v.as_str()).unwrap_or("");
                out.push_str(&sse_line(
                    "content_block_start",
                    &json!({"type": "content_block_start", "index": idx, "content_block": {"type": "text", "text": ""}}),
                ));
                if !text.is_empty() {
                    out.push_str(&sse_line(
                        "content_block_delta",
                        &json!({"type": "content_block_delta", "index": idx, "delta": {"type": "text_delta", "text": text}}),
                    ));
                }
                out.push_str(&sse_line(
                    "content_block_stop",
                    &json!({"type": "content_block_stop", "index": idx}),
                ));
            }
            "thinking" => {
                let thinking = block.get("thinking").and_then(|v| v.as_str()).unwrap_or("");
                out.push_str(&sse_line(
                    "content_block_start",
                    &json!({"type": "content_block_start", "index": idx, "content_block": {"type": "thinking", "thinking": ""}}),
                ));
                if !thinking.is_empty() {
                    out.push_str(&sse_line(
                        "content_block_delta",
                        &json!({"type": "content_block_delta", "index": idx, "delta": {"type": "thinking_delta", "thinking": thinking}}),
                    ));
                }
                out.push_str(&sse_line(
                    "content_block_stop",
                    &json!({"type": "content_block_stop", "index": idx}),
                ));
            }
            "tool_use" => {
                out.push_str(&sse_line(
                    "content_block_start",
                    &json!({"type": "content_block_start", "index": idx, "content_block": block}),
                ));
                if let Some(input) = block.get("input") {
                    let pj = serde_json::to_string(input).unwrap_or_else(|_| "{}".to_string());
                    out.push_str(&sse_line(
                        "content_block_delta",
                        &json!({"type": "content_block_delta", "index": idx, "delta": {"type": "input_json_delta", "partial_json": pj}}),
                    ));
                }
                out.push_str(&sse_line(
                    "content_block_stop",
                    &json!({"type": "content_block_stop", "index": idx}),
                ));
            }
            _ => {
                out.push_str(&sse_line(
                    "content_block_start",
                    &json!({"type": "content_block_start", "index": idx, "content_block": block}),
                ));
                out.push_str(&sse_line(
                    "content_block_stop",
                    &json!({"type": "content_block_stop", "index": idx}),
                ));
            }
        }
    }

    let mut msg_delta = json!({
        "type": "message_delta",
        "delta": {"stop_reason": stop_reason, "stop_sequence": Value::Null}
    });
    if let Some(u) = resp.get("usage") {
        msg_delta["usage"] = u.clone();
    }
    out.push_str(&sse_line("message_delta", &msg_delta));
    out.push_str(&sse_line("message_stop", &json!({"type": "message_stop"})));
    out.into_bytes()
}
