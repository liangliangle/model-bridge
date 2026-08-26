import { useEffect, useState } from "react";
import { invoke } from "../lib/tauri";

interface ToolInfo {
  name: string;
  description: string | null;
  input_schema?: unknown;
}

interface McpServerEditData {
  id: string;
  name: string;
  endpoint: string;
  auth_token: string | null;
  custom_headers: Record<string, string>;
  blocked_tools: string[];
  enabled: boolean;
  oauth_enabled: boolean;
  cached_tools: ToolInfo[];
}

interface FullConfig {
  listen_host: string;
  listen_port: number;
  mcp_servers: McpServerEditData[];
}

function emptyServer(): McpServerEditData {
  return { id: "", name: "", endpoint: "", auth_token: null, custom_headers: {}, blocked_tools: [], enabled: true, oauth_enabled: false, cached_tools: [] };
}

export default function McpConfig() {
  const [config, setConfig] = useState<FullConfig | null>(null);
  const [selected, setSelected] = useState<McpServerEditData | null>(null);
  const [editing, setEditing] = useState(false);
  const [editData, setEditData] = useState<McpServerEditData>(emptyServer());
  const [newTool, setNewTool] = useState("");
  const [newHeaderKey, setNewHeaderKey] = useState("");
  const [newHeaderVal, setNewHeaderVal] = useState("");
  const [saving, setSaving] = useState(false);
  const [message, setMessage] = useState("");

  useEffect(() => { loadConfig(); }, []);

  const loadConfig = async () => {
    try {
      const data = await invoke<FullConfig>("get_full_config");
      setConfig(data);
      setSelected((prev) => {
        if (!prev) return data.mcp_servers.length > 0 ? data.mcp_servers[0] : null;
        // 已有选中项：用最新数据里同 id 的项刷新（否则 cached_tools 等更新读不到）
        return data.mcp_servers.find((s) => s.id === prev.id) ?? prev;
      });
    } catch (e) { console.error(e); }
  };

  const baseUrl = config ? `http://${config.listen_host === "0.0.0.0" ? "localhost" : config.listen_host}:${config.listen_port}` : "";

  const resetPending = () => { setNewTool(""); setNewHeaderKey(""); setNewHeaderVal(""); };
  const handleSelect = (s: McpServerEditData) => { setSelected(s); setEditing(false); };
  const handleEdit = () => { if (!selected) return; setEditData({ ...selected, custom_headers: { ...(selected.custom_headers || {}) }, blocked_tools: [...(selected.blocked_tools || [])] }); resetPending(); setEditing(true); };
  const handleNew = () => { setEditData(emptyServer()); resetPending(); setEditing(true); setSelected(null); };

  const handleDelete = async () => {
    if (!selected || !confirm(`确定删除 MCP server "${selected.name}" ?`)) return;
    try { await invoke("delete_mcp_server", { serverId: selected.id }); setSelected(null); setEditing(false); showMessage("删除成功"); loadConfig(); }
    catch (e) { showMessage(`删除失败: ${e}`); }
  };

  const handleSave = async () => {
    if (!editData.id.trim() || !editData.name.trim() || !editData.endpoint.trim()) { showMessage("ID、名称、Endpoint 不能为空"); return; }
    // 合并未点"添加"的暂存行
    const merged: McpServerEditData = {
      ...editData,
      blocked_tools: [...editData.blocked_tools],
      custom_headers: { ...(editData.custom_headers || {}) },
    };
    if (newTool.trim() && !merged.blocked_tools.includes(newTool.trim())) merged.blocked_tools.push(newTool.trim());
    if (newHeaderKey.trim() && newHeaderVal.trim()) merged.custom_headers[newHeaderKey.trim()] = newHeaderVal.trim();
    setSaving(true);
    try { await invoke("save_mcp_server", { server: merged }); resetPending(); setEditing(false); showMessage("保存成功"); loadConfig(); setSelected(merged); }
    catch (e) { showMessage(`保存失败: ${e}`); }
    finally { setSaving(false); }
  };

  const showMessage = (msg: string) => { setMessage(msg); setTimeout(() => setMessage(""), 3000); };

  return (
    <div>
      <div className="flex items-center justify-between mb-4">
        <h2 className="font-display text-2xl font-semibold tracking-tight">MCP 中继</h2>
        {message && <span className="text-xs px-3 py-1.5 rounded-lg bg-emerald-500/10 text-emerald-400 border border-emerald-500/20">{message}</span>}
      </div>

      <div className="flex gap-4">
        {/* server 列表 */}
        <div className="w-56 shrink-0">
          <div className="glass-card p-3 mb-2 space-y-0.5">
            {config?.mcp_servers.map((s) => (
              <div
                key={s.id}
                onClick={() => handleSelect(s)}
                className={`px-3 py-2.5 rounded-lg cursor-pointer text-sm transition-all duration-200 ${
                  selected?.id === s.id ? "bg-th-accent-g text-th-accent" : "text-th-text-s hover:text-th-text hover:bg-th-hover"
                }`}
              >
                <div className="flex items-center gap-2">
                  <span className={`w-2 h-2 rounded-full ${s.enabled ? "bg-emerald-400" : "bg-th-text-m"}`} />
                  <span className="font-medium truncate">{s.name}</span>
                </div>
                <div className="text-[10px] text-th-text-m ml-4 font-mono">{s.blocked_tools?.length || 0} 个黑名单工具</div>
              </div>
            ))}
            {(!config || config.mcp_servers.length === 0) && <div className="text-center text-th-text-m text-xs py-4">暂无 MCP server</div>}
          </div>
          <button onClick={handleNew} className="btn-primary w-full text-xs">+ 新增 MCP server</button>
        </div>

        {/* 编辑/查看面板 */}
        <div className="flex-1 glass-card p-5 overflow-auto animate-fade-in">
          {editing ? (
            <McpForm data={editData} onChange={setEditData} onSave={handleSave} onCancel={() => setEditing(false)} saving={saving}
              newTool={newTool} onNewToolChange={setNewTool}
              newHeaderKey={newHeaderKey} newHeaderVal={newHeaderVal} onNewHeaderKeyChange={setNewHeaderKey} onNewHeaderValChange={setNewHeaderVal}
              isNew={!selected || !config?.mcp_servers.find((s) => s.id === editData.id)} />
          ) : selected ? (
            <McpView server={selected} baseUrl={baseUrl} onEdit={handleEdit} onDelete={handleDelete} onChanged={loadConfig} />
          ) : (
            <div className="flex items-center justify-center h-64 text-th-text-m text-sm">选择一个 MCP server 查看 或 点击"新增 MCP server"</div>
          )}
        </div>
      </div>
    </div>
  );
}

// ========== 查看模式 ==========
interface OauthStatus { authorized: boolean; expiresAt: number | null; needsReauth: boolean }

function McpView({ server, baseUrl, onEdit, onDelete, onChanged }: { server: McpServerEditData; baseUrl: string; onEdit: () => void; onDelete: () => void; onChanged: () => void }) {
  const accessUrl = `${baseUrl}/mcp/${server.id}`;
  const [copied, setCopied] = useState(false);
  const copy = () => { navigator.clipboard.writeText(accessUrl); setCopied(true); setTimeout(() => setCopied(false), 2000); };

  const [oauthStatus, setOauthStatus] = useState<OauthStatus | null>(null);
  const [authorizing, setAuthorizing] = useState(false);
  const [oauthMsg, setOauthMsg] = useState("");

  // 工具列表
  const [fetching, setFetching] = useState(false);
  const [toolsMsg, setToolsMsg] = useState("");
  const blockedSet = new Set(server.blocked_tools || []);
  // OAuth server 未授权时不能拉工具
  const oauthBlocked = server.oauth_enabled && !(oauthStatus?.authorized);

  const fetchTools = async () => {
    setFetching(true); setToolsMsg("");
    try {
      const res = await invoke<{ success?: boolean; tools?: ToolInfo[]; error?: string; needsAuth?: boolean }>("fetch_mcp_tools", { serverId: server.id });
      if (res.error) {
        setToolsMsg(res.needsAuth ? "请先完成 OAuth 授权再拉取工具" : `拉取失败: ${res.error}`);
      } else {
        setToolsMsg(`已拉取 ${res.tools?.length ?? 0} 个工具`);
        onChanged();
      }
    } catch (e) { setToolsMsg(`拉取失败: ${e}`); }
    finally { setFetching(false); }
  };

  const toggleTool = async (tool: string, enabled: boolean) => {
    try {
      const res = await invoke<{ success?: boolean; error?: string }>("toggle_mcp_tool", { serverId: server.id, tool, enabled });
      if (res.error) { setToolsMsg(res.error); return; }
      onChanged();
    } catch (e) { setToolsMsg(`操作失败: ${e}`); }
  };

  const loadOauthStatus = async () => {
    if (!server.oauth_enabled) return;
    try { setOauthStatus(await invoke<OauthStatus>("get_mcp_oauth_status", { serverId: server.id })); }
    catch (e) { console.error(e); }
  };

  useEffect(() => { setOauthStatus(null); setOauthMsg(""); loadOauthStatus(); /* eslint-disable-next-line */ }, [server.id, server.oauth_enabled]);

  const handleAuthorize = async () => {
    setAuthorizing(true); setOauthMsg("");
    try {
      const res = await invoke<{ authorizeUrl?: string; error?: string }>("start_mcp_oauth", { serverId: server.id });
      if (res.error || !res.authorizeUrl) { setOauthMsg(`发起授权失败: ${res.error ?? "未知错误"}`); setAuthorizing(false); return; }
      window.open(res.authorizeUrl, "_blank", "width=600,height=800");
      // 轮询授权状态
      let tries = 0;
      const timer = setInterval(async () => {
        tries++;
        try {
          const s = await invoke<OauthStatus>("get_mcp_oauth_status", { serverId: server.id });
          if (s.authorized) { clearInterval(timer); setOauthStatus(s); setAuthorizing(false); setOauthMsg("授权成功"); }
        } catch { /* ignore */ }
        if (tries > 150) { clearInterval(timer); setAuthorizing(false); } // 5 分钟超时
      }, 2000);
    } catch (e) { setOauthMsg(`发起授权失败: ${e}`); setAuthorizing(false); }
  };

  const oauthBadge = () => {
    if (!oauthStatus) return { text: "加载中…", cls: "text-th-text-m" };
    if (oauthStatus.authorized && !oauthStatus.needsReauth) return { text: "已授权", cls: "text-emerald-400" };
    if (oauthStatus.needsReauth) return { text: "已过期，需重新授权", cls: "text-red-400" };
    return { text: "未授权", cls: "text-amber-400" };
  };

  return (
    <div>
      <div className="flex items-center justify-between mb-5">
        <h3 className="font-display font-semibold text-lg text-th-text">{server.name}</h3>
        <div className="flex gap-2">
          <button onClick={onEdit} className="btn-primary text-xs">编辑</button>
          <button onClick={onDelete} className="btn-danger text-xs">删除</button>
        </div>
      </div>

      {/* 接入地址 */}
      <div className="mb-5 p-3 rounded-lg" style={{ background: "var(--color-bg-elevated)", border: "1px solid var(--color-border)" }}>
        <div className="text-[10px] text-th-text-m uppercase tracking-wider mb-1">客户端接入地址（Streamable HTTP）</div>
        <button onClick={copy} className="flex items-center gap-2 text-sm font-mono text-th-accent hover:opacity-80" title="点击复制">
          <span className="truncate">{accessUrl}</span>
          <span className="shrink-0 text-[10px]">{copied ? "✓" : "⎘"}</span>
        </button>
      </div>

      {/* OAuth 授权 */}
      {server.oauth_enabled && (
        <div className="mb-5 p-3 rounded-lg" style={{ background: "var(--color-bg-elevated)", border: "1px solid var(--color-border)" }}>
          <div className="flex items-center justify-between">
            <div className="flex items-center gap-2">
              <span className="text-[10px] text-th-text-m uppercase tracking-wider">OAuth 2.1</span>
              <span className={`text-xs font-medium ${oauthBadge().cls}`}>{oauthBadge().text}</span>
              {oauthStatus?.expiresAt && (
                <span className="text-[10px] text-th-text-m font-mono">过期 {new Date(oauthStatus.expiresAt * 1000).toLocaleString()}</span>
              )}
            </div>
            <button onClick={handleAuthorize} disabled={authorizing} className="btn-primary text-xs">
              {authorizing ? "授权中…" : (oauthStatus?.authorized ? "重新授权" : "授权")}
            </button>
          </div>
          {oauthMsg && <div className="text-xs text-th-text-s mt-2">{oauthMsg}</div>}
        </div>
      )}

      {/* 配置信息 */}
      <div className="grid grid-cols-2 gap-x-6 gap-y-3 text-sm mb-6">
        <InfoField label="ID" value={server.id} mono />
        <InfoField label="状态" value={server.enabled ? "启用" : "禁用"} dot={server.enabled ? "bg-emerald-400" : "bg-th-text-m"} />
        <InfoField label="上游 Endpoint" value={server.endpoint} mono />
        <InfoField label="Auth Token" value={server.auth_token ? `${server.auth_token.slice(0, 8)}···` : "(透传客户端)"} />
      </div>

      {/* 工具列表（启用/禁用）*/}
      <div className="flex items-center justify-between mb-2">
        <SectionTitle className="!mb-0">工具列表</SectionTitle>
        <div className="flex items-center gap-2">
          {toolsMsg && <span className="text-[10px] text-th-text-m">{toolsMsg}</span>}
          <button onClick={fetchTools} disabled={fetching || oauthBlocked} className="btn-ghost text-xs"
            title={oauthBlocked ? "请先完成 OAuth 授权" : ""}>
            {fetching ? "拉取中…" : "↻ 刷新工具列表"}
          </button>
        </div>
      </div>
      {oauthBlocked && <p className="text-[10px] text-amber-400 mb-2">该 server 启用了 OAuth，请先在上方完成授权再拉取工具</p>}
      <div className="rounded-lg overflow-hidden" style={{ border: "1px solid var(--color-border)" }}>
        <table className="w-full text-sm">
          <thead style={{ background: "var(--color-bg-elevated)" }}>
            <tr className="border-b border-th-border text-[10px] uppercase tracking-wider text-th-text-m">
              <th className="px-3 py-2 text-left">工具</th>
              <th className="px-3 py-2 text-left">描述</th>
              <th className="px-3 py-2 text-center w-16">启用</th>
            </tr>
          </thead>
          <tbody>
            {(server.cached_tools || []).map((t) => {
              const on = !blockedSet.has(t.name);
              return (
                <tr key={t.name} className="border-b border-th-border last:border-0">
                  <td className="px-3 py-2 font-mono text-xs text-th-text align-top">{t.name}</td>
                  <td className="px-3 py-2 text-xs text-th-text-s">{t.description || "—"}</td>
                  <td className="px-3 py-2 text-center align-top">
                    <div className={`inline-block w-8 h-4 rounded-full relative cursor-pointer transition-colors ${on ? "" : "bg-th-text-m"}`}
                      style={on ? { background: "var(--color-accent)" } : {}}
                      onClick={() => toggleTool(t.name, !on)}>
                      <div className={`w-3.5 h-3.5 rounded-full bg-white absolute top-0.5 transition-all ${on ? "left-4" : "left-0.5"}`} />
                    </div>
                  </td>
                </tr>
              );
            })}
            {(server.cached_tools || []).length === 0 && (
              <tr><td colSpan={3} className="px-3 py-4 text-center text-th-text-m text-xs">尚未拉取，点击"刷新工具列表"</td></tr>
            )}
          </tbody>
        </table>
      </div>
      <p className="text-[10px] text-th-text-m mt-2">关闭的工具会从 tools/list 隐藏、tools/call 调用时拒绝（精确匹配）</p>

      {/* 自定义 Headers */}
      <SectionTitle className="mt-6">自定义 Headers</SectionTitle>
      <DataTable headers={["Header 名称", "值"]}
        rows={Object.entries(server.custom_headers || {}).map(([k, v]) => [k, v])}
        empty="无自定义 Header" mono
      />
    </div>
  );
}

// ========== 编辑表单 ==========
function McpForm({ data, onChange, onSave, onCancel, saving, newTool, onNewToolChange, newHeaderKey, newHeaderVal, onNewHeaderKeyChange, onNewHeaderValChange, isNew }: {
  data: McpServerEditData; onChange: (d: McpServerEditData) => void; onSave: () => void; onCancel: () => void; saving: boolean;
  newTool: string; onNewToolChange: (v: string) => void;
  newHeaderKey: string; newHeaderVal: string; onNewHeaderKeyChange: (v: string) => void; onNewHeaderValChange: (v: string) => void;
  isNew: boolean;
}) {
  const set = (field: keyof McpServerEditData, value: unknown) => onChange({ ...data, [field]: value });

  const addTool = () => {
    const t = newTool.trim();
    if (!t || data.blocked_tools.includes(t)) { onNewToolChange(""); return; }
    onChange({ ...data, blocked_tools: [...data.blocked_tools, t] });
    onNewToolChange("");
  };
  const removeTool = (t: string) => onChange({ ...data, blocked_tools: data.blocked_tools.filter((x) => x !== t) });

  const addHeader = () => {
    if (!newHeaderKey.trim() || !newHeaderVal.trim()) return;
    onChange({ ...data, custom_headers: { ...(data.custom_headers || {}), [newHeaderKey.trim()]: newHeaderVal.trim() } });
    onNewHeaderKeyChange(""); onNewHeaderValChange("");
  };

  return (
    <div>
      <h3 className="font-display font-semibold text-lg mb-4 text-th-text">{isNew ? "新增 MCP server" : `编辑: ${data.name}`}</h3>

      <div className="grid grid-cols-2 gap-4 mb-6">
        <FormField label="Server ID" required><input className="input" value={data.id} onChange={(e) => set("id", e.target.value)} placeholder="my-mcp" disabled={!isNew} /></FormField>
        <FormField label="显示名称" required><input className="input" value={data.name} onChange={(e) => set("name", e.target.value)} placeholder="我的 MCP Server" /></FormField>
        <FormField label="上游 Endpoint URL" required><input className="input" value={data.endpoint} onChange={(e) => set("endpoint", e.target.value)} placeholder="https://example.com/mcp" /></FormField>
        <FormField label="Auth Token（留空透传客户端）"><input className="input" type="password" value={data.auth_token ?? ""} onChange={(e) => set("auth_token", e.target.value || null)} placeholder="注入 Authorization: Bearer" /></FormField>
        <FormField label="启用">
          <label className="flex items-center gap-2 mt-1 cursor-pointer">
            <div className={`w-8 h-4 rounded-full relative transition-colors ${data.enabled ? "" : "bg-th-text-m"}`} style={data.enabled ? { background: "var(--color-accent)" } : {}} onClick={() => set("enabled", !data.enabled)}>
              <div className={`w-3.5 h-3.5 rounded-full bg-white absolute top-0.5 transition-all ${data.enabled ? "left-4" : "left-0.5"}`} />
            </div>
            <span className="text-sm text-th-text-s">{data.enabled ? "已启用" : "已禁用"}</span>
          </label>
        </FormField>
        <FormField label="OAuth 2.1">
          <label className="flex items-center gap-2 mt-1 cursor-pointer">
            <div className={`w-8 h-4 rounded-full relative transition-colors ${data.oauth_enabled ? "" : "bg-th-text-m"}`} style={data.oauth_enabled ? { background: "var(--color-accent)" } : {}} onClick={() => set("oauth_enabled", !data.oauth_enabled)}>
              <div className={`w-3.5 h-3.5 rounded-full bg-white absolute top-0.5 transition-all ${data.oauth_enabled ? "left-4" : "left-0.5"}`} />
            </div>
            <span className="text-sm text-th-text-s">{data.oauth_enabled ? "走 OAuth（忽略 Auth Token）" : "关闭"}</span>
          </label>
          <p className="text-[10px] text-th-text-m mt-1">开启后保存，再到查看页点"授权"完成 OAuth</p>
        </FormField>
      </div>

      {/* 工具黑名单 */}
      <SectionTitle>工具黑名单</SectionTitle>
      <div className="rounded-lg overflow-hidden mb-1" style={{ border: "1px solid var(--color-border)" }}>
        <table className="w-full text-sm">
          <thead style={{ background: "var(--color-bg-elevated)" }}>
            <tr className="border-b border-th-border text-[10px] uppercase tracking-wider text-th-text-m">
              <th className="px-3 py-2 text-left">被屏蔽的工具 name</th><th className="px-3 py-2 w-14"></th>
            </tr>
          </thead>
          <tbody>
            {data.blocked_tools.map((t) => (
              <tr key={t} className="border-b border-th-border">
                <td className="px-3 py-2 font-mono text-xs text-th-text">{t}</td>
                <td className="px-3 py-2 text-center"><button onClick={() => removeTool(t)} className="text-red-400 hover:text-red-300 text-xs">删除</button></td>
              </tr>
            ))}
            <tr style={{ background: "var(--color-bg-hover)" }}>
              <td className="px-3 py-2"><input className="input text-xs !py-1" value={newTool} onChange={(e) => onNewToolChange(e.target.value)} placeholder="工具 name（精确匹配）" onKeyDown={(e) => e.key === "Enter" && addTool()} /></td>
              <td className="px-3 py-2 text-center"><button onClick={addTool} className="text-th-accent text-xs font-medium hover:text-th-accent-d">添加</button></td>
            </tr>
          </tbody>
        </table>
      </div>
      <p className="text-[10px] text-th-text-m mb-6">tools/list 中删除这些工具，tools/call 调用时拒绝</p>

      {/* 自定义 Headers */}
      <SectionTitle>自定义 Headers</SectionTitle>
      <div className="rounded-lg overflow-hidden mb-1" style={{ border: "1px solid var(--color-border)" }}>
        <table className="w-full text-sm">
          <thead style={{ background: "var(--color-bg-elevated)" }}>
            <tr className="border-b border-th-border text-[10px] uppercase tracking-wider text-th-text-m">
              <th className="px-3 py-2 text-left">Header 名称</th><th className="px-3 py-2 text-left">值</th><th className="px-3 py-2 w-14"></th>
            </tr>
          </thead>
          <tbody>
            {Object.entries(data.custom_headers || {}).map(([key, val]) => (
              <tr key={key} className="border-b border-th-border">
                <td className="px-3 py-2 font-mono text-xs text-th-text">{key}</td>
                <td className="px-3 py-2 font-mono text-xs text-th-text">{val}</td>
                <td className="px-3 py-2 text-center"><button onClick={() => { const h = { ...data.custom_headers }; delete h[key]; onChange({ ...data, custom_headers: h }); }} className="text-red-400 hover:text-red-300 text-xs">删除</button></td>
              </tr>
            ))}
            <tr style={{ background: "var(--color-bg-hover)" }}>
              <td className="px-3 py-2"><input className="input text-xs !py-1" value={newHeaderKey} onChange={(e) => onNewHeaderKeyChange(e.target.value)} placeholder="Header 名称" onKeyDown={(e) => e.key === "Enter" && addHeader()} /></td>
              <td className="px-3 py-2"><input className="input text-xs !py-1" value={newHeaderVal} onChange={(e) => onNewHeaderValChange(e.target.value)} placeholder="Header 值" onKeyDown={(e) => e.key === "Enter" && addHeader()} /></td>
              <td className="px-3 py-2 text-center"><button onClick={addHeader} className="text-th-accent text-xs font-medium hover:text-th-accent-d">添加</button></td>
            </tr>
          </tbody>
        </table>
      </div>
      <p className="text-[10px] text-th-text-m mb-6">转发到上游时叠加，可覆盖默认头</p>

      <div className="flex gap-3">
        <button onClick={onSave} disabled={saving} className="btn-primary">{saving ? "保存中..." : "保存"}</button>
        <button onClick={onCancel} className="btn-ghost">取消</button>
      </div>
    </div>
  );
}

// ========== 通用组件 ==========
function InfoField({ label, value, mono, dot }: { label: string; value: string; mono?: boolean; dot?: string }) {
  return (
    <div className="py-1.5 border-b border-th-border">
      <div className="text-[10px] text-th-text-m uppercase tracking-wider mb-0.5">{label}</div>
      <div className={`text-sm text-th-text flex items-center gap-2 ${mono ? "font-mono" : ""}`}>
        {dot && <span className={`w-2 h-2 rounded-full ${dot}`} />}
        <span className="truncate">{value}</span>
      </div>
    </div>
  );
}

function FormField({ label, required, children }: { label: string; required?: boolean; children: React.ReactNode }) {
  return (
    <div>
      <label className="block text-[10px] text-th-text-m mb-1 uppercase tracking-wider">
        {label} {required && <span className="text-red-400">*</span>}
      </label>
      {children}
    </div>
  );
}

function SectionTitle({ children, className }: { children: React.ReactNode; className?: string }) {
  return <h4 className={`font-display font-medium text-sm text-th-text mb-2 ${className ?? ""}`}>{children}</h4>;
}

function DataTable({ headers, rows, empty, mono }: { headers: string[]; rows: string[][]; empty: string; mono?: boolean }) {
  return (
    <div className="rounded-lg overflow-hidden" style={{ border: "1px solid var(--color-border)" }}>
      <table className="w-full text-xs">
        <thead style={{ background: "var(--color-bg-elevated)" }}>
          <tr className="border-b border-th-border">
            {headers.map((h, i) => <th key={i} className="px-3 py-2 text-[10px] uppercase tracking-wider text-th-text-m text-left">{h}</th>)}
          </tr>
        </thead>
        <tbody>
          {rows.map((row, i) => (
            <tr key={i} className="border-b border-th-border last:border-0">
              {row.map((cell, j) => <td key={j} className={`px-3 py-2 ${mono ? "font-mono" : ""} text-th-text`}>{cell}</td>)}
            </tr>
          ))}
          {rows.length === 0 && <tr><td colSpan={headers.length} className="px-3 py-4 text-center text-th-text-m">{empty}</td></tr>}
        </tbody>
      </table>
    </div>
  );
}
