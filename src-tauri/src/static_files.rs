//! 前端静态资源服务
//!
//! 通过 build.rs 在编译时将 dist/ 目录的所有文件以 include_bytes! 方式嵌入二进制。
//! 运行时根据 URL 路径查找对应文件，支持 SPA 路由 fallback 到 index.html。

use axum::extract::Request;
use axum::response::{Html, IntoResponse, Response};
use axum::http::{StatusCode, header};
use axum::body::Body;

// 引入 build.rs 生成的嵌入资源查找函数
include!(concat!(env!("OUT_DIR"), "/embedded_assets.rs"));

/// 静态文件处理器，作为 axum 的 fallback handler
///
/// 查找逻辑：
/// 1. 精确匹配请求路径对应的文件
/// 2. 未命中则返回 index.html（SPA 路由支持）
/// 3. index.html 也不存在则显示提示信息
pub async fn static_handler(req: Request) -> Response {
    let path = req.uri().path().trim_start_matches('/');

    // 根路径直接返回 index.html
    let lookup = if path.is_empty() { "index.html" } else { path };

    // 尝试精确匹配文件
    if let Some((data, mime)) = get_embedded_file(lookup) {
        return Response::builder()
            .header(header::CONTENT_TYPE, mime)
            .body(Body::from(data))
            .unwrap();
    }

    // Unknown API paths must not fall through to the SPA shell.
    if path.starts_with("api/") {
        return StatusCode::NOT_FOUND.into_response();
    }

    // SPA fallback：非 API 路径都返回 index.html，由前端路由处理
    if let Some((data, _)) = get_embedded_file("index.html") {
        return Response::builder()
            .header(header::CONTENT_TYPE, "text/html; charset=utf-8")
            .body(Body::from(data))
            .unwrap();
    }

    // 前端未嵌入时的降级提示
    (StatusCode::OK, Html(
        "<html><body><h2>Model Bridge</h2><p>Frontend not embedded. Build with <code>pnpm build</code> first, then <code>cargo build --release</code>.</p></body></html>"
    )).into_response()
}
