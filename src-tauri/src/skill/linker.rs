//! Skill 软链接操作：启用/禁用/分发/纳管 + SKILL.md 读写。
//! 安全是核心：禁用只删软链接绝不删真实目录；编辑只碰 SKILL.md 防路径穿越。

use std::path::{Path, PathBuf};

use super::scanner::expand_path;

/// 校验 skill 名合法（防路径穿越）
fn validate_skill_name(name: &str) -> Result<(), String> {
    if name.is_empty()
        || name.contains('/')
        || name.contains('\\')
        || name.contains("..")
        || name.starts_with('.')
    {
        return Err(format!("非法 skill 名: {}", name));
    }
    Ok(())
}

/// 启用：在 agent 根目录建软链接指向中心库的 skill
pub fn enable(central_lib: &str, agent_root: &str, skill: &str, force: bool) -> Result<(), String> {
    validate_skill_name(skill)?;
    let central = expand_path(central_lib);
    let src = central.join(skill);
    if !src.join("SKILL.md").is_file() {
        return Err(format!("中心库不存在该 skill: {}", skill));
    }
    let root = expand_path(agent_root);
    std::fs::create_dir_all(&root).map_err(|e| format!("创建 agent 目录失败: {}", e))?;
    let link = root.join(skill);

    match std::fs::symlink_metadata(&link) {
        Ok(meta) => {
            if meta.file_type().is_symlink() {
                // 已是链接：指向中心库则幂等成功；否则需 force
                let points_central = matches!(
                    (link.canonicalize(), src.canonicalize()),
                    (Ok(a), Ok(b)) if a == b
                );
                if points_central {
                    return Ok(()); // 幂等
                }
                if !force {
                    return Err("目标已存在指向别处的软链接，使用 force 覆盖".to_string());
                }
                std::fs::remove_file(&link).map_err(|e| format!("删除旧链接失败: {}", e))?;
            } else {
                // 真实目录/文件：绝不覆盖
                return Err("目标已存在真实目录，请先纳入管理（import）".to_string());
            }
        }
        Err(_) => { /* 不存在，直接建链 */ }
    }

    create_symlink(&src, &link)
}

/// 禁用：删除 agent 根目录下的软链接（绝不删真实目录）
pub fn disable(agent_root: &str, skill: &str) -> Result<(), String> {
    validate_skill_name(skill)?;
    let link = expand_path(agent_root).join(skill);
    let meta = match std::fs::symlink_metadata(&link) {
        Ok(m) => m,
        Err(_) => return Ok(()), // 本就不存在，视为已禁用
    };
    // 关键安全点：只删软链接，绝不 remove_dir_all 真实目录
    if !meta.file_type().is_symlink() {
        return Err("拒绝：这是真实目录而非软链接，不会删除".to_string());
    }
    std::fs::remove_file(&link).map_err(|e| format!("删除链接失败: {}", e))
}

/// 纳入管理：把 agent 下的真实 skill 目录移到中心库，再原位建软链接
pub fn import(central_lib: &str, agent_root: &str, skill: &str) -> Result<(), String> {
    validate_skill_name(skill)?;
    let central = expand_path(central_lib);
    let dst = central.join(skill);
    if dst.exists() {
        return Err(format!("中心库已存在同名 skill: {}，请改名或手动处理", skill));
    }
    let root = expand_path(agent_root);
    let real = root.join(skill);
    let meta = std::fs::symlink_metadata(&real).map_err(|e| format!("源不存在: {}", e))?;
    if meta.file_type().is_symlink() || !meta.is_dir() {
        return Err("源不是真实目录，无需纳管".to_string());
    }
    if !real.join("SKILL.md").is_file() {
        return Err("源目录不含 SKILL.md".to_string());
    }
    std::fs::create_dir_all(&central).map_err(|e| format!("创建中心库失败: {}", e))?;
    // 移动真实目录到中心库
    std::fs::rename(&real, &dst).map_err(|e| format!("移动到中心库失败: {}", e))?;
    // 原位建软链接
    create_symlink(&dst, &real)
}

/// 读取中心库 skill 的 SKILL.md 内容
pub fn read_skill_md(central_lib: &str, skill: &str) -> Result<String, String> {
    let path = safe_skill_md_path(central_lib, skill)?;
    std::fs::read_to_string(&path).map_err(|e| format!("读取 SKILL.md 失败: {}", e))
}

/// 写入中心库 skill 的 SKILL.md 内容（仅此文件，防穿越）
pub fn write_skill_md(central_lib: &str, skill: &str, content: &str) -> Result<(), String> {
    let path = safe_skill_md_path(central_lib, skill)?;
    // 写前校验 frontmatter 合法（若有）
    validate_frontmatter(content)?;
    std::fs::write(&path, content).map_err(|e| format!("写入 SKILL.md 失败: {}", e))
}

/// 计算并校验 SKILL.md 路径，确保落在中心库内、文件名严格为 SKILL.md
fn safe_skill_md_path(central_lib: &str, skill: &str) -> Result<PathBuf, String> {
    validate_skill_name(skill)?;
    let central = expand_path(central_lib);
    let skill_dir = central.join(skill);
    // 校验 skill_dir 规范化后仍在中心库内
    let central_canon = central
        .canonicalize()
        .map_err(|e| format!("中心库路径无效: {}", e))?;
    let dir_canon = skill_dir
        .canonicalize()
        .map_err(|e| format!("skill 目录无效: {}", e))?;
    if !dir_canon.starts_with(&central_canon) {
        return Err("路径越界，拒绝访问".to_string());
    }
    Ok(dir_canon.join("SKILL.md"))
}

/// 校验 frontmatter 是合法 YAML（若文件以 --- 开头）
fn validate_frontmatter(content: &str) -> Result<(), String> {
    let trimmed = content.trim_start();
    if !trimmed.starts_with("---") {
        return Ok(()); // 无 frontmatter，放行
    }
    let after_first = match trimmed.find('\n') {
        Some(i) => &trimmed[i + 1..],
        None => return Ok(()),
    };
    let yaml = match after_first.find("\n---") {
        Some(i) => &after_first[..i],
        None => return Ok(()),
    };
    serde_yaml::from_str::<serde_yaml::Value>(yaml)
        .map(|_| ())
        .map_err(|e| format!("frontmatter YAML 非法: {}", e))
}

/// 创建软链接（仅 unix）
#[cfg(unix)]
fn create_symlink(src: &Path, link: &Path) -> Result<(), String> {
    std::os::unix::fs::symlink(src, link).map_err(|e| format!("创建软链接失败: {}", e))
}

#[cfg(not(unix))]
fn create_symlink(_src: &Path, _link: &Path) -> Result<(), String> {
    Err("当前平台暂不支持软链接管理".to_string())
}
