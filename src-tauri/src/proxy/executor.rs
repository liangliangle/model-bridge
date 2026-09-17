//! 渠道执行器：负责单个渠道的请求处理。
//! 格式已在上游路由层保证匹配，此处只做透传：替换 model、处理 header、发送请求。

use axum::response::Response;
use axum::http::StatusCode;
use futures::stream::StreamExt;
use parking_lot::Mutex as ParkingMutex;
use serde_json::Value;
use std::sync::Arc;
use std::time::Instant;

use crate::channel::config::{AppConfig, ChannelConfig, ProviderType};
use crate::provider::{collect_forwarded_headers, headers_to_json};
use crate::proxy::context::RequestContext;
use crate::proxy::server::AppState;

/// 在指定渠道上执行请求，返回 HTTP Response 或错误字符串
pub async fn execute_on_channel(
    state: &Arc<AppState>,
    ctx: &RequestContext,
    channel: &ChannelConfig,
    actual_model: &str,
    audit_id: i64,
) -> Result<Response, String> {
    let start = Instant::now();
    let url = &channel.endpoint.url;
    let debug = state.config.read().debug;

    if debug {
        tracing::debug!(
            audit_id,
            provider = ?channel.provider,
            channel_id = %channel.id,
            model_alias = %ctx.model_alias,
            actual_model,
            is_stream = ctx.is_stream,
            "[DEBUG] >>> Incoming request (passthrough)"
        );
        tracing::debug!(audit_id, "[DEBUG] >>> Original body:\n{}", pretty_json(&ctx.raw_body));
    }

    // 计算格式转换器：格式一致 → None（透传），受支持的跨协议 → Some(fc)。
    // 路由层已按协议矩阵过滤掉不可服务的渠道，这里报错属于兜底：
    // 宁可返回明确错误，也不能退化成把错误协议发给上游。
    let converter = match crate::converter::FormatConverter::plan(ctx.input_format, &channel.provider) {
        Ok(converter) => converter,
        Err(unsupported) => return Err(format!("invalid_request: {unsupported}")),
    };

    let mut send_body_str = match &converter {
        None => {
            // 透传：只替换 model 字段，保留原始格式
            replace_model_in_json_str(&ctx.raw_body, actual_model)
        }
        Some(fc) => {
            if fc.from_format() == crate::converter::ApiFormat::OpenAIResponses {
                if let Err(message) = crate::converter::request::validate_responses_cross_format(&ctx.parsed_body) {
                    return Err(format!("invalid_request: {}", message));
                }
            }
            // 跨格式：入口格式 → Chat → 目标 provider 格式
            let mut converted = fc.convert_request(&ctx.parsed_body, actual_model);
            // Chat 目标的流式请求必须显式要求上游下发 usage：OpenAI Chat 默认不在
            // 流里带 usage，不开启则流式请求的 token 审计与下发 usage 都会归零。
            // 仅转换路径注入；透传路径保持客户端原始请求体不变。
            if ctx.is_stream && fc.to_format() == crate::converter::ApiFormat::OpenAIChat {
                ensure_chat_stream_usage(&mut converted);
            }
            let s = serde_json::to_string(&converted).unwrap_or_else(|_| ctx.raw_body.clone());
            if debug {
                tracing::debug!(audit_id, "[DEBUG] >>> Converted body ({:?} → {:?}):\n{}",
                    fc.from_format(), fc.to_format(), pretty_json(&s));
            }
            s
        }
    };

    // 如果开启了 strip_thinking，从请求体中移除 thinking 相关内容块
    if channel.strip_thinking {
        send_body_str = strip_thinking_blocks(&send_body_str);
    }

    // Anthropic 渠道：移除 temperature
    if channel.provider == ProviderType::Anthropic {
        send_body_str = sanitize_anthropic_body_str(&send_body_str);
        // 自动注入 prompt caching 断点（请求中无任何 cache_control 时才注入）
        if channel.auto_cache {
            send_body_str = inject_anthropic_cache_str(&send_body_str);
        }
    }

    // 如果配置了 force_effort，强制覆盖 output_config.effort
    if let Some(effort) = &channel.force_effort {
        send_body_str = force_effort_in_json_str(&send_body_str, effort);
    }

    // 转换路径下 convert_request 会丢弃 stream 字段，按入口的流式意图补回
    if converter.is_some() {
        send_body_str = set_stream_in_json_str(&send_body_str, ctx.is_stream);
    }

    // 构建转发请求
    let timeout = if ctx.is_stream {
        std::time::Duration::from_secs(1800)
    } else {
        std::time::Duration::from_millis(channel.timeout_ms)
    };
    let mut builder = state.http_client.post(url)
        .timeout(timeout)
        .body(send_body_str.clone());

    // 用 HashMap 收集 headers，按优先级从低到高 insert（后写覆盖前写）
    let mut headers = std::collections::HashMap::<String, String>::new();

    // 1. 默认 header
    headers.insert("content-type".to_string(), "application/json".to_string());

    // 2. 透传调用方 headers + custom_headers
    collect_forwarded_headers(&mut headers, &ctx.original_headers, channel);

    // 3. Provider 认证头（优先级高于透传和 custom_headers）
    match channel.provider {
        ProviderType::Anthropic => {
            headers.insert("x-api-key".to_string(), channel.api_key.clone());
            headers.insert("anthropic-version".to_string(), "2023-06-01".to_string());
        }
        _ => {
            if !channel.api_key.is_empty() {
                headers.insert("authorization".to_string(), format!("Bearer {}", channel.api_key));
            }
        }
    }

    // 一次性写入 builder
    for (k, v) in &headers {
        builder = builder.header(k.as_str(), v.as_str());
    }

    // 构建 Request 对象，提取实际请求头
    let request = builder.build().map_err(|e| format!("Build request failed: {}", e))?;
    let req_headers_json = headers_to_json(request.headers());

    if debug {
        tracing::debug!(audit_id, "[DEBUG] >>> Forwarding to: {}", url);
        tracing::debug!(audit_id, "[DEBUG] >>> Request headers: {}", req_headers_json);
    }

    // 更新审计：转发请求头 + 转发请求体
    let _ = state.audit_db.update_forwarded_request(
        audit_id,
        Some(&req_headers_json),
        Some(&send_body_str),
    );

    // 发送请求
    let resp = state.http_client.execute(request).await.map_err(|e| {
        if e.is_timeout() { "timeout".to_string() }
        else { format!("network_error: {}", e) }
    })?;

    let status = resp.status().as_u16();
    let resp_headers_json = headers_to_json(resp.headers());
    let upstream_content_type = resp
        .headers()
        .get(reqwest::header::CONTENT_TYPE)
        .and_then(|v| v.to_str().ok())
        .unwrap_or("")
        .to_lowercase();

    if debug {
        tracing::debug!(
            audit_id, status,
            latency_ms = start.elapsed().as_millis() as u64,
            "[DEBUG] <<< Upstream response"
        );
        tracing::debug!(audit_id, "[DEBUG] <<< Response headers: {}", resp_headers_json);
    }

    if status == 429 {
        return Err("rate_limited".to_string());
    }

    // 流式响应处理：透传字节流，或经 converter 转成入口格式的事件流
    if ctx.is_stream {
        // 降级：跨格式转换路径下，若上游对流式请求返回了非 SSE 的完整 JSON
        // （常见于带工具调用、或上游把 stream 请求当非流式处理的情况），
        // 按 \n\n 解析 SSE 的流式转换器无法处理，需读完整 body 转换后重放为 SSE。
        if let Some(fc) = converter.as_ref() {
            let is_event_stream = upstream_content_type.contains("text/event-stream");
            if !is_event_stream && status < 400 {
                let raw = resp.text().await.map_err(|e| format!("read response failed: {}", e))?;
                if let Ok(up) = serde_json::from_str::<Value>(&raw) {
                    let sse_bytes = fc.full_response_to_sse(&up, actual_model);
                    // 审计记录转换后的完整结果对象（而非逐条 SSE 事件）
                    let converted_obj = fc.convert_response(&up, actual_model);
                    let converted_text = serde_json::to_string_pretty(&converted_obj)
                        .unwrap_or_else(|_| String::from_utf8_lossy(&sse_bytes).to_string());
                    let latency = start.elapsed().as_millis() as u64;
                    let tokens = crate::audit::db::extract_tokens_from_json(&up);
                    let cost = compute_cost_from_config(&state, actual_model, &tokens);
                    let _ = state.audit_db.update_first_byte(
                        audit_id, latency,
                        Some(&resp_headers_json), Some(&resp_headers_json),
                    );
                    let _ = state.audit_db.update_response(
                        audit_id, status, latency,
                        Some(&resp_headers_json), Some(&raw),
                        Some(&resp_headers_json), Some(&converted_text),
                        &tokens, cost,
                    );
                    if debug {
                        tracing::debug!(audit_id,
                            "[DEBUG] <<< Non-SSE upstream on stream request; replayed as {:?} SSE ({} bytes)",
                            fc.from_format(), sse_bytes.len());
                    }
                    return Ok(axum::response::Response::builder()
                        .status(200)
                        .header("Content-Type", "text/event-stream")
                        .header("Cache-Control", "no-cache")
                        .header("Connection", "keep-alive")
                        .body(axum::body::Body::from(sse_bytes))
                        .unwrap());
                }
                // 解析失败：把已读取的原始文本作为单块透传返回
                return Ok(axum::response::Response::builder()
                    .status(StatusCode::from_u16(status).unwrap_or(StatusCode::OK))
                    .header("Content-Type", "text/event-stream")
                    .header("Cache-Control", "no-cache")
                    .header("Connection", "keep-alive")
                    .body(axum::body::Body::from(raw))
                    .unwrap());
            }
        }

        let first_byte = start.elapsed().as_millis() as u64;
        let _ = state.audit_db.update_first_byte(
            audit_id, first_byte,
            Some(&resp_headers_json), Some(&resp_headers_json),
        );

        let audit_db = state.audit_db.clone();
        let mut stream = Box::pin(resp.bytes_stream());

        // 缓冲首个 chunk，检测 SSE 错误事件（在转发给客户端之前）
        let first_chunk: Option<bytes::Bytes> = match tokio::time::timeout(
            std::time::Duration::from_secs(30),
            stream.next(),
        ).await {
            Ok(Some(Ok(bytes))) => Some(bytes),
            Ok(Some(Err(e))) => {
                return Err(format!("network_error: {}", e));
            }
            Ok(None) => None,
            Err(_) => {
                return Err("timeout".to_string());
            }
        };

        // 检测首个 chunk 中的错误事件
        if let Some(ref chunk) = first_chunk {
            if let Some(error_msg) = detect_sse_error_in_prefix(chunk) {
                // 确认没有正常内容输出才重试
                if !sse_has_content_output(chunk) {
                    tracing::info!(
                        audit_id,
                        error = %error_msg,
                        "Stream error detected in first chunk, triggering retry"
                    );
                    let _ = state.audit_db.update_error(
                        audit_id, 502, start.elapsed().as_millis() as u64,
                        &format!("stream_error: {}", error_msg),
                    );
                    return Err(format!("stream_error: {}", error_msg));
                }
            }
        }

        let collected = Arc::new(ParkingMutex::new(Vec::<u8>::new()));
        let collected_clone = collected.clone();
        // 收集转换后下发给客户端的字节，用于审计记录「转发响应」（转换后）
        let forwarded = Arc::new(ParkingMutex::new(Vec::<u8>::new()));
        let forwarded_clone = forwarded.clone();

        let (done_tx, done_rx) = tokio::sync::oneshot::channel::<()>();
        let done_tx = Arc::new(ParkingMutex::new(Some(done_tx)));
        let done_tx_clone = done_tx.clone();

        let idle_timeout = std::time::Duration::from_secs(300);
        let collected_for_audit = collected.clone();
        let forwarded_for_audit = forwarded.clone();

        if debug {
            tracing::debug!(audit_id, "[DEBUG] <<< Streaming response started");
        }

        // 转换路径下构造流式转换器；透传路径为 None
        let stream_conv: Option<Box<dyn crate::converter::stream::StreamConverter>> =
            converter.as_ref().map(|fc| fc.create_stream_converter());

        // 降级重放所需：转换器副本与模型名，供上游发来非 SSE 完整 JSON 时补救
        let has_converter = converter.is_some();
        let fc_copy = converter.clone();
        let actual_model_owned = actual_model.to_string();

        let stream_debug = debug;
        let stream_audit_id = audit_id;
        let byte_stream = futures::stream::unfold(
            (stream, idle_timeout, collected_clone, done_tx_clone, stream_conv, false, false, fc_copy, actual_model_owned, first_chunk),
            move |(mut stream, timeout, collected, done_tx, mut stream_conv, ended, mut produced_any, fc_copy, actual_model_owned, pending_chunk)| {
                let debug = stream_debug;
                let audit_id = stream_audit_id;
                let forwarded = forwarded_clone.clone();
                async move {
                if ended {
                    return None;
                }

                // 如果有缓冲的首个 chunk，先输出它
                if let Some(chunk) = pending_chunk {
                    collected.lock().extend_from_slice(&chunk);

                    if debug {
                        let raw_text = String::from_utf8_lossy(&chunk);
                        tracing::debug!(audit_id, "[DEBUG] <<< Stream chunk ({} bytes, buffered first):\n{}", chunk.len(), raw_text);
                    }

                    let out_bytes = match stream_conv.as_mut() {
                        Some(sc) => {
                            let mut merged = Vec::<u8>::new();
                            for part in sc.process_chunk(&chunk) {
                                merged.extend_from_slice(&part);
                            }
                            bytes::Bytes::from(merged)
                        }
                        None => chunk,
                    };
                    if !out_bytes.is_empty() {
                        produced_any = true;
                    }
                    forwarded.lock().extend_from_slice(&out_bytes);
                    return Some((Ok(out_bytes), (stream, timeout, collected, done_tx, stream_conv, false, produced_any, fc_copy, actual_model_owned, None)));
                }

                match tokio::time::timeout(timeout, stream.next()).await {
                    Ok(Some(Ok(bytes))) => {
                        // 审计始终收集上游原始字节
                        collected.lock().extend_from_slice(&bytes);

                        if debug {
                            let raw_text = String::from_utf8_lossy(&bytes);
                            tracing::debug!(audit_id, "[DEBUG] <<< Stream chunk ({} bytes):\n{}", bytes.len(), raw_text);
                        }

                        let out_bytes = match stream_conv.as_mut() {
                            Some(sc) => {
                                let mut merged = Vec::<u8>::new();
                                for part in sc.process_chunk(&bytes) {
                                    merged.extend_from_slice(&part);
                                }
                                bytes::Bytes::from(merged)
                            }
                            None => bytes,
                        };
                        if !out_bytes.is_empty() {
                            produced_any = true;
                        }
                        // 收集转换后下发给客户端的字节
                        forwarded.lock().extend_from_slice(&out_bytes);
                        Some((Ok(out_bytes), (stream, timeout, collected, done_tx, stream_conv, false, produced_any, fc_copy, actual_model_owned, None)))
                    }
                    Ok(Some(Err(e))) => {
                        if let Some(tx) = done_tx.lock().take() { let _ = tx.send(()); }
                        Some((Err(e), (stream, timeout, collected, done_tx, stream_conv, true, produced_any, fc_copy, actual_model_owned, None)))
                    }
                    Ok(None) => {
                        // 上游结束：转换路径下发收尾事件
                        if let Some(sc) = stream_conv.as_mut() {
                            let mut merged = Vec::<u8>::new();
                            for part in sc.finalize() {
                                merged.extend_from_slice(&part);
                            }
                            if !merged.is_empty() {
                                produced_any = true;
                                forwarded.lock().extend_from_slice(&merged);
                                if let Some(tx) = done_tx.lock().take() { let _ = tx.send(()); }
                                return Some((
                                    Ok(bytes::Bytes::from(merged)),
                                    (stream, timeout, collected, done_tx, stream_conv, true, produced_any, fc_copy, actual_model_owned, None),
                                ));
                            }
                        }
                        // 降级：转换路径下全程零产出，说明上游虽声明 SSE 但实际是完整 JSON。
                        // 把累积的原始字节解析为完整响应，重放为入口格式的 SSE 事件流。
                        if !produced_any {
                            if let Some(fc) = fc_copy.as_ref() {
                                let raw_bytes = collected.lock().clone();
                                if let Ok(up) = serde_json::from_slice::<Value>(&raw_bytes) {
                                    let sse = fc.full_response_to_sse(&up, &actual_model_owned);
                                    if !sse.is_empty() {
                                        if debug {
                                            tracing::debug!(audit_id,
                                                "[DEBUG] <<< Stream produced nothing; replayed non-SSE upstream as {:?} SSE ({} bytes)",
                                                fc.from_format(), sse.len());
                                        }
                                        forwarded.lock().extend_from_slice(&sse);
                                        if let Some(tx) = done_tx.lock().take() { let _ = tx.send(()); }
                                        return Some((
                                            Ok(bytes::Bytes::from(sse)),
                                            (stream, timeout, collected, done_tx, stream_conv, true, true, fc_copy, actual_model_owned, None),
                                        ));
                                    }
                                }
                            }
                        }
                        if let Some(tx) = done_tx.lock().take() { let _ = tx.send(()); }
                        None
                    }
                    Err(_) => {
                        tracing::warn!("Streaming idle timeout ({}s)", timeout.as_secs());
                        // 超时同样要让转换器收尾，否则下游会缺 message_stop / finish_reason。
                        // （Chat→Anthropic 的 usage 收尾依赖 finalize，缺了会丢结束事件。）
                        if let Some(sc) = stream_conv.as_mut() {
                            let mut merged = Vec::<u8>::new();
                            for part in sc.finalize() {
                                merged.extend_from_slice(&part);
                            }
                            if !merged.is_empty() {
                                forwarded.lock().extend_from_slice(&merged);
                                if let Some(tx) = done_tx.lock().take() { let _ = tx.send(()); }
                                return Some((
                                    Ok(bytes::Bytes::from(merged)),
                                    (stream, timeout, collected, done_tx, stream_conv, true, produced_any, fc_copy, actual_model_owned, None),
                                ));
                            }
                        }
                        if let Some(tx) = done_tx.lock().take() { let _ = tx.send(()); }
                        None
                    }
                }
            }},
        );

        // 后台：流结束后更新审计
        let stream_start = start;
        let upstream_status_for_audit = status;
        let actual_model_for_cost = actual_model.to_string();
        let config_for_cost = state.config.clone();
        tokio::spawn(async move {
            let _ = done_rx.await;
            let total_latency = stream_start.elapsed().as_millis() as u64;
            let raw_bytes = collected_for_audit.lock().clone();
            if !raw_bytes.is_empty() {
                let raw_text = String::from_utf8_lossy(&raw_bytes).to_string();
                let tokens = extract_tokens_for_stream_cost(&raw_text);
                let cost = compute_cost_from_config_ref(&config_for_cost, &actual_model_for_cost, &tokens);
                if has_converter {
                    // 转换路径：response_body 记录转换后下发内容，upstream_response_body 记录上游原始
                    let fwd_bytes = forwarded_for_audit.lock().clone();
                    let fwd_text = String::from_utf8_lossy(&fwd_bytes).to_string();
                    let _ = audit_db.update_streaming_response(audit_id, &raw_text, Some(&fwd_text), Some(upstream_status_for_audit), total_latency, cost);
                } else {
                    // 透传路径：无转换，两者一致
                    let _ = audit_db.update_streaming_response(audit_id, &raw_text, None, Some(upstream_status_for_audit), total_latency, cost);
                }
            }
        });

        let body_stream = axum::body::Body::from_stream(byte_stream);

        return Ok(axum::response::Response::builder()
            .status(200)
            .header("Content-Type", "text/event-stream")
            .header("Cache-Control", "no-cache")
            .header("Connection", "keep-alive")
            .body(body_stream)
            .unwrap());
    }

    // 非流式响应处理：直接透传
    let raw_resp_text = resp.text().await.map_err(|e| format!("read response failed: {}", e))?;

    if debug {
        tracing::debug!(audit_id, "[DEBUG] <<< Upstream response body:\n{}", pretty_json(&raw_resp_text));
    }

    if status >= 500 {
        let preview = &raw_resp_text[..raw_resp_text.len().min(200)];
        return Err(format!("server_error_{}: {}", status, preview));
    }

    if status >= 400 {
        let msg = serde_json::from_str::<Value>(&raw_resp_text).ok()
            .and_then(|v| v.get("error")?.get("message")?.as_str().map(|s| s.to_string()))
            .unwrap_or_else(|| raw_resp_text[..raw_resp_text.len().min(200)].to_string());
        return Err(format!("HTTP {} — {}", status, msg));
    }

    let tokens = serde_json::from_str::<Value>(&raw_resp_text).ok()
        .map(|v| crate::audit::db::extract_tokens_from_json(&v))
        .unwrap_or_default();
    let cost = compute_cost_from_config(&state, actual_model, &tokens);

    // 转换路径：把上游响应转回入口格式；透传路径原样返回
    let response_body_str = match &converter {
        None => raw_resp_text.clone(),
        Some(fc) => match serde_json::from_str::<Value>(&raw_resp_text) {
            Ok(up) => {
                let converted = fc.convert_response(&up, actual_model);
                let s = serde_json::to_string(&converted).unwrap_or_else(|_| raw_resp_text.clone());
                if debug {
                    tracing::debug!(audit_id, "[DEBUG] <<< Converted response ({:?} → {:?}):\n{}",
                        fc.to_format(), fc.from_format(), pretty_json(&s));
                }
                s
            }
            Err(_) => raw_resp_text.clone(),
        },
    };

    let _ = state.audit_db.update_response(
        audit_id, status, start.elapsed().as_millis() as u64,
        Some(&resp_headers_json), Some(&raw_resp_text),
        Some(&resp_headers_json), Some(&response_body_str),
        &tokens, cost,
    );

    Ok(axum::response::Response::builder()
        .status(StatusCode::from_u16(status).unwrap_or(StatusCode::OK))
        .header("Content-Type", "application/json")
        .body(axum::body::Body::from(response_body_str))
        .unwrap())
}

// ==================== 流式错误检测 ====================

/// 检测 SSE 字节前缀中是否包含错误事件。
/// 返回 Some(error_message) 表示检测到错误，None 表示无错误。
///
/// 支持的格式：
/// - SSE `event:error` 行（DashScope/Qwen 等）
/// - Anthropic `{"type":"error", ...}` data 事件
/// - OpenAI `{"error": {...}}` data 事件
pub fn detect_sse_error_in_prefix(bytes: &[u8]) -> Option<String> {
    let text = String::from_utf8_lossy(bytes);
    let mut has_event_error = false;

    for line in text.lines() {
        let trimmed = line.trim();

        // 检测 SSE event:error 行
        if trimmed.eq_ignore_ascii_case("event:error") || trimmed.eq_ignore_ascii_case("event: error") {
            has_event_error = true;
            continue;
        }

        // 解析 data: 行
        let data = trimmed.strip_prefix("data: ").or_else(|| trimmed.strip_prefix("data:"));
        let data = match data {
            Some(d) => d.trim(),
            None => continue,
        };

        if data == "[DONE]" || data.is_empty() {
            continue;
        }

        // 尝试解析 JSON
        if let Ok(json) = serde_json::from_str::<serde_json::Value>(data) {
            // OpenAI 格式: {"error": {"message": "...", "type": "..."}}
            if let Some(error) = json.get("error") {
                if error.is_object() {
                    let msg = error.get("message")
                        .and_then(|m| m.as_str())
                        .unwrap_or("Unknown stream error")
                        .to_string();
                    return Some(msg);
                }
            }

            // Anthropic 格式: {"type": "error", "error": {"type": "...", "message": "..."}}
            if json.get("type").and_then(|t| t.as_str()) == Some("error") {
                let msg = json.get("error")
                    .and_then(|e| e.get("message"))
                    .and_then(|m| m.as_str())
                    .unwrap_or("Unknown stream error")
                    .to_string();
                return Some(msg);
            }
        }
    }

    // 如果有 event:error 行但 data 不是标准 JSON，仍视为错误
    if has_event_error {
        // 尝试从 data 行提取原始文本作为错误信息
        for line in text.lines() {
            let trimmed = line.trim();
            if let Some(data) = trimmed.strip_prefix("data: ").or_else(|| trimmed.strip_prefix("data:")) {
                let data = data.trim();
                if !data.is_empty() && data != "[DONE]" {
                    return Some(format!("SSE error event: {}", data));
                }
            }
        }
        return Some("SSE error event detected".to_string());
    }

    None
}

/// 检测 SSE 字节中是否包含正常内容输出（content delta）。
/// 用于判断是否已经向客户端产出了有效内容。
pub fn sse_has_content_output(bytes: &[u8]) -> bool {
    let text = String::from_utf8_lossy(bytes);

    for line in text.lines() {
        let trimmed = line.trim();
        let data = match trimmed.strip_prefix("data: ").or_else(|| trimmed.strip_prefix("data:")) {
            Some(d) => d.trim(),
            None => continue,
        };

        if data == "[DONE]" || data.is_empty() {
            continue;
        }

        if let Ok(json) = serde_json::from_str::<serde_json::Value>(data) {
            // OpenAI: choices[].delta.content 存在
            if let Some(choices) = json.get("choices").and_then(|c| c.as_array()) {
                for choice in choices {
                    if choice.get("delta")
                        .and_then(|d| d.get("content"))
                        .and_then(|c| c.as_str())
                        .map(|s| !s.is_empty())
                        .unwrap_or(false)
                    {
                        return true;
                    }
                }
            }

            // Anthropic: type == "content_block_delta"
            if json.get("type").and_then(|t| t.as_str()) == Some("content_block_delta") {
                return true;
            }
        }
    }

    false
}

// ==================== 工具函数 ====================

/// 从配置中按实际模型查找定价并计算成本；未配置定价时返回 None。
fn compute_cost_from_config(state: &Arc<AppState>, actual_model: &str, tokens: &crate::audit::db::TokenUsage) -> Option<f64> {
    compute_cost_from_config_ref(&state.config, actual_model, tokens)
}

fn compute_cost_from_config_ref(config: &Arc<parking_lot::RwLock<AppConfig>>, actual_model: &str, tokens: &crate::audit::db::TokenUsage) -> Option<f64> {
    let cfg = config.read();
    let price = cfg.find_model_price(actual_model)?;
    Some(crate::cost::compute_cost(price, tokens.input, tokens.output, tokens.cache_read, tokens.cache_creation))
}

/// 从流式原始响应中提取 token 用量（供后台成本计算）
fn extract_tokens_for_stream_cost(raw_stream: &str) -> crate::audit::db::TokenUsage {
    crate::audit::db::extract_tokens_from_stream(raw_stream)
}

/// 尝试格式化 JSON 字符串，失败则原样返回
fn pretty_json(raw: &str) -> String {
    serde_json::from_str::<Value>(raw)
        .and_then(|v| serde_json::to_string_pretty(&v))
        .unwrap_or_else(|_| raw.to_string())
}

/// 替换 JSON 中的 model 字段值，保留其余所有字段的原始字节。
fn replace_model_in_json_str(raw: &str, new_model: &str) -> String {
    use serde_json::value::RawValue;
    use std::collections::HashMap;

    let mut map: HashMap<String, Box<RawValue>> = match serde_json::from_str(raw) {
        Ok(m) => m,
        Err(_) => return raw.to_string(),
    };

    if let Ok(new_val) = RawValue::from_string(format!("\"{}\"", new_model)) {
        map.insert("model".to_string(), new_val);
    }

    serde_json::to_string(&map).unwrap_or_else(|_| raw.to_string())
}

/// 设置或移除 JSON 请求体顶层的 stream 字段（转换路径专用）。
fn set_stream_in_json_str(raw: &str, is_stream: bool) -> String {
    let mut body: Value = match serde_json::from_str(raw) {
        Ok(v) => v,
        Err(_) => return raw.to_string(),
    };
    if let Some(m) = body.as_object_mut() {
        if is_stream {
            m.insert("stream".to_string(), Value::Bool(true));
        } else {
            m.remove("stream");
        }
    }
    serde_json::to_string(&body).unwrap_or_else(|_| raw.to_string())
}

/// 为 Chat 目标的流式请求打开 `stream_options.include_usage`。
///
/// OpenAI Chat Completions 默认不在流式响应里返回 usage，只有请求显式带上
/// `stream_options.include_usage: true` 才会在末尾下发一个 `choices: []` 的
/// usage chunk。转换路径的入口协议（Anthropic / Responses）没有这个字段，
/// 因此由网关补齐，否则流式请求的 token 统计恒为 0。
fn ensure_chat_stream_usage(body: &mut Value) {
    let obj = match body.as_object_mut() {
        Some(o) => o,
        None => return,
    };
    let options = obj
        .entry("stream_options".to_string())
        .or_insert_with(|| Value::Object(serde_json::Map::new()));
    if let Some(options) = options.as_object_mut() {
        options.insert("include_usage".to_string(), Value::Bool(true));
    }
}

/// 强制覆盖请求体中的 output_config.effort 字段。
fn force_effort_in_json_str(raw: &str, effort: &str) -> String {
    use serde_json::value::RawValue;
    use std::collections::HashMap;

    let mut map: HashMap<String, Box<RawValue>> = match serde_json::from_str(raw) {
        Ok(m) => m,
        Err(_) => return raw.to_string(),
    };

    let effort_json = serde_json::to_string(effort).unwrap_or_else(|_| "\"\"".to_string());

    let mut inner: HashMap<String, Box<RawValue>> = map.get("output_config")
        .and_then(|oc| serde_json::from_str(oc.get()).ok())
        .unwrap_or_default();

    if let Ok(val) = RawValue::from_string(effort_json) {
        inner.insert("effort".to_string(), val);
    }

    if let Ok(new_oc) = serde_json::to_string(&inner).and_then(RawValue::from_string) {
        map.insert("output_config".to_string(), new_oc);
    }

    serde_json::to_string(&map).unwrap_or_else(|_| raw.to_string())
}

/// 从 JSON 请求体中移除所有 thinking / redacted_thinking 类型的内容块。
fn strip_thinking_blocks(raw: &str) -> String {
    let mut body: Value = match serde_json::from_str(raw) {
        Ok(v) => v,
        Err(_) => return raw.to_string(),
    };

    if let Some(messages) = body.get_mut("messages").and_then(|m| m.as_array_mut()) {
        for msg in messages.iter_mut() {
            if let Some(content) = msg.get_mut("content") {
                if let Some(blocks) = content.as_array_mut() {
                    blocks.retain(|block| {
                        let block_type = block.get("type").and_then(|t| t.as_str()).unwrap_or("");
                        block_type != "thinking" && block_type != "redacted_thinking"
                    });
                }
            }
        }
    }

    serde_json::to_string(&body).unwrap_or_else(|_| raw.to_string())
}

/// Anthropic 渠道：移除 temperature 参数。
fn sanitize_anthropic_body_str(raw: &str) -> String {
    let mut body: Value = match serde_json::from_str(raw) {
        Ok(v) => v,
        Err(_) => return raw.to_string(),
    };

    body.as_object_mut().map(|m| m.remove("temperature"));

    serde_json::to_string(&body).unwrap_or_else(|_| raw.to_string())
}

/// Anthropic 渠道：自动注入 prompt caching 断点（无 cache_control 时才注入，幂等）。
fn inject_anthropic_cache_str(raw: &str) -> String {
    let mut body: Value = match serde_json::from_str(raw) {
        Ok(v) => v,
        Err(_) => return raw.to_string(),
    };

    crate::converter::request::inject_anthropic_cache_breakpoints(&mut body);

    serde_json::to_string(&body).unwrap_or_else(|_| raw.to_string())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_detect_sse_error_dashscope_format() {
        // 模拟 log 18867 的 DashScope/Qwen 格式
        let sse = "id:1\nevent:error\n:HTTP_STATUS/429\ndata:{\"request_id\":\"566e3621\",\"code\":\"Throttling.AllocationQuota\",\"message\":\"Allocated quota exceeded\"}\n\n";
        let result = detect_sse_error_in_prefix(sse.as_bytes());
        assert!(result.is_some());
        let msg = result.unwrap();
        assert!(msg.contains("Throttling.AllocationQuota") || msg.contains("SSE error event"));
    }

    #[test]
    fn test_detect_sse_error_anthropic_format() {
        let sse = "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n";
        let result = detect_sse_error_in_prefix(sse.as_bytes());
        assert!(result.is_some());
        assert!(result.unwrap().contains("Overloaded"));
    }

    #[test]
    fn test_detect_sse_error_openai_format() {
        let sse = "data: {\"error\":{\"message\":\"Rate limit exceeded\",\"type\":\"rate_limit_error\"}}\n\n";
        let result = detect_sse_error_in_prefix(sse.as_bytes());
        assert!(result.is_some());
        assert!(result.unwrap().contains("Rate limit"));
    }

    #[test]
    fn test_detect_sse_error_normal_content() {
        let sse = "data: {\"id\":\"chatcmpl-123\",\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n\n";
        let result = detect_sse_error_in_prefix(sse.as_bytes());
        assert!(result.is_none());
    }

    #[test]
    fn test_detect_sse_error_anthropic_normal() {
        let sse = "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"Hi\"}}\n\n";
        let result = detect_sse_error_in_prefix(sse.as_bytes());
        assert!(result.is_none());
    }

    #[test]
    fn test_sse_has_content_output_openai() {
        let sse = "data: {\"id\":\"chatcmpl-123\",\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n\n";
        assert!(sse_has_content_output(sse.as_bytes()));
    }

    #[test]
    fn test_sse_has_content_output_anthropic() {
        let sse = "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"Hi\"}}\n\n";
        assert!(sse_has_content_output(sse.as_bytes()));
    }

    #[test]
    fn test_sse_has_content_output_error_only() {
        let sse = "id:1\nevent:error\ndata:{\"code\":\"Throttling\",\"message\":\"quota exceeded\"}\n\n";
        assert!(!sse_has_content_output(sse.as_bytes()));
    }

    #[test]
    fn test_sse_has_content_output_empty_delta() {
        // OpenAI 的 role-only delta 不算内容输出
        let sse = "data: {\"id\":\"chatcmpl-123\",\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n";
        assert!(!sse_has_content_output(sse.as_bytes()));
    }

    #[test]
    fn test_strip_thinking_blocks_non_thinking_mode() {
        // 无顶层 thinking 参数：所有 thinking 块都应被移除
        let raw = r#"{
            "model": "claude-3-7",
            "messages": [
                {"role": "user", "content": "hi"},
                {"role": "assistant", "content": [
                    {"type": "thinking", "thinking": "old reasoning"},
                    {"type": "text", "text": "old answer"}
                ]},
                {"role": "user", "content": "again"},
                {"role": "assistant", "content": [
                    {"type": "thinking", "thinking": "recent reasoning"},
                    {"type": "text", "text": "recent answer"}
                ]}
            ]
        }"#;
        let out = strip_thinking_blocks(raw);
        let body: Value = serde_json::from_str(&out).unwrap();
        let msgs = body["messages"].as_array().unwrap();
        for m in msgs {
            let Some(blocks) = m["content"].as_array() else { continue };
            for b in blocks {
                let t = b["type"].as_str().unwrap_or("");
                assert_ne!(t, "thinking", "non-thinking mode must strip ALL thinking blocks");
            }
        }
    }

    #[test]
    fn test_strip_thinking_blocks_strips_all_in_thinking_mode() {
        // 思考模式下也要全部去除 thinking 和 redacted_thinking 块
        let raw = r#"{
            "model": "claude-3-7",
            "thinking": {"type": "enabled", "budget_tokens": 1024},
            "messages": [
                {"role": "user", "content": "hi"},
                {"role": "assistant", "content": [
                    {"type": "thinking", "thinking": "old reasoning"},
                    {"type": "text", "text": "old answer"}
                ]},
                {"role": "user", "content": "again"},
                {"role": "assistant", "content": [
                    {"type": "thinking", "thinking": "recent reasoning"},
                    {"type": "text", "text": "recent answer"}
                ]}
            ]
        }"#;
        let out = strip_thinking_blocks(raw);
        let body: Value = serde_json::from_str(&out).unwrap();
        let msgs = body["messages"].as_array().unwrap();
        for m in msgs {
            let Some(blocks) = m["content"].as_array() else { continue };
            for b in blocks {
                let t = b["type"].as_str().unwrap_or("");
                assert_ne!(t, "thinking", "strip_thinking must remove ALL thinking blocks even in thinking mode");
                assert_ne!(t, "redacted_thinking", "strip_thinking must remove ALL redacted_thinking blocks even in thinking mode");
            }
        }
    }

    #[test]
    fn test_strip_thinking_blocks_removes_redacted_thinking() {
        let raw = r#"{
            "model": "claude-3-7",
            "thinking": {"type": "enabled", "budget_tokens": 1024},
            "messages": [
                {"role": "user", "content": "hi"},
                {"role": "assistant", "content": [
                    {"type": "redacted_thinking", "data": "REDACTED"},
                    {"type": "text", "text": "answer"}
                ]}
            ]
        }"#;
        let out = strip_thinking_blocks(raw);
        let body: Value = serde_json::from_str(&out).unwrap();
        let blocks = body["messages"][1]["content"].as_array().unwrap();
        for b in blocks {
            let t = b["type"].as_str().unwrap_or("");
            assert_ne!(t, "redacted_thinking", "redacted_thinking must also be removed");
        }
    }

    #[test]
    fn test_ensure_chat_stream_usage_sets_flag() {
        let mut body = serde_json::json!({"model": "m", "stream": true, "messages": []});
        ensure_chat_stream_usage(&mut body);
        assert_eq!(body["stream_options"]["include_usage"], true);
    }

    /// 已有 stream_options 时只补 include_usage，不能整体覆盖掉其它选项。
    #[test]
    fn test_ensure_chat_stream_usage_preserves_existing_options() {
        let mut body = serde_json::json!({"stream_options": {"custom_key": 1}});
        ensure_chat_stream_usage(&mut body);
        assert_eq!(body["stream_options"]["include_usage"], true);
        assert_eq!(body["stream_options"]["custom_key"], 1);
    }

    #[test]
    fn test_ensure_chat_stream_usage_ignores_non_object_body() {
        let mut body = serde_json::json!("not an object");
        ensure_chat_stream_usage(&mut body);
        assert_eq!(body, serde_json::json!("not an object"));
    }
}
