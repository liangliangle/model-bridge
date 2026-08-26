//! Skill 统一管理子系统：扫描各 agent 的 skill 目录，统一查看/编辑/启停/分发。
//!
//! - `scanner` — 扫描 + frontmatter 解析 + 跨 agent 启用矩阵
//! - `linker`  — 软链接启停/分发/纳管 + SKILL.md 读写（安全校验集中处）
//!
//! 以中心库（~/.agents/skills）为源：启用=建软链接，禁用=删软链接，分发=链到多 agent。

pub mod linker;
pub mod scanner;

use std::sync::Arc;

use axum::extract::State;
use axum::http::HeaderMap;
use axum::response::{IntoResponse, Response};
use axum::Json;
use serde::Serialize;
use serde_json::json;

use crate::channel::config::SkillAgentTarget;
use crate::proxy::server::{check_admin_auth, AppState};

// ========== 视图结构（给前端）==========

/// 某 skill 在某 agent 上的状态
#[derive(Debug, Clone, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum SkillState {
    Native,   // agent 原生读取中心库 → 中心库有即生效（全启，不可单独启停）
    Linked,   // 软链接指向中心库同名 → 已启用（受管）
    External, // 软链接但指向中心库以外 → 已启用但不受管
    RealDir,  // 真实目录 → 已启用但独立（Cursor / frontend-design）
    Broken,   // 悬空软链接
    Absent,   // 不存在 → 禁用
}

#[derive(Debug, Clone, Serialize)]
pub struct AgentSkillState {
    pub agent: String,
    pub state: SkillState,
    pub path: Option<String>,
    pub link_target: Option<String>,
}

#[derive(Debug, Clone, Serialize)]
pub struct SkillSummary {
    pub name: String,            // frontmatter name
    pub dir_name: String,        // 目录名（启停/编辑都用它定位）
    pub description: String,
    pub in_central: bool,        // 中心库是否有该 skill
    pub central_path: Option<String>,
    pub editable: bool,          // == in_central
    pub agents: Vec<AgentSkillState>,
}

/// 插件携带的 skill（只读）
#[derive(Debug, Clone, Serialize)]
pub struct PluginSkill {
    pub name: String,
    pub description: String,
    pub plugin: String,
    pub path: String,
}

// ========== handler ==========

/// GET /api/skills —— 扫描汇总
pub async fn get_skills(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
) -> Response {
    if let Some(reject) = check_admin_auth(&state, &headers) { return reject; }
    let cfg = state.config.read().skill_manager.clone();
    let skills = scanner::scan_skills(&cfg);
    let plugin_skills = scanner::scan_plugin_skills();
    Json(json!({
        "central_lib": cfg.central_lib,
        "agents": cfg.agents,
        "skills": skills,
        "plugin_skills": plugin_skills,
    })).into_response()
}

#[derive(Debug, serde::Deserialize)]
pub struct SkillContentQuery {
    pub name: String, // 目录名
}

/// GET /api/skills/content?name= —— 读中心库 skill 的 SKILL.md
pub async fn get_skill_content(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    axum::extract::Query(params): axum::extract::Query<SkillContentQuery>,
) -> Response {
    if let Some(reject) = check_admin_auth(&state, &headers) { return reject; }
    let central = state.config.read().skill_manager.central_lib.clone();
    match linker::read_skill_md(&central, &params.name) {
        Ok(content) => Json(json!({ "name": params.name, "content": content })).into_response(),
        Err(e) => Json(json!({ "error": e })).into_response(),
    }
}

#[derive(Debug, serde::Deserialize)]
pub struct SaveContentReq {
    pub name: String,
    pub content: String,
}

/// POST /api/skills/content —— 写中心库 skill 的 SKILL.md
pub async fn save_skill_content(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    Json(req): Json<SaveContentReq>,
) -> Response {
    if let Some(reject) = check_admin_auth(&state, &headers) { return reject; }
    let central = state.config.read().skill_manager.central_lib.clone();
    match linker::write_skill_md(&central, &req.name, &req.content) {
        Ok(_) => Json(json!({ "success": true })).into_response(),
        Err(e) => Json(json!({ "error": e })).into_response(),
    }
}

#[derive(Debug, serde::Deserialize)]
pub struct ToggleReq {
    pub skill: String, // 目录名
    pub agent: String,
    pub enabled: bool,
    #[serde(default)]
    pub force: bool,
}

/// POST /api/skills/toggle —— 启用/禁用某 skill 对某 agent
pub async fn toggle_skill(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    Json(req): Json<ToggleReq>,
) -> Response {
    if let Some(reject) = check_admin_auth(&state, &headers) { return reject; }
    let (central, root, native) = match resolve_agent_root(&state, &req.agent) {
        Ok(v) => v,
        Err(e) => return Json(json!({ "error": e })).into_response(),
    };
    if native {
        return Json(json!({ "error": "该 agent 原生读取中心库，skill 自动全部生效，无需启停" })).into_response();
    }
    let result = if req.enabled {
        linker::enable(&central, &root, &req.skill, req.force)
    } else {
        linker::disable(&root, &req.skill)
    };
    match result {
        Ok(_) => Json(json!({ "success": true })).into_response(),
        Err(e) => Json(json!({ "error": e })).into_response(),
    }
}

#[derive(Debug, serde::Deserialize)]
pub struct DistributeReq {
    pub skill: String,
    pub agents: Vec<String>,
    pub enabled: bool,
    #[serde(default)]
    pub force: bool,
}

/// POST /api/skills/distribute —— 批量启停到多个 agent
pub async fn distribute_skill(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    Json(req): Json<DistributeReq>,
) -> Response {
    if let Some(reject) = check_admin_auth(&state, &headers) { return reject; }
    let mut results = Vec::new();
    for agent in &req.agents {
        let r = match resolve_agent_root(&state, agent) {
            Ok((_, _, true)) => Err("该 agent 原生读取中心库，无需启停".to_string()),
            Ok((central, root, false)) => {
                if req.enabled {
                    linker::enable(&central, &root, &req.skill, req.force)
                } else {
                    linker::disable(&root, &req.skill)
                }
            }
            Err(e) => Err(e),
        };
        results.push(json!({
            "agent": agent,
            "success": r.is_ok(),
            "error": r.err(),
        }));
    }
    Json(json!({ "results": results })).into_response()
}

#[derive(Debug, serde::Deserialize)]
pub struct ImportReq {
    pub skill: String,
    pub agent: String,
}

/// POST /api/skills/import —— 把 agent 下真实 skill 目录纳入中心库管理
pub async fn import_skill(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    Json(req): Json<ImportReq>,
) -> Response {
    if let Some(reject) = check_admin_auth(&state, &headers) { return reject; }
    let (central, root, native) = match resolve_agent_root(&state, &req.agent) {
        Ok(v) => v,
        Err(e) => return Json(json!({ "error": e })).into_response(),
    };
    if native {
        return Json(json!({ "error": "该 agent 原生读取中心库，无需纳管" })).into_response();
    }
    match linker::import(&central, &root, &req.skill) {
        Ok(_) => Json(json!({ "success": true })).into_response(),
        Err(e) => Json(json!({ "error": e })).into_response(),
    }
}

#[derive(Debug, serde::Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct SaveCentralReq {
    pub central_lib: String,
}

/// POST /api/config/skill/central —— 修改中心库路径
pub async fn save_skill_central(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    Json(req): Json<SaveCentralReq>,
) -> Response {
    if let Some(reject) = check_admin_auth(&state, &headers) { return reject; }
    if req.central_lib.trim().is_empty() {
        return Json(json!({ "error": "中心库路径不能为空" })).into_response();
    }
    {
        let mut config = state.config.write();
        config.skill_manager.central_lib = req.central_lib;
    }
    persist(&state)
}

/// POST /api/config/skill/agent —— 新增/更新扫描目标
/// 直接收 SkillAgentTarget（对齐 save_mcp_server，前端 invoke 解包对象作 body）
pub async fn save_skill_agent(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    Json(target): Json<SkillAgentTarget>,
) -> Response {
    if let Some(reject) = check_admin_auth(&state, &headers) { return reject; }
    {
        let mut config = state.config.write();
        let agents = &mut config.skill_manager.agents;
        if let Some(existing) = agents.iter_mut().find(|a| a.name == target.name) {
            // 保留 builtin 标记，只更新可编辑字段
            existing.root_path = target.root_path;
            existing.enabled = target.enabled;
            existing.native_agents_dir = target.native_agents_dir;
        } else {
            let mut t = target;
            t.builtin = false; // 新增的一律非内置
            agents.push(t);
        }
    }
    persist(&state)
}

#[derive(Debug, serde::Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct DeleteAgentReq {
    pub name: String,
}

/// POST /api/config/skill/agent/delete —— 删除扫描目标
pub async fn delete_skill_agent(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    Json(req): Json<DeleteAgentReq>,
) -> Response {
    if let Some(reject) = check_admin_auth(&state, &headers) { return reject; }
    {
        let mut config = state.config.write();
        let agents = &mut config.skill_manager.agents;
        agents.retain(|a| a.name != req.name);
    }
    persist(&state)
}

// ========== 辅助 ==========

/// 取中心库路径 + 指定 agent 的根目录 + 是否原生读取中心库
fn resolve_agent_root(state: &Arc<AppState>, agent: &str) -> Result<(String, String, bool), String> {
    let cfg = state.config.read();
    let central = cfg.skill_manager.central_lib.clone();
    let target = cfg.skill_manager.agents.iter()
        .find(|a| a.name == agent)
        .ok_or_else(|| format!("未知 agent: {}", agent))?;
    Ok((central, target.root_path.clone(), target.native_agents_dir))
}

fn persist(state: &Arc<AppState>) -> Response {
    match crate::commands::save_config_to_disk(&state.config.read()) {
        Ok(_) => Json(json!({ "success": true })).into_response(),
        Err(e) => Json(json!({ "error": e })).into_response(),
    }
}
