//! 路由器：负责选择渠道、创建审计日志、管理 failover 循环。
//! 不做格式转换，不处理 header，只负责"选谁"和"记日志"。

use axum::http::StatusCode;
use axum::response::{IntoResponse, Response};
use axum::Json;
use serde_json::json;
use std::sync::Arc;
use std::time::Instant;

use crate::channel::failover::select_routes;
use crate::proxy::context::RequestContext;
use crate::proxy::executor;
use crate::proxy::server::AppState;

/// 路由入口：接收 RequestContext，选择渠道并 failover
pub async fn route_request(state: Arc<AppState>, ctx: RequestContext) -> Response {
    let start = Instant::now();
    if let Some(error) = &ctx.parse_error {
        return (
            StatusCode::BAD_REQUEST,
            Json(json!({"error": {"message": format!("Invalid JSON request: {}", error), "type": "invalid_request_error"}})),
        ).into_response();
    }
    let config = state.config.read().clone();
    let routes = select_routes(&config, &state.health_map, &ctx.model_alias, ctx.input_format);

    // 无可用渠道
    if routes.is_empty() {
        if let Ok(id) = state.audit_db.create(
            "POST", &ctx.path, &ctx.model_alias,
            ctx.original_headers_str.as_deref(),
            Some(&ctx.raw_body),
            ctx.user_agent.as_deref(),
        ) {
            let _ = state.audit_db.update_error(id, 503, start.elapsed().as_millis() as u64, "No available channels");
        }
        return (StatusCode::SERVICE_UNAVAILABLE,
            Json(json!({"error": {"message": "No available channels", "type": "server_error"}})),
        ).into_response();
    }

    let mut last_error: Option<String> = None;
    let mut failover_chain: Vec<String> = Vec::new();
    let mut total_retries: u32 = 0;

    // 按优先级逐个尝试渠道
    for route in &routes {
        // 单渠道内重试：1 + retry_count 次尝试
        let max_attempts = 1 + route.channel.retry_count;
        failover_chain.push(route.channel.id.clone());

        for attempt in 0..max_attempts {
            if attempt > 0 {
                total_retries += 1;
                tokio::time::sleep(std::time::Duration::from_millis(route.channel.retry_delay_ms)).await;
                tracing::info!(
                    channel = %route.channel.id, attempt = attempt + 1, max = max_attempts,
                    "Retrying on same channel"
                );
            }

            let attempt_start = Instant::now();

            let audit_id = state.audit_db.create(
                "POST", &ctx.path, &ctx.model_alias,
                ctx.original_headers_str.as_deref(),
                Some(&ctx.raw_body),
                ctx.user_agent.as_deref(),
            ).unwrap_or(-1);

            let _ = state.audit_db.update_route(
                audit_id, &route.channel.id, &route.actual_model,
                &format!("{:?}", route.mapping_source),
            );

            let result = executor::execute_on_channel(
                &state, &ctx, &route.channel, &route.actual_model, audit_id,
            ).await;

            match result {
                Ok(response) => {
                    let latency = attempt_start.elapsed().as_millis() as u64;
                    // 记录重试信息到成功的审计记录
                    let _ = state.audit_db.update_retry_info(audit_id, total_retries, &failover_chain);
                    let mut health = state.health_map.write();
                    if let Some(h) = health.get_mut(&route.channel.id) {
                        h.record_success(latency, &config.failover.circuit_breaker);
                    }
                    return response;
                }
                Err(err_msg) => {
                    let latency = attempt_start.elapsed().as_millis() as u64;
                    let error_status = if err_msg.starts_with("invalid_request:") { 400 } else { 502 };
                    let _ = state.audit_db.update_error(audit_id, error_status, latency, &err_msg);
                    let _ = state.audit_db.update_retry_info(audit_id, total_retries, &failover_chain);
                    last_error = Some(err_msg);

                    // 仅在该渠道所有重试耗尽后才更新熔断计数
                    if attempt + 1 == max_attempts {
                        let mut health = state.health_map.write();
                        if let Some(h) = health.get_mut(&route.channel.id) {
                            h.record_failure(&config.failover.circuit_breaker);
                        }
                    }
                }
            }
        }
    }

    // 所有渠道都失败
    let final_status = if last_error.as_deref().map(|e| e.starts_with("invalid_request:")).unwrap_or(false) {
        StatusCode::BAD_REQUEST
    } else {
        StatusCode::BAD_GATEWAY
    };
    (final_status,
        Json(json!({"error": {"message": format!("All channels failed: {}", last_error.unwrap_or_default()), "type": "server_error"}})),
    ).into_response()
}
