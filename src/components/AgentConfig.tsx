import { useEffect, useState } from "react";
import { invoke } from "../lib/tauri";

interface AgentTarget {
  name: string;
  root_path: string;
  enabled: boolean;
  builtin: boolean;
  native_agents_dir: boolean;
}

interface SkillsData {
  central_lib: string;
  agents: AgentTarget[];
}

export default function AgentConfig() {
  const [centralLib, setCentralLib] = useState("");
  const [centralDraft, setCentralDraft] = useState("");
  const [agents, setAgents] = useState<AgentTarget[]>([]);
  const [loading, setLoading] = useState(false);
  const [message, setMessage] = useState("");
  // 新增 agent 暂存
  const [newName, setNewName] = useState("");
  const [newPath, setNewPath] = useState("");

  useEffect(() => { load(); }, []);

  const load = async () => {
    setLoading(true);
    try {
      const d = await invoke<SkillsData>("get_skills");
      setCentralLib(d.central_lib);
      setCentralDraft(d.central_lib);
      setAgents(d.agents);
    } catch (e) { console.error(e); }
    finally { setLoading(false); }
  };

  const showMessage = (m: string) => { setMessage(m); setTimeout(() => setMessage(""), 3000); };

  const saveCentral = async () => {
    if (!centralDraft.trim()) { showMessage("中心库路径不能为空"); return; }
    try {
      const res = await invoke<{ success?: boolean; error?: string }>("save_skill_central", { centralLib: centralDraft.trim() });
      if (res.error) { showMessage(res.error); return; }
      showMessage("中心库路径已保存"); await load();
    } catch (e) { showMessage(`保存失败: ${e}`); }
  };

  const saveAgent = async (target: AgentTarget) => {
    try {
      const res = await invoke<{ success?: boolean; error?: string }>("save_skill_agent", { target });
      if (res.error) { showMessage(res.error); return; }
      await load();
    } catch (e) { showMessage(`保存失败: ${e}`); }
  };

  const deleteAgent = async (name: string) => {
    if (!confirm(`确定删除扫描目标 "${name}" ?`)) return;
    try {
      const res = await invoke<{ success?: boolean; error?: string }>("delete_skill_agent", { name });
      if (res.error) { showMessage(res.error); return; }
      showMessage("已删除"); await load();
    } catch (e) { showMessage(`删除失败: ${e}`); }
  };

  const addAgent = async () => {
    const name = newName.trim(), root_path = newPath.trim();
    if (!name || !root_path) { showMessage("名称和路径不能为空"); return; }
    if (agents.some((a) => a.name === name)) { showMessage("该名称已存在"); return; }
    await saveAgent({ name, root_path, enabled: true, builtin: false, native_agents_dir: false });
    setNewName(""); setNewPath("");
  };

  // 本地改某 agent 字段后立即保存
  const updateField = (a: AgentTarget, patch: Partial<AgentTarget>) => saveAgent({ ...a, ...patch });

  return (
    <div>
      <div className="flex items-center justify-between mb-4">
        <h2 className="font-display text-2xl font-semibold tracking-tight">Agent 管理</h2>
        <div className="flex items-center gap-3">
          {message && <span className="text-xs px-3 py-1.5 rounded-lg bg-emerald-500/10 text-emerald-400 border border-emerald-500/20">{message}</span>}
          <button onClick={load} disabled={loading} className="btn-ghost text-xs">{loading ? "加载中..." : "↻ 刷新"}</button>
        </div>
      </div>

      {/* 中心库路径 */}
      <div className="glass-card p-4 mb-5">
        <h3 className="font-display font-medium text-sm text-th-text mb-2">中心库路径</h3>
        <p className="text-[10px] text-th-text-m mb-2">被管理 skill 的真实存储；启用=在 agent 目录建软链接指向这里</p>
        <div className="flex gap-2">
          <input className="input font-mono text-sm" value={centralDraft} onChange={(e) => setCentralDraft(e.target.value)} placeholder="~/.agents/skills" />
          <button onClick={saveCentral} disabled={centralDraft.trim() === centralLib} className="btn-primary text-xs shrink-0">保存</button>
        </div>
      </div>

      {/* 扫描目标列表 */}
      <h3 className="font-display font-medium mb-2 text-th-text">扫描目标（Agent）</h3>
      <div className="glass-card overflow-hidden">
        <table className="w-full text-sm">
          <thead style={{ background: "var(--color-bg-elevated)" }}>
            <tr className="border-b border-th-border text-[10px] uppercase tracking-wider text-th-text-m">
              <th className="px-3 py-2.5 text-left">名称</th>
              <th className="px-3 py-2.5 text-left">Skill 根目录</th>
              <th className="px-3 py-2.5 text-center w-24">纳入 Skill 管理</th>
              <th className="px-3 py-2.5 text-center w-28">原生读中心库</th>
              <th className="px-3 py-2.5 text-right w-16"></th>
            </tr>
          </thead>
          <tbody>
            {agents.map((a) => (
              <tr key={a.name} className={`border-b border-th-border last:border-0 ${a.enabled ? "" : "opacity-50"}`}>
                <td className="px-3 py-2.5">
                  <span className="font-medium text-th-text">{a.name}</span>
                  {a.builtin && <span className="text-[9px] px-1 ml-2 rounded bg-th-elev text-th-text-m">内置</span>}
                  {!a.enabled && <span className="text-[9px] px-1 ml-2 rounded bg-th-elev text-amber-400">已禁用</span>}
                </td>
                <td className="px-3 py-2.5">
                  <input
                    className="input font-mono text-xs !py-1"
                    defaultValue={a.root_path}
                    onBlur={(e) => { if (e.target.value.trim() && e.target.value !== a.root_path) updateField(a, { root_path: e.target.value.trim() }); }}
                  />
                </td>
                <td className="px-3 py-2.5 text-center">
                  <Toggle on={a.enabled} onChange={(v) => updateField(a, { enabled: v })} />
                </td>
                <td className="px-3 py-2.5 text-center">
                  <Toggle on={a.native_agents_dir} onChange={(v) => updateField(a, { native_agents_dir: v })} />
                </td>
                <td className="px-3 py-2.5 text-right">
                  <button onClick={() => deleteAgent(a.name)} className="text-red-400 hover:text-red-300 text-xs">删除</button>
                </td>
              </tr>
            ))}
            {/* 新增行 */}
            <tr style={{ background: "var(--color-bg-hover)" }}>
              <td className="px-3 py-2.5">
                <input className="input text-xs !py-1" value={newName} onChange={(e) => setNewName(e.target.value)} placeholder="agent 名" />
              </td>
              <td className="px-3 py-2.5">
                <input className="input font-mono text-xs !py-1" value={newPath} onChange={(e) => setNewPath(e.target.value)} placeholder="~/.xxx/skills" onKeyDown={(e) => e.key === "Enter" && addAgent()} />
              </td>
              <td colSpan={2}></td>
              <td className="px-3 py-2.5 text-right">
                <button onClick={addAgent} className="text-th-accent text-xs font-medium hover:text-th-accent-d">添加</button>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
      <p className="text-[10px] text-th-text-m mt-2">「纳入 Skill 管理」关闭后，该 agent 将从 Skill 管理页的矩阵中移除，不再被扫描。删除后可通过下方表单重新添加。</p>
      <p className="text-[10px] text-th-text-m mt-1">「原生读中心库」开启后，该 agent 直接读取中心库，skill 自动全部生效，不再建软链接（避免重复）。</p>
    </div>
  );
}

function Toggle({ on, onChange }: { on: boolean; onChange: (v: boolean) => void }) {
  return (
    <div className={`inline-block w-8 h-4 rounded-full relative cursor-pointer transition-colors ${on ? "" : "bg-th-text-m"}`}
      style={on ? { background: "var(--color-accent)" } : {}}
      onClick={() => onChange(!on)}>
      <div className={`w-3.5 h-3.5 rounded-full bg-white absolute top-0.5 transition-all ${on ? "left-4" : "left-0.5"}`} />
    </div>
  );
}
