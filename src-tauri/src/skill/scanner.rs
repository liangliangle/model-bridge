//! Skill 扫描：遍历中心库与各 agent 的 skill 根目录，解析 frontmatter，
//! 计算每个 skill 的跨 agent 启用矩阵。纯读操作。

use std::collections::BTreeMap;
use std::path::{Path, PathBuf};

use serde::Deserialize;

use crate::channel::config::SkillManagerConfig;
use super::{AgentSkillState, PluginSkill, SkillState, SkillSummary};

/// 展开路径中的 ~ 和 ${HOME}
pub fn expand_path(p: &str) -> PathBuf {
    let home = std::env::var("HOME").unwrap_or_default();
    let expanded = if let Some(rest) = p.strip_prefix("~/") {
        format!("{}/{}", home, rest)
    } else if p == "~" {
        home.clone()
    } else {
        p.replace("${HOME}", &home)
    };
    PathBuf::from(expanded)
}

/// SKILL.md frontmatter（只取 name/description，其余忽略）
#[derive(Deserialize, Default)]
struct Frontmatter {
    #[serde(default)]
    name: Option<String>,
    #[serde(default)]
    description: Option<String>,
}

/// 解析 SKILL.md，返回 (name, description)。失败时 name 退回目录名、description 空。
pub fn parse_skill_md(skill_md: &Path, fallback_name: &str) -> (String, String) {
    let content = std::fs::read_to_string(skill_md).unwrap_or_default();
    let fm = parse_frontmatter(&content);
    let name = fm.name.filter(|s| !s.is_empty()).unwrap_or_else(|| fallback_name.to_string());
    let description = fm.description.unwrap_or_default();
    (name, description)
}

/// 从 markdown 文本切出 YAML frontmatter 并解析
fn parse_frontmatter(content: &str) -> Frontmatter {
    let trimmed = content.trim_start();
    if !trimmed.starts_with("---") {
        return Frontmatter::default();
    }
    // 跳过首行 ---，找到下一行单独的 ---
    let after_first = match trimmed.find('\n') {
        Some(i) => &trimmed[i + 1..],
        None => return Frontmatter::default(),
    };
    // 找闭合 ---（行首）
    let end = after_first
        .find("\n---")
        .map(|i| i)
        .or_else(|| if after_first.starts_with("---") { Some(0) } else { None });
    let yaml = match end {
        Some(i) => &after_first[..i],
        None => return Frontmatter::default(),
    };
    serde_yaml::from_str::<Frontmatter>(yaml).unwrap_or_default()
}

/// 判断 agent 根目录下某 skill 项的状态
fn detect_state(entry_path: &Path, central_skill: &Path) -> (SkillState, Option<String>) {
    let meta = match std::fs::symlink_metadata(entry_path) {
        Ok(m) => m,
        Err(_) => return (SkillState::Absent, None),
    };
    if meta.file_type().is_symlink() {
        let target = std::fs::read_link(entry_path).ok();
        let target_str = target.as_ref().map(|t| t.to_string_lossy().to_string());
        // 目标不存在 → 悬空
        if !entry_path.exists() {
            return (SkillState::Broken, target_str);
        }
        // 指向中心库同名 → Linked，否则 External
        let points_central = match (entry_path.canonicalize(), central_skill.canonicalize()) {
            (Ok(a), Ok(b)) => a == b,
            _ => false,
        };
        if points_central {
            (SkillState::Linked, target_str)
        } else {
            (SkillState::External, target_str)
        }
    } else if meta.is_dir() {
        (SkillState::RealDir, None)
    } else {
        (SkillState::Absent, None)
    }
}

/// 是否为有效 skill 目录（含 SKILL.md）
fn has_skill_md(dir: &Path) -> bool {
    dir.join("SKILL.md").is_file()
}

/// 跳过的目录项（隐藏/系统）
fn should_skip(name: &str) -> bool {
    name.starts_with('.')
}

/// 扫描所有 skill，返回汇总列表
pub fn scan_skills(cfg: &SkillManagerConfig) -> Vec<SkillSummary> {
    let central = expand_path(&cfg.central_lib);
    // (name, root_path, native_agents_dir)
    let enabled_agents: Vec<(&str, PathBuf, bool)> = cfg
        .agents
        .iter()
        .filter(|a| a.enabled)
        .map(|a| (a.name.as_str(), expand_path(&a.root_path), a.native_agents_dir))
        .collect();

    let mut map: BTreeMap<String, SkillSummary> = BTreeMap::new();

    // 1. 中心库的 skill（可编辑、可启停的源）
    if let Ok(entries) = std::fs::read_dir(&central) {
        for entry in entries.flatten() {
            let path = entry.path();
            let fname = entry.file_name().to_string_lossy().to_string();
            if should_skip(&fname) || !path.is_dir() || !has_skill_md(&path) {
                continue;
            }
            let (name, description) = parse_skill_md(&path.join("SKILL.md"), &fname);
            map.insert(fname.clone(), SkillSummary {
                name,
                dir_name: fname.clone(),
                description,
                in_central: true,
                central_path: Some(path.to_string_lossy().to_string()),
                editable: true,
                agents: Vec::new(),
            });
        }
    }

    // 2. 各 agent 根目录的 skill，关联或新建条目
    //    原生读取中心库的 agent 不遍历其 root_path（它直接读中心库，目录里不该有副本）
    for (agent_name, root, native) in &enabled_agents {
        if *native {
            continue;
        }
        if let Ok(entries) = std::fs::read_dir(root) {
            for entry in entries.flatten() {
                let path = entry.path();
                let fname = entry.file_name().to_string_lossy().to_string();
                if should_skip(&fname) {
                    continue;
                }
                // 软链接可能指向目录；用 symlink_metadata 不跟随，但有效性看是否含 SKILL.md（跟随）
                let is_link = std::fs::symlink_metadata(&path)
                    .map(|m| m.file_type().is_symlink())
                    .unwrap_or(false);
                if !is_link && !path.is_dir() {
                    continue;
                }
                // 非链接的真实目录要求含 SKILL.md；链接则看目标
                if !is_link && !has_skill_md(&path) {
                    continue;
                }

                let central_skill = central.join(&fname);
                let (state, link_target) = detect_state(&path, &central_skill);

                let entry_summary = map.entry(fname.clone()).or_insert_with(|| {
                    // agent 独有（中心库没有）：从该副本读 frontmatter，不可编辑
                    let (name, description) = parse_skill_md(&path.join("SKILL.md"), &fname);
                    SkillSummary {
                        name,
                        dir_name: fname.clone(),
                        description,
                        in_central: false,
                        central_path: None,
                        editable: false,
                        agents: Vec::new(),
                    }
                });
                entry_summary.agents.push(AgentSkillState {
                    agent: agent_name.to_string(),
                    state,
                    path: Some(path.to_string_lossy().to_string()),
                    link_target,
                });
            }
        }
    }

    // 3. 补齐：每个 skill 在所有 enabled agent 上都要有一行，保证前端矩阵对齐
    //    - 原生 agent：中心库有该 skill → Native（全启）；否则 Absent
    //    - 非原生 agent：缺的填 Absent
    for summary in map.values_mut() {
        for (agent_name, _, native) in &enabled_agents {
            if !summary.agents.iter().any(|a| a.agent == *agent_name) {
                let state = if *native && summary.in_central {
                    SkillState::Native
                } else {
                    SkillState::Absent
                };
                summary.agents.push(AgentSkillState {
                    agent: agent_name.to_string(),
                    state,
                    path: None,
                    link_target: None,
                });
            }
        }
        // 固定 agent 顺序，便于前端展示
        summary.agents.sort_by(|a, b| a.agent.cmp(&b.agent));
    }

    map.into_values().collect()
}

/// 扫描 Claude 插件携带的 skill（只读）
pub fn scan_plugin_skills() -> Vec<PluginSkill> {
    let mut out = Vec::new();
    let plugins_root = expand_path("~/.claude/plugins");
    let installed = plugins_root.join("installed_plugins.json");
    let Ok(content) = std::fs::read_to_string(&installed) else {
        return out;
    };
    let Ok(json) = serde_json::from_str::<serde_json::Value>(&content) else {
        return out;
    };

    // installed_plugins.json 结构多样，尽量宽容地找 installPath
    collect_install_paths(&json, &mut out);
    out
}

/// 从 installed_plugins.json 递归找 installPath，扫其 skills/ 子目录
fn collect_install_paths(json: &serde_json::Value, out: &mut Vec<PluginSkill>) {
    match json {
        serde_json::Value::Object(map) => {
            if let Some(install_path) = map.get("installPath").and_then(|v| v.as_str()) {
                let plugin_name = map
                    .get("name")
                    .and_then(|v| v.as_str())
                    .unwrap_or("unknown")
                    .to_string();
                let skills_dir = PathBuf::from(install_path).join("skills");
                if let Ok(entries) = std::fs::read_dir(&skills_dir) {
                    for entry in entries.flatten() {
                        let path = entry.path();
                        let fname = entry.file_name().to_string_lossy().to_string();
                        if should_skip(&fname) || !path.is_dir() || !has_skill_md(&path) {
                            continue;
                        }
                        let (name, description) = parse_skill_md(&path.join("SKILL.md"), &fname);
                        out.push(PluginSkill {
                            name,
                            description,
                            plugin: plugin_name.clone(),
                            path: path.to_string_lossy().to_string(),
                        });
                    }
                }
            }
            for v in map.values() {
                collect_install_paths(v, out);
            }
        }
        serde_json::Value::Array(arr) => {
            for v in arr {
                collect_install_paths(v, out);
            }
        }
        _ => {}
    }
}
