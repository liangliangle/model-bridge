//! 构建脚本：在编译时将前端 dist/ 目录的所有文件嵌入到二进制中
//!
//! 生成 embedded_assets.rs，包含一个 get_embedded_file(path) 函数，
//! 通过 include_bytes! 宏将每个文件的内容编译为静态字节数组。
//! 运行时通过路径查找即可直接返回文件内容，无需外部 dist/ 目录。

use std::env;
use std::fs;
use std::path::{Path, PathBuf};

fn main() {
    let out_dir = env::var("OUT_DIR").unwrap();
    let dest_path = PathBuf::from(&out_dir).join("embedded_assets.rs");
    let dist_dir = PathBuf::from("../dist");

    if !dist_dir.is_dir() {
        // dist 目录不存在时生成空实现，运行时会显示提示信息
        fs::write(&dest_path, r#"
pub fn get_embedded_file(path: &str) -> Option<(&'static [u8], &'static str)> {
    None
}
"#).unwrap();
        return;
    }

    // 递归收集 dist/ 下所有文件
    let mut entries: Vec<(String, PathBuf)> = Vec::new();
    collect_files(&dist_dir, &dist_dir, &mut entries);

    // 生成 match 语句，每个文件路径对应一个 include_bytes! 分支
    let mut code = String::new();
    code.push_str("pub fn get_embedded_file(path: &str) -> Option<(&'static [u8], &'static str)> {\n");
    code.push_str("    match path {\n");

    for (web_path, file_path) in &entries {
        let abs_path = fs::canonicalize(file_path).unwrap();
        let abs_str = abs_path.to_str().unwrap().replace('\\', "/");
        let mime = guess_mime(file_path);

        // 生成: "index.html" => Some((include_bytes!("/abs/path/index.html"), "text/html")),
        code.push_str(&format!(
            "        \"{}\" => Some((include_bytes!(\"{}\"), \"{}\")),\n",
            web_path, abs_str, mime
        ));
    }

    code.push_str("        _ => None,\n");
    code.push_str("    }\n");
    code.push_str("}\n");

    fs::write(&dest_path, code).unwrap();

    // dist 目录变化时重新构建
    println!("cargo:rerun-if-changed=../dist");
}

/// 递归收集目录下所有文件，记录 (相对路径, 绝对路径)
fn collect_files(base: &Path, dir: &Path, entries: &mut Vec<(String, PathBuf)>) {
    if let Ok(read_dir) = fs::read_dir(dir) {
        for entry in read_dir.flatten() {
            let path = entry.path();
            if path.is_dir() {
                collect_files(base, &path, entries);
            } else {
                let rel = path.strip_prefix(base).unwrap();
                let web_path = rel.to_str().unwrap().replace('\\', "/");
                entries.push((web_path, path));
            }
        }
    }
}

/// 根据文件扩展名推断 MIME 类型
fn guess_mime(path: &Path) -> &'static str {
    match path.extension().and_then(|e| e.to_str()) {
        Some("html") => "text/html; charset=utf-8",
        Some("js") => "application/javascript; charset=utf-8",
        Some("css") => "text/css; charset=utf-8",
        Some("json") => "application/json",
        Some("png") => "image/png",
        Some("svg") => "image/svg+xml",
        Some("ico") => "image/x-icon",
        Some("woff2") => "font/woff2",
        Some("woff") => "font/woff",
        Some("ttf") => "font/ttf",
        Some("map") => "application/json",
        _ => "application/octet-stream",
    }
}
