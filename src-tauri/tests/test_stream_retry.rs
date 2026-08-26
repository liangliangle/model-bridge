//! 集成测试：流式请求 SSE 错误检测 + 自动重试 + 渠道故障转移
//!
//! 测试场景：
//! 1. 同渠道重试：上游前 N 次返回 SSE error 事件，第 N+1 次返回正常流 → 客户端收到正常内容
//! 2. 跨渠道 failover：渠道 A 始终返回 SSE error，重试耗尽后转移到渠道 B → 客户端收到 B 的内容
//! 3. 已有内容输出时不重试：上游返回正常内容流 → 客户端正常收到，不触发重试

use axum::routing::post;
use axum::Router;
use model_bridge_lib::channel::config::*;
use model_bridge_lib::channel::health::new_health_map;
use model_bridge_lib::proxy::context::{InputFormat, RequestContext};
use model_bridge_lib::proxy::router::route_request;
use model_bridge_lib::proxy::server::AppState;
use std::collections::HashMap;
use std::sync::atomic::{AtomicU32, Ordering};
use std::sync::Arc;
use tokio::net::TcpListener;

/// 启动 mock 上游服务器，返回其 URL
/// `fail_count`: 前 N 次请求返回 SSE error，之后返回正常流
async fn start_mock_upstream(fail_count: u32, content: &'static str) -> (String, Arc<AtomicU32>) {
    let request_count = Arc::new(AtomicU32::new(0));
    let count_clone = request_count.clone();

    let app = Router::new().route(
        "/v1/messages",
        post(move || {
            let count_clone = count_clone.clone();
            async move {
                let n = count_clone.fetch_add(1, Ordering::SeqCst);
                if n < fail_count {
                    // 返回 SSE error 事件（模拟 DashScope/Qwen 格式）
                    let error_sse = format!(
                        "id:1\nevent:error\n:HTTP_STATUS/429\ndata:{{\"request_id\":\"test-{}\",\"code\":\"Throttling.AllocationQuota\",\"message\":\"Allocated quota exceeded\"}}\n\n",
                        n
                    );
                    axum::http::Response::builder()
                        .status(200)
                        .header("Content-Type", "text/event-stream")
                        .body(axum::body::Body::from(error_sse))
                        .unwrap()
                } else {
                    // 返回正常 SSE 流
                    let sse_body = format!(
                        "event: content_block_delta\ndata: {{\"type\":\"content_block_delta\",\"index\":0,\"delta\":{{\"type\":\"text_delta\",\"text\":\"{}\"}}}}\n\nevent: message_stop\ndata: {{\"type\":\"message_stop\"}}\n\n",
                        content
                    );
                    axum::http::Response::builder()
                        .status(200)
                        .header("Content-Type", "text/event-stream")
                        .body(axum::body::Body::from(sse_body))
                        .unwrap()
                }
            }
        }),
    );

    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    let url = format!("http://{}/v1/messages", addr);

    tokio::spawn(async move {
        axum::serve(listener, app).await.unwrap();
    });

    (url, request_count)
}

/// 启动 mock 上游（OpenAI Chat 格式）
async fn start_mock_upstream_openai(fail_count: u32, content: &'static str) -> (String, Arc<AtomicU32>) {
    let request_count = Arc::new(AtomicU32::new(0));
    let count_clone = request_count.clone();

    let app = Router::new().route(
        "/v1/chat/completions",
        post(move || {
            let count_clone = count_clone.clone();
            async move {
                let n = count_clone.fetch_add(1, Ordering::SeqCst);
                if n < fail_count {
                    // OpenAI 格式 SSE error
                    let error_sse = format!(
                        "data: {{\"error\":{{\"message\":\"Rate limit exceeded (attempt {})\",\"type\":\"rate_limit_error\"}}}}\n\ndata: [DONE]\n\n",
                        n
                    );
                    axum::http::Response::builder()
                        .status(200)
                        .header("Content-Type", "text/event-stream")
                        .body(axum::body::Body::from(error_sse))
                        .unwrap()
                } else {
                    let sse_body = format!(
                        "data: {{\"id\":\"chatcmpl-test\",\"object\":\"chat.completion.chunk\",\"choices\":[{{\"index\":0,\"delta\":{{\"content\":\"{}\"}},\"finish_reason\":null}}]}}\n\ndata: [DONE]\n\n",
                        content
                    );
                    axum::http::Response::builder()
                        .status(200)
                        .header("Content-Type", "text/event-stream")
                        .body(axum::body::Body::from(sse_body))
                        .unwrap()
                }
            }
        }),
    );

    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    let url = format!("http://{}/v1/chat/completions", addr);

    tokio::spawn(async move {
        axum::serve(listener, app).await.unwrap();
    });

    (url, request_count)
}

fn make_test_config(channels: Vec<ChannelConfig>) -> AppConfig {
    AppConfig {
        listen_port: 0,
        listen_host: "127.0.0.1".to_string(),
        public_url: None,
        debug: true,
        models: vec![],
        channels,
        failover: FailoverConfig::default(),
        ultimate_fallback: None,
        auth: AuthConfig::default(),
        mcp_servers: vec![],
        skill_manager: SkillManagerConfig::default(),
        audit_retention_days: None,
    }
}

fn make_channel(id: &str, url: &str, provider: ProviderType, retry_count: u32, priority: u32) -> ChannelConfig {
    ChannelConfig {
        id: id.to_string(),
        name: id.to_string(),
        provider,
        endpoint: EndpointConfig { url: url.to_string() },
        api_key: "test-key".to_string(),
        priority,
        enabled: true,
        fallback_model: "test-model".to_string(),
        model_mapping: {
            let mut m = HashMap::new();
            m.insert("test-model".to_string(), "actual-model".to_string());
            m
        },
        timeout_ms: 30000,
        rate_limit: None,
        custom_headers: HashMap::new(),
        strip_thinking: false,
        retry_count,
        retry_delay_ms: 50, // 短延迟加速测试
        auto_cache: false,
        force_effort: None,
    }
}

async fn make_app_state(config: AppConfig) -> Arc<AppState> {
    let channel_ids: Vec<String> = config.channels.iter().map(|c| c.id.clone()).collect();
    let health_map = new_health_map(&channel_ids);

    // 每个测试使用独立的内存数据库，避免并行测试时文件锁冲突
    let audit_db = model_bridge_lib::audit::db::AuditDb::new(":memory:").unwrap();

    Arc::new(AppState {
        config: Arc::new(parking_lot::RwLock::new(config)),
        health_map,
        audit_db,
        http_client: reqwest::Client::new(),
        oauth: model_bridge_lib::mcp::oauth::new_store_from_config(),
    })
}

/// 测试 1：同渠道重试 — 上游前 2 次返回 SSE error，第 3 次返回正常流
/// 渠道 retry_count=2（共 3 次尝试），应在第 3 次成功
#[tokio::test]
async fn test_stream_retry_same_channel() {
    let (url, request_count) = start_mock_upstream(2, "Hello from retry").await;

    let channel = make_channel("ch1", &url, ProviderType::Anthropic, 2, 1);
    let config = make_test_config(vec![channel]);
    let state = make_app_state(config).await;

    let body = r#"{"model":"test-model","messages":[{"role":"user","content":"hi"}],"stream":true}"#;
    let ctx = RequestContext::from_raw(
        InputFormat::Anthropic,
        "/v1/messages",
        axum::http::HeaderMap::new(),
        body.to_string(),
    );

    let response = route_request(state, ctx).await;

    assert_eq!(response.status(), 200, "Should succeed after retries");

    // 读取响应体
    let body_bytes = axum::body::to_bytes(response.into_body(), usize::MAX).await.unwrap();
    let body_text = String::from_utf8_lossy(&body_bytes);

    println!("Response body: {}", body_text);
    assert!(body_text.contains("Hello from retry"), "Should contain the successful content");
    assert!(!body_text.contains("Throttling"), "Should not contain error content");

    // 验证上游收到了 3 次请求（2 次失败 + 1 次成功）
    assert_eq!(request_count.load(Ordering::SeqCst), 3, "Should have made 3 attempts");
}

/// 测试 2：跨渠道 failover — 渠道 A 始终失败（retry_count=1，共 2 次），转移到渠道 B 成功
#[tokio::test]
async fn test_stream_retry_failover_to_second_channel() {
    // 渠道 A：始终返回 error（fail_count 设为很大值）
    let (url_a, count_a) = start_mock_upstream(100, "never").await;
    // 渠道 B：始终正常
    let (url_b, count_b) = start_mock_upstream(0, "Hello from channel B").await;

    let channel_a = make_channel("ch-a", &url_a, ProviderType::Anthropic, 1, 1);
    let channel_b = make_channel("ch-b", &url_b, ProviderType::Anthropic, 0, 2);
    let config = make_test_config(vec![channel_a, channel_b]);
    let state = make_app_state(config).await;

    let body = r#"{"model":"test-model","messages":[{"role":"user","content":"hi"}],"stream":true}"#;
    let ctx = RequestContext::from_raw(
        InputFormat::Anthropic,
        "/v1/messages",
        axum::http::HeaderMap::new(),
        body.to_string(),
    );

    let response = route_request(state, ctx).await;

    assert_eq!(response.status(), 200, "Should succeed via channel B");

    let body_bytes = axum::body::to_bytes(response.into_body(), usize::MAX).await.unwrap();
    let body_text = String::from_utf8_lossy(&body_bytes);

    println!("Response body: {}", body_text);
    assert!(body_text.contains("Hello from channel B"), "Should contain channel B content");

    // 渠道 A 收到 2 次请求（1 + retry_count=1）
    assert_eq!(count_a.load(Ordering::SeqCst), 2, "Channel A should have 2 attempts");
    // 渠道 B 收到 1 次请求
    assert_eq!(count_b.load(Ordering::SeqCst), 1, "Channel B should have 1 attempt");
}

/// 测试 3：已有内容输出时不重试 — 上游直接返回正常流，不触发重试
#[tokio::test]
async fn test_stream_no_retry_when_content_present() {
    let (url, request_count) = start_mock_upstream(0, "Direct content").await;

    let channel = make_channel("ch1", &url, ProviderType::Anthropic, 2, 1);
    let config = make_test_config(vec![channel]);
    let state = make_app_state(config).await;

    let body = r#"{"model":"test-model","messages":[{"role":"user","content":"hi"}],"stream":true}"#;
    let ctx = RequestContext::from_raw(
        InputFormat::Anthropic,
        "/v1/messages",
        axum::http::HeaderMap::new(),
        body.to_string(),
    );

    let response = route_request(state, ctx).await;

    assert_eq!(response.status(), 200);

    let body_bytes = axum::body::to_bytes(response.into_body(), usize::MAX).await.unwrap();
    let body_text = String::from_utf8_lossy(&body_bytes);

    println!("Response body: {}", body_text);
    assert!(body_text.contains("Direct content"));

    // 只有 1 次请求，没有重试
    assert_eq!(request_count.load(Ordering::SeqCst), 1, "Should only make 1 request (no retry)");
}

/// 测试 4：OpenAI Chat 格式的 SSE error 也能触发重试
#[tokio::test]
async fn test_stream_retry_openai_format() {
    let (url, request_count) = start_mock_upstream_openai(1, "OpenAI retry success").await;

    let channel = make_channel("ch1", &url, ProviderType::Openai, 1, 1);
    let config = make_test_config(vec![channel]);
    let state = make_app_state(config).await;

    let body = r#"{"model":"test-model","messages":[{"role":"user","content":"hi"}],"stream":true}"#;
    let ctx = RequestContext::from_raw(
        InputFormat::OpenAI,
        "/v1/chat/completions",
        axum::http::HeaderMap::new(),
        body.to_string(),
    );

    let response = route_request(state, ctx).await;

    assert_eq!(response.status(), 200, "Should succeed after retry");

    let body_bytes = axum::body::to_bytes(response.into_body(), usize::MAX).await.unwrap();
    let body_text = String::from_utf8_lossy(&body_bytes);

    println!("Response body: {}", body_text);
    assert!(body_text.contains("OpenAI retry success"));
    assert_eq!(request_count.load(Ordering::SeqCst), 2, "Should have 2 attempts");
}

/// 测试 5：所有渠道重试耗尽后返回错误
#[tokio::test]
async fn test_stream_all_channels_exhausted() {
    let (url_a, _count_a) = start_mock_upstream(100, "never").await;
    let (url_b, _count_b) = start_mock_upstream(100, "never").await;

    let channel_a = make_channel("ch-a", &url_a, ProviderType::Anthropic, 1, 1);
    let channel_b = make_channel("ch-b", &url_b, ProviderType::Anthropic, 0, 2);
    let config = make_test_config(vec![channel_a, channel_b]);
    let state = make_app_state(config).await;

    let body = r#"{"model":"test-model","messages":[{"role":"user","content":"hi"}],"stream":true}"#;
    let ctx = RequestContext::from_raw(
        InputFormat::Anthropic,
        "/v1/messages",
        axum::http::HeaderMap::new(),
        body.to_string(),
    );

    let response = route_request(state, ctx).await;

    // 所有渠道失败，应返回 502
    assert_eq!(response.status(), 502, "Should return 502 when all channels fail");

    let body_bytes = axum::body::to_bytes(response.into_body(), usize::MAX).await.unwrap();
    let body_text = String::from_utf8_lossy(&body_bytes);
    println!("Error response: {}", body_text);
    assert!(body_text.contains("All channels failed"));
}

/// 测试 6：完整 HTTP 端到端 — 启动真实 axum 服务器，通过 HTTP 请求验证流式重试
/// 这模拟了用户用 curl 发送请求到 /v1/messages 的完整路径
#[tokio::test]
async fn test_stream_retry_full_http_e2e() {
    // mock 上游：前 2 次返回 error，第 3 次正常
    let (upstream_url, request_count) = start_mock_upstream(2, "E2E success!").await;

    let channel = make_channel("e2e-ch", &upstream_url, ProviderType::Anthropic, 2, 1);
    let config = make_test_config(vec![channel]);
    let state = make_app_state(config).await;

    // 启动真实的 axum HTTP 服务器
    let app = axum::Router::new()
        .route("/v1/messages", axum::routing::post(
            |headers: axum::http::HeaderMap, body: String| async move {
                let ctx = RequestContext::from_raw(
                    InputFormat::Anthropic, "/v1/messages", headers, body,
                );
                route_request(state.clone(), ctx).await
            }
        ));

    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let proxy_addr = listener.local_addr().unwrap();
    tokio::spawn(async move {
        axum::serve(listener, app).await.unwrap();
    });

    // 用 reqwest 发送流式请求（等同于 curl -N）
    let client = reqwest::Client::new();
    let resp = client
        .post(format!("http://{}/v1/messages", proxy_addr))
        .header("Content-Type", "application/json")
        .body(r#"{"model":"test-model","messages":[{"role":"user","content":"hi"}],"stream":true}"#)
        .send()
        .await
        .unwrap();

    assert_eq!(resp.status(), 200, "HTTP status should be 200");
    assert!(resp.headers().get("content-type").unwrap().to_str().unwrap().contains("text/event-stream"));

    let body_text = resp.text().await.unwrap();
    println!("[E2E] SSE response:\n{}", body_text);

    assert!(body_text.contains("E2E success!"), "Should contain successful content");
    assert!(!body_text.contains("Throttling"), "Should NOT contain error");
    assert_eq!(request_count.load(Ordering::SeqCst), 3, "Upstream should receive 3 requests (2 fail + 1 success)");
}

/// 测试 7：验证审计日志中记录了 retry_count 和 failover_chain
#[tokio::test]
async fn test_stream_retry_audit_trail() {
    let (url_a, _count_a) = start_mock_upstream(100, "never").await;
    let (url_b, _count_b) = start_mock_upstream(0, "Success via B").await;

    let channel_a = make_channel("ch-a", &url_a, ProviderType::Anthropic, 1, 1);
    let channel_b = make_channel("ch-b", &url_b, ProviderType::Anthropic, 0, 2);
    let config = make_test_config(vec![channel_a, channel_b]);
    let state = make_app_state(config).await;

    let body = r#"{"model":"test-model","messages":[{"role":"user","content":"hi"}],"stream":true}"#;
    let ctx = RequestContext::from_raw(
        InputFormat::Anthropic,
        "/v1/messages",
        axum::http::HeaderMap::new(),
        body.to_string(),
    );

    let response = route_request(state.clone(), ctx).await;
    assert_eq!(response.status(), 200);

    // 消费响应体以触发流结束，等待后台审计任务完成
    let _body = axum::body::to_bytes(response.into_body(), usize::MAX).await.unwrap();
    tokio::time::sleep(std::time::Duration::from_millis(100)).await;

    // 查询渠道 B 的审计记录（成功的那个），验证 retry_count 和 failover_chain
    let logs = state.audit_db.query_list(
        None, Some("ch-b"), None, None, None, None, None, 1,
    ).unwrap();

    assert!(!logs.is_empty(), "Should have at least one audit record");
    let log = &logs[0];
    println!("[Audit] retry_count={}, failover_chain={:?}", log.retry_count, log.failover_chain);

    // 渠道 A 重试了 1 次（attempt 0 失败 + attempt 1 失败），然后转移到 B
    assert!(log.retry_count >= 1, "retry_count should be >= 1, got {}", log.retry_count);
    let chain = log.failover_chain.as_deref().unwrap_or("");
    assert!(chain.contains("ch-a"), "failover_chain should contain ch-a, got: {}", chain);
    assert!(chain.contains("ch-b"), "failover_chain should contain ch-b, got: {}", chain);
}
