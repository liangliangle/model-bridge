import { useEffect, useState } from "react";
import { invoke } from "../lib/tauri";

type SkillState = "native" | "linked" | "external" | "real_dir" | "broken" | "absent";

interface AgentSkillState {
  agent: string;
  state: SkillState;
  path: string | null;
  link_target: string | null;
}

interface SkillSummary {
  name: string;
  dir_name: string;
  description: string;
  in_central: boolean;
  central_path: string | null;
  editable: boolean;
  agents: AgentSkillState[];
}

interface PluginSkill {
  name: string;
  description: string;
  plugin: string;
  path: string;
}

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
  skills: SkillSummary[];
  plugin_skills: PluginSkill[];
}

const STATE_LABEL: Record<SkillState, { text: string; cls: string }> = {
  native: { text: "原生·全启", cls: "text-cyan-400" },
  linked: { text: "已启用", cls: "text-emerald-400" },
  external: { text: "外部链接", cls: "text-orange-400" },
  real_dir: { text: "独立目录", cls: "text-amber-400" },
  broken: { text: "悬空", cls: "text-red-400" },
  absent: { text: "未启用", cls: "text-th-text-m" },
};

export default function SkillConfig() {
  const [data, setData] = useState<SkillsData | null>(null);
  const [selected, setSelected] = useState<SkillSummary | null>(null);
  const [loading, setLoading] = useState(false);
  const [message, setMessage] = useState("");
  const [editing, setEditing] = useState(false);
  const [editContent, setEditContent] = useState("");
  const [saving, setSaving] = useState(false);

  useEffect(() => { load(); }, []);

  const load = async () => {
    setLoading(true);
    try {
      const d = await invoke<SkillsData>("get_skills");
      setData(d);
      // 保持选中项同步
      if (selected) {
        const fresh = d.skills.find((s) => s.dir_name === selected.dir_name);
        setSelected(fresh ?? null);
      }
    } catch (e) { console.error(e); }
    finally { setLoading(false); }
  };

  const showMessage = (m: string) => { setMessage(m); setTimeout(() => setMessage(""), 3000); };

  const handleToggle = async (skill: SkillSummary, agent: string, enabled: boolean) => {
    try {
      const res = await invoke<{ success?: boolean; error?: string }>("toggle_skill", { skill: skill.dir_name, agent, enabled });
      if (res.error) { showMessage(res.error); return; }
      showMessage(enabled ? "已启用" : "已禁用");
      await load();
    } catch (e) { showMessage(`操作失败: ${e}`); }
  };

  const handleImport = async (skill: SkillSummary, agent: string) => {
    if (!confirm(`将 "${skill.dir_name}" 从 ${agent} 移动到中心库并建立软链接？此操作会移动真实目录。`)) return;
    try {
      const res = await invoke<{ success?: boolean; error?: string }>("import_skill", { skill: skill.dir_name, agent });
      if (res.error) { showMessage(res.error); return; }
      showMessage("已纳入管理");
      await load();
    } catch (e) { showMessage(`纳管失败: ${e}`); }
  };

  const openEdit = async (skill: SkillSummary) => {
    try {
      const res = await invoke<{ content?: string; error?: string }>("get_skill_content", { name: skill.dir_name });
      if (res.error || res.content == null) { showMessage(res.error ?? "读取失败"); return; }
      setEditContent(res.content);
      setEditing(true);
    } catch (e) { showMessage(`读取失败: ${e}`); }
  };

  const saveEdit = async () => {
    if (!selected) return;
    setSaving(true);
    try {
      const res = await invoke<{ success?: boolean; error?: string }>("save_skill_content", { name: selected.dir_name, content: editContent });
      if (res.error) { showMessage(res.error); setSaving(false); return; }
      showMessage("保存成功"); setEditing(false); await load();
    } catch (e) { showMessage(`保存失败: ${e}`); }
    finally { setSaving(false); }
  };

  const enabledCount = (s: SkillSummary) => s.agents.filter((a) => a.state !== "absent").length;

  return (
    <div>
      <div className="flex items-center justify-between mb-4">
        <h2 className="font-display text-2xl font-semibold tracking-tight">Skill 管理</h2>
        <div className="flex items-center gap-3">
          {message && <span className="text-xs px-3 py-1.5 rounded-lg bg-emerald-500/10 text-emerald-400 border border-emerald-500/20">{message}</span>}
          <button onClick={load} disabled={loading} className="btn-ghost text-xs">{loading ? "扫描中..." : "↻ 刷新扫描"}</button>
        </div>
      </div>

      <div className="flex gap-4">
        {/* skill 列表 */}
        <div className="w-60 shrink-0">
          <div className="glass-card p-3 mb-2 space-y-0.5 max-h-[70vh] overflow-auto">
            {data?.skills.map((s) => (
              <div
                key={s.dir_name}
                onClick={() => { setSelected(s); setEditing(false); }}
                className={`px-3 py-2.5 rounded-lg cursor-pointer text-sm transition-all duration-200 ${
                  selected?.dir_name === s.dir_name ? "bg-th-accent-g text-th-accent" : "text-th-text-s hover:text-th-text hover:bg-th-hover"
                }`}
              >
                <div className="flex items-center gap-2">
                  <span className="font-medium truncate flex-1">{s.name}</span>
                  {!s.in_central && <span className="text-[9px] px-1 rounded bg-th-elev text-th-text-m shrink-0">未纳管</span>}
                </div>
                <div className="text-[10px] text-th-text-m ml-0 font-mono">{enabledCount(s)}/{s.agents.length} agent 启用</div>
              </div>
            ))}
            {(!data || data.skills.length === 0) && <div className="text-center text-th-text-m text-xs py-4">无 skill</div>}
          </div>
        </div>

        {/* 详情面板 */}
        <div className="flex-1 glass-card p-5 overflow-auto animate-fade-in">
          {!selected ? (
            <div className="flex items-center justify-center h-64 text-th-text-m text-sm">选择一个 skill 查看</div>
          ) : editing ? (
            <div>
              <h3 className="font-display font-semibold text-lg mb-4 text-th-text">编辑 SKILL.md — {selected.name}</h3>
              <textarea
                className="input font-mono text-xs !h-[55vh] resize-none"
                value={editContent}
                onChange={(e) => setEditContent(e.target.value)}
              />
              <div className="flex gap-3 mt-4">
                <button onClick={saveEdit} disabled={saving} className="btn-primary">{saving ? "保存中..." : "保存"}</button>
                <button onClick={() => setEditing(false)} className="btn-ghost">取消</button>
              </div>
            </div>
          ) : (
            <SkillView
              skill={selected}
              onToggle={handleToggle}
              onImport={handleImport}
              onEdit={() => openEdit(selected)}
            />
          )}
        </div>
      </div>

      {/* 插件 skill（只读）*/}
      {data && data.plugin_skills.length > 0 && (
        <div className="mt-6">
          <h3 className="font-display font-medium mb-2 text-th-text flex items-center gap-2">
            插件 Skill <span className="text-xs text-th-text-m font-body font-normal">（由插件系统管理，只读）</span>
          </h3>
          <div className="glass-card p-3 grid grid-cols-2 lg:grid-cols-3 gap-2">
            {data.plugin_skills.map((p, i) => (
              <div key={i} className="px-3 py-2 rounded-lg" style={{ background: "var(--color-bg-elevated)" }}>
                <div className="text-sm text-th-text truncate">{p.name}</div>
                <div className="text-[10px] text-th-text-m font-mono">{p.plugin}</div>
              </div>
            ))}
          </div>
        </div>
      )}
    </div>
  );
}

function SkillView({ skill, onToggle, onImport, onEdit }: {
  skill: SkillSummary;
  onToggle: (s: SkillSummary, agent: string, enabled: boolean) => void;
  onImport: (s: SkillSummary, agent: string) => void;
  onEdit: () => void;
}) {
  return (
    <div>
      <div className="flex items-center justify-between mb-4">
        <h3 className="font-display font-semibold text-lg text-th-text">{skill.name}</h3>
        {skill.editable && <button onClick={onEdit} className="btn-primary text-xs">编辑 SKILL.md</button>}
      </div>

      {skill.description && <p className="text-sm text-th-text-s mb-4 leading-relaxed">{skill.description}</p>}

      <div className="grid grid-cols-2 gap-x-6 gap-y-2 text-sm mb-5">
        <InfoField label="目录名" value={skill.dir_name} mono />
        <InfoField label="是否在中心库" value={skill.in_central ? "是（可编辑/启停）" : "否（未纳管）"} />
        {skill.central_path && <InfoField label="中心库路径" value={skill.central_path} mono />}
      </div>

      {/* 跨 agent 启用矩阵 */}
      <h4 className="font-display font-medium text-sm text-th-text mb-2">跨 Agent 启用矩阵</h4>
      <div className="rounded-lg overflow-hidden" style={{ border: "1px solid var(--color-border)" }}>
        <table className="w-full text-sm">
          <thead style={{ background: "var(--color-bg-elevated)" }}>
            <tr className="border-b border-th-border text-[10px] uppercase tracking-wider text-th-text-m">
              <th className="px-3 py-2 text-left">Agent</th>
              <th className="px-3 py-2 text-left">状态</th>
              <th className="px-3 py-2 text-right">操作</th>
            </tr>
          </thead>
          <tbody>
            {skill.agents.map((a) => {
              const st = STATE_LABEL[a.state];
              return (
                <tr key={a.agent} className="border-b border-th-border last:border-0">
                  <td className="px-3 py-2 font-medium text-th-text">{a.agent}</td>
                  <td className="px-3 py-2">
                    <span className={`text-xs ${st.cls}`}>{st.text}</span>
                    {a.link_target && a.state !== "linked" && (
                      <span className="text-[10px] text-th-text-m ml-2 font-mono">→ {a.link_target}</span>
                    )}
                  </td>
                  <td className="px-3 py-2 text-right">
                    {/* native：原生读取中心库，不可单独启停 */}
                    {a.state === "native" && (
                      <span className="text-[10px] text-th-text-m">自动生效</span>
                    )}
                    {/* absent / linked：开关（需中心库有源）*/}
                    {(a.state === "absent" || a.state === "linked") && skill.in_central && (
                      <Toggle on={a.state === "linked"} onChange={(v) => onToggle(skill, a.agent, v)} />
                    )}
                    {/* absent 但不在中心库：无法启用 */}
                    {a.state === "absent" && !skill.in_central && (
                      <span className="text-[10px] text-th-text-m">需先纳管</span>
                    )}
                    {/* real_dir：纳入管理 */}
                    {a.state === "real_dir" && (
                      <button onClick={() => onImport(skill, a.agent)} className="text-th-accent text-xs hover:opacity-80">纳入管理</button>
                    )}
                    {/* broken：清理 */}
                    {a.state === "broken" && (
                      <button onClick={() => onToggle(skill, a.agent, false)} className="text-red-400 text-xs hover:opacity-80">清理</button>
                    )}
                    {/* external：只读提示 */}
                    {a.state === "external" && <span className="text-[10px] text-th-text-m">外部链接</span>}
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>
      </div>
      <p className="text-[10px] text-th-text-m mt-2">启用=在该 agent 目录建软链接指向中心库；禁用=删软链接（中心库目录保留）</p>
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

function InfoField({ label, value, mono }: { label: string; value: string; mono?: boolean }) {
  return (
    <div className="py-1.5 border-b border-th-border">
      <div className="text-[10px] text-th-text-m uppercase tracking-wider mb-0.5">{label}</div>
      <div className={`text-sm text-th-text ${mono ? "font-mono" : ""} truncate`}>{value}</div>
    </div>
  );
}
