import { useEffect, useState } from "react";
import { invoke } from "../lib/tauri";

interface ChannelEditData {
  id: string;
  name: string;
  provider: string;
  url: string;
  api_key: string;
  priority: number;
  enabled: boolean;
  fallback_model: string;
  model_mapping: Record<string, string>;
  timeout_ms: number;
  custom_headers: Record<string, string>;
  strip_thinking: boolean;
  retry_count: number;
  retry_delay_ms: number;
  force_effort: string | null;
  auto_cache: boolean;
}

interface FullConfig {
  listen_port: number;
  channels: ChannelEditData[];
}

const PROVIDERS = [
  { value: "openai", label: "OpenAI (Chat)", url_placeholder: "https://api.openai.com/v1/chat/completions" },
  { value: "openai_responses", label: "OpenAI (Responses)", url_placeholder: "https://api.openai.com/v1/responses" },
  { value: "anthropic", label: "Anthropic (Messages)", url_placeholder: "https://api.anthropic.com/v1/messages" },
];

function emptyChannel(existingCount = 0): ChannelEditData {
  return { id: "", name: "", provider: "openai", url: "https://api.openai.com/v1/chat/completions", api_key: "", priority: existingCount + 1, enabled: true, fallback_model: "", model_mapping: {}, timeout_ms: 60000, custom_headers: {}, strip_thinking: false, retry_count: 0, retry_delay_ms: 500, force_effort: null, auto_cache: true };
}

const providerLabel = (p: string) => PROVIDERS.find((x) => x.value === p)?.label ?? p;

export default function ChannelConfig() {
  const [config, setConfig] = useState<FullConfig | null>(null);
  const [viewing, setViewing] = useState<ChannelEditData | null>(null); // 详情弹框
  const [editData, setEditData] = useState<ChannelEditData | null>(null); // 编辑弹框数据
  const [isNew, setIsNew] = useState(false);
  const [newMappingAlias, setNewMappingAlias] = useState("");
  const [newMappingModel, setNewMappingModel] = useState("");
  const [newHeaderKey, setNewHeaderKey] = useState("");
  const [newHeaderVal, setNewHeaderVal] = useState("");
  const [saving, setSaving] = useState(false);
  const [message, setMessage] = useState("");

  useEffect(() => { loadConfig(); }, []);

  const loadConfig = async () => {
    try {
      const data = await invoke<FullConfig>("get_full_config");
      setConfig(data);
    } catch (e) { console.error(e); }
  };

  const sorted = config ? config.channels.slice().sort((a, b) => a.priority - b.priority) : [];

  const resetPendingRows = () => { setNewMappingAlias(""); setNewMappingModel(""); setNewHeaderKey(""); setNewHeaderVal(""); };
  const showMessage = (msg: string) => { setMessage(msg); setTimeout(() => setMessage(""), 3000); };

  const handleView = (ch: ChannelEditData) => setViewing(ch);

  const handleEdit = (ch: ChannelEditData) => {
    setEditData({ ...ch, model_mapping: { ...ch.model_mapping }, custom_headers: { ...(ch.custom_headers || {}) } });
    resetPendingRows();
    setIsNew(false);
  };

  const handleNew = () => {
    setEditData(emptyChannel(config?.channels.length ?? 0));
    resetPendingRows();
    setIsNew(true);
  };

  const handleDelete = async (ch: ChannelEditData) => {
    if (!confirm(`确定删除渠道 "${ch.name}" ?`)) return;
    try {
      await invoke("delete_channel", { channelId: ch.id });
      setViewing(null); setEditData(null);
      showMessage("删除成功");
      loadConfig();
    } catch (e) { showMessage(`删除失败: ${e}`); }
  };

  const handleSave = async () => {
    if (!editData) return;
    if (!editData.id.trim() || !editData.name.trim() || !editData.fallback_model.trim()) { showMessage("ID、名称、兜底模型不能为空"); return; }
    // 合并"填了但未点添加"的暂存行，避免用户漏点添加导致数据丢失
    const merged: ChannelEditData = {
      ...editData,
      model_mapping: { ...editData.model_mapping },
      custom_headers: { ...(editData.custom_headers || {}) },
    };
    if (newMappingAlias.trim() && newMappingModel.trim()) merged.model_mapping[newMappingAlias.trim()] = newMappingModel.trim();
    if (newHeaderKey.trim() && newHeaderVal.trim()) merged.custom_headers[newHeaderKey.trim()] = newHeaderVal.trim();
    setSaving(true);
    try {
      await invoke("save_channel", { channel: merged });
      resetPendingRows();
      setEditData(null);
      showMessage("保存成功");
      loadConfig();
    } catch (e) { showMessage(`保存失败: ${e}`); }
    finally { setSaving(false); }
  };

  const addMapping = () => {
    if (!editData || !newMappingAlias.trim() || !newMappingModel.trim()) return;
    setEditData({ ...editData, model_mapping: { ...editData.model_mapping, [newMappingAlias.trim()]: newMappingModel.trim() } });
    setNewMappingAlias(""); setNewMappingModel("");
  };

  const removeMapping = (alias: string) => {
    if (!editData) return;
    const m = { ...editData.model_mapping }; delete m[alias];
    setEditData({ ...editData, model_mapping: m });
  };

  // ========== 拖拽排序 ==========
  const [draggedIdx, setDraggedIdx] = useState<number | null>(null);
  const [dragOverIdx, setDragOverIdx] = useState<number | null>(null);

  const handleDragStart = (idx: number) => setDraggedIdx(idx);
  const handleDragOver = (e: React.DragEvent, idx: number) => {
    e.preventDefault();
    if (draggedIdx === null || draggedIdx === idx) { setDragOverIdx(null); return; }
    setDragOverIdx(idx);
  };
  const handleDragLeave = () => setDragOverIdx(null);
  const handleDragEnd = () => { setDraggedIdx(null); setDragOverIdx(null); };

  const handleDrop = async (e: React.DragEvent, dropIdx: number) => {
    e.preventDefault();
    const dragIdx = draggedIdx;
    setDraggedIdx(null); setDragOverIdx(null);
    if (!config || dragIdx === null || dragIdx === dropIdx) return;
    const sortedArr = config.channels.slice().sort((a, b) => a.priority - b.priority);
    const [moved] = sortedArr.splice(dragIdx, 1);
    sortedArr.splice(dropIdx, 0, moved);
    const reordered = sortedArr.map((ch, i) => ({ ...ch, priority: i + 1 }));
    setConfig({ ...config, channels: reordered });
    try {
      await Promise.all(reordered.map((ch) => invoke("save_channel", { channel: ch })));
      showMessage("优先级已更新");
      await loadConfig();
    } catch (err) { showMessage(`排序保存失败: ${err}`); await loadConfig(); }
  };

  return (
    <div>
      <div className="flex items-end justify-between mb-5">
        <div>
          <div className="section-label">Configuration</div>
          <h2 className="mt-1 font-display text-2xl font-semibold tracking-tight">Channel configuration</h2>
          <p className="mt-1 text-sm text-th-text-m">管理上游提供商、模型路由和故障转移参数。拖拽调整优先级。</p>
        </div>
        <div className="flex items-center gap-3">
          {message && <span className="text-xs px-3 py-1.5 rounded-lg bg-emerald-500/10 text-emerald-400 border border-emerald-500/20">{message}</span>}
          <button onClick={handleNew} className="btn-primary text-xs">+ 新增渠道</button>
        </div>
      </div>

      <div className="page-surface overflow-hidden">
        <div className="overflow-auto">
          <table className="w-full text-xs">
            <thead>
              <tr className="border-b border-th-border text-[10px] uppercase tracking-wider text-th-text-m" style={{ background: "var(--color-bg-elevated)" }}>
                <th className="px-2.5 py-1.5 text-left w-6"></th>
                <th className="px-2.5 py-1.5 text-left w-6"></th>
                <th className="px-2.5 py-1.5 text-left">渠道</th>
                <th className="px-2.5 py-1.5 text-left">提供商</th>
                <th className="px-2.5 py-1.5 text-left">兜底模型</th>
                <th className="px-2.5 py-1.5 text-left">Endpoint URL</th>
                <th className="px-2.5 py-1.5 text-center w-14">优先级</th>
                <th className="px-2.5 py-1.5 text-right w-40">操作</th>
              </tr>
            </thead>
            <tbody>
              {sorted.map((ch, idx) => (
                <tr
                  key={ch.id}
                  draggable
                  onDragStart={() => handleDragStart(idx)}
                  onDragOver={(e) => handleDragOver(e, idx)}
                  onDragLeave={handleDragLeave}
                  onDrop={(e) => handleDrop(e, idx)}
                  onDragEnd={handleDragEnd}
                  className={`border-b border-th-border last:border-0 transition-colors ${
                    draggedIdx === idx ? "opacity-40" : ""
                  } ${dragOverIdx === idx && draggedIdx !== idx ? "bg-th-accent-g" : ""}`}
                >
                  <td className="px-2.5 py-1.5 text-th-text-m">
                    <span className="cursor-grab active:cursor-grabbing select-none leading-none" title="拖拽调整优先级">⠿</span>
                  </td>
                  <td className="px-2.5 py-1.5">
                    <span className={`inline-block w-1.5 h-1.5 rounded-full ${ch.enabled ? "bg-emerald-400" : "bg-th-text-m"}`} title={ch.enabled ? "已启用" : "已禁用"} />
                  </td>
                  <td className="px-2.5 py-1.5">
                    <div className="flex items-center gap-1.5 max-w-[220px]">
                      <span className="font-medium text-th-text truncate">{ch.name}</span>
                      <span className="text-[10px] text-th-text-m font-mono truncate shrink-0">({ch.id})</span>
                    </div>
                  </td>
                  <td className="px-2.5 py-1.5 text-th-text-s">{providerLabel(ch.provider)}</td>
                  <td className="px-2.5 py-1.5 font-mono text-th-text">{ch.fallback_model || "—"}</td>
                  <td className="px-2.5 py-1.5 font-mono text-th-text-s max-w-[280px] truncate" title={ch.url}>{ch.url}</td>
                  <td className="px-2.5 py-1.5 text-center font-mono text-th-text-m">{ch.priority}</td>
                  <td className="px-2.5 py-1.5">
                    <div className="flex gap-1 justify-end">
                      <button onClick={() => handleView(ch)} className="btn-ghost text-[11px] !px-2 !py-0.5">详情</button>
                      <button onClick={() => handleEdit(ch)} className="btn-primary text-[11px] !px-2 !py-0.5">编辑</button>
                      <button onClick={() => handleDelete(ch)} className="btn-danger text-[11px] !px-2 !py-0.5">删除</button>
                    </div>
                  </td>
                </tr>
              ))}
              {sorted.length === 0 && (
                <tr><td colSpan={8} className="px-3 py-12 text-center text-th-text-m">暂无渠道，点击右上角"新增渠道"</td></tr>
              )}
            </tbody>
          </table>
        </div>
      </div>

      {/* 详情弹框 */}
      {viewing && (
        <Modal title={viewing.name} onClose={() => setViewing(null)} maxWidth="max-w-3xl">
          <ChannelView channel={viewing} />
        </Modal>
      )}

      {/* 编辑弹框 */}
      {editData && (
        <Modal title={isNew ? "新增渠道" : `编辑: ${editData.name}`} onClose={() => setEditData(null)} maxWidth="max-w-3xl">
          <ChannelForm data={editData} onChange={setEditData} onSave={handleSave} onCancel={() => setEditData(null)} saving={saving}
            newMappingAlias={newMappingAlias} newMappingModel={newMappingModel} onNewAliasChange={setNewMappingAlias} onNewModelChange={setNewMappingModel}
            onAddMapping={addMapping} onRemoveMapping={removeMapping}
            newHeaderKey={newHeaderKey} newHeaderVal={newHeaderVal} onNewHeaderKeyChange={setNewHeaderKey} onNewHeaderValChange={setNewHeaderVal}
            isNew={isNew} />
        </Modal>
      )}
    </div>
  );
}

// ========== 通用弹框 ==========
function Modal({ title, onClose, children, maxWidth = "max-w-2xl" }: { title: string; onClose: () => void; children: React.ReactNode; maxWidth?: string }) {
  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/40 backdrop-blur-sm" onClick={onClose}>
      <div className={`bg-th-base border border-th-border rounded-2xl shadow-2xl shadow-black/20 w-[92vw] ${maxWidth} max-h-[85vh] flex flex-col animate-slide-up overflow-hidden`} onClick={(ev) => ev.stopPropagation()}>
        <div className="flex items-center justify-between px-5 py-3 border-b border-th-border shrink-0">
          <span className="font-display font-medium text-th-text">{title}</span>
          <button onClick={onClose} className="text-th-text-m hover:text-th-text transition-colors text-lg px-2">✕</button>
        </div>
        <div className="flex-1 overflow-auto p-5">{children}</div>
      </div>
    </div>
  );
}

// ========== 查看模式（详情弹框内容） ==========
function ChannelView({ channel }: { channel: ChannelEditData }) {
  const [testing, setTesting] = useState(false);
  const [testResult, setTestResult] = useState<Record<string, unknown> | null>(null);

  // 切换渠道时清空上一个渠道的测试结果（组件实例被复用，state 不会自动重置）
  useEffect(() => { setTestResult(null); setTesting(false); }, [channel.id]);

  const handleTest = async () => {
    setTesting(true); setTestResult(null);
    try { setTestResult(await invoke<Record<string, unknown>>("test_channel", { channelId: channel.id })); }
    catch (e) { setTestResult({ success: false, error: String(e) }); }
    finally { setTesting(false); }
  };

  return (
    <div>
      {/* 测试连接 */}
      <div className="flex items-center justify-end mb-4">
        <button onClick={handleTest} disabled={testing} className="btn-ghost text-xs">{testing ? "测试中..." : "⚡ 测试连接"}</button>
      </div>

      {/* 测试结果 */}
      {testResult && (
        <div className={`mb-4 rounded-lg p-4 ${testResult.success ? "bg-emerald-500/10 border border-emerald-500/20" : "bg-red-500/10 border border-red-500/20"}`}>
          <div className="flex items-center gap-2 mb-2">
            <span className={`w-2.5 h-2.5 rounded-full ${testResult.success ? "bg-emerald-400" : "bg-red-400"}`} />
            <span className={`font-medium text-sm ${testResult.success ? "text-emerald-400" : "text-red-400"}`}>{testResult.success ? "连接成功" : "连接失败"}</span>
            {testResult.status_code != null && <span className="text-xs text-th-text-m font-mono">HTTP {String(testResult.status_code)}</span>}
            {testResult.latency_ms != null && <span className="text-xs text-th-text-m font-mono">{String(testResult.latency_ms)}ms</span>}
          </div>
          {testResult.error != null && <div className="text-xs text-red-400 mb-2">{String(testResult.error)}</div>}
          {testResult.response_body != null && (
            <details className="text-xs">
              <summary className="cursor-pointer text-th-text-m hover:text-th-text">查看响应</summary>
              <pre className="mt-2 p-2 rounded-lg text-th-text-s font-mono overflow-auto whitespace-pre-wrap" style={{ background: "var(--color-bg-elevated)" }}>{String(testResult.response_body)}</pre>
            </details>
          )}
        </div>
      )}

      {/* 配置信息 */}
      <div className="grid grid-cols-2 gap-x-6 gap-y-3 text-sm mb-6">
        <InfoField label="ID" value={channel.id} mono />
        <InfoField label="提供商" value={providerLabel(channel.provider)} />
        <InfoField label="API URL" value={channel.url} mono />
        <InfoField label="状态" value={channel.enabled ? "启用" : "禁用"} dot={channel.enabled ? "bg-emerald-400" : "bg-th-text-m"} />
        <InfoField label="兜底模型" value={channel.fallback_model} mono />
        <InfoField label="超时" value={`${channel.timeout_ms}ms`} />
        <InfoField label="单渠道重试" value={channel.retry_count ? `${channel.retry_count}次 / 间隔${channel.retry_delay_ms}ms` : "关闭"} />
        <InfoField label="抹除 Thinking" value={channel.strip_thinking ? "开启" : "关闭"} />
        <InfoField label="自动缓存断点" value={channel.auto_cache ? "开启" : "关闭"} />
        <InfoField label="强制 Effort" value={channel.force_effort ? channel.force_effort : "不覆盖"} mono />
        <InfoField label="API Key" value={channel.api_key ? `${channel.api_key.slice(0, 8)}···` : "(未设置)"} />
      </div>

      {/* 模型映射 */}
      <SectionTitle>模型映射</SectionTitle>
      <DataTable headers={["调用方模型名", "→", "实际模型"]}
        rows={Object.entries(channel.model_mapping).map(([a, m]) => [a, "→", m])}
        empty="无映射规则" mono
      />
      <p className="text-[10px] text-th-text-m mt-2">未命中映射规则的请求将使用兜底模型: <span className="font-mono text-th-text-s">{channel.fallback_model}</span></p>

      {/* 自定义 Headers */}
      <SectionTitle className="mt-6">自定义 Headers</SectionTitle>
      <DataTable headers={["Header 名称", "值"]}
        rows={Object.entries(channel.custom_headers || {}).map(([k, v]) => [k, v])}
        empty="无自定义 Header" mono
      />
      <p className="text-[10px] text-th-text-m mt-1">自定义 header 会覆盖默认生成的同名 header</p>
    </div>
  );
}

// ========== 编辑表单（编辑弹框内容） ==========
function ChannelForm({ data, onChange, onSave, onCancel, saving, newMappingAlias, newMappingModel, onNewAliasChange, onNewModelChange, onAddMapping, onRemoveMapping, newHeaderKey, newHeaderVal, onNewHeaderKeyChange, onNewHeaderValChange, isNew }: {
  data: ChannelEditData; onChange: (d: ChannelEditData) => void; onSave: () => void; onCancel: () => void; saving: boolean;
  newMappingAlias: string; newMappingModel: string; onNewAliasChange: (v: string) => void; onNewModelChange: (v: string) => void;
  onAddMapping: () => void; onRemoveMapping: (alias: string) => void;
  newHeaderKey: string; newHeaderVal: string; onNewHeaderKeyChange: (v: string) => void; onNewHeaderValChange: (v: string) => void;
  isNew: boolean;
}) {
  const set = (field: keyof ChannelEditData, value: unknown) => onChange({ ...data, [field]: value });
  const addHeader = () => {
    if (!newHeaderKey.trim() || !newHeaderVal.trim()) return;
    onChange({ ...data, custom_headers: { ...(data.custom_headers || {}), [newHeaderKey.trim()]: newHeaderVal.trim() } });
    onNewHeaderKeyChange(""); onNewHeaderValChange("");
  };

  return (
    <div>
      <div className="grid grid-cols-2 gap-4 mb-6">
        <FormField label="渠道 ID" required><input className="input" value={data.id} onChange={(e) => set("id", e.target.value)} placeholder="openai-primary" disabled={!isNew} /></FormField>
        <FormField label="渠道名称" required><input className="input" value={data.name} onChange={(e) => set("name", e.target.value)} placeholder="OpenAI 主账号" /></FormField>
        <FormField label="提供商" required>
          <select className="input" value={data.provider} onChange={(e) => {
            const newProvider = e.target.value;
            const newUrl = PROVIDERS.find(p => p.value === newProvider)?.url_placeholder ?? data.url;
            onChange({ ...data, provider: newProvider, url: newUrl });
          }}>
            {PROVIDERS.map((p) => <option key={p.value} value={p.value}>{p.label}</option>)}
          </select>
        </FormField>
        <FormField label="API URL" required><input className="input" value={data.url} onChange={(e) => set("url", e.target.value)} placeholder={PROVIDERS.find(p => p.value === data.provider)?.url_placeholder ?? "https://api.openai.com/v1/chat/completions"} /></FormField>
        <FormField label="API Key"><input className="input" type="password" value={data.api_key} onChange={(e) => set("api_key", e.target.value)} placeholder="sk-..." /></FormField>
        <FormField label="兜底模型" required><input className="input" value={data.fallback_model} onChange={(e) => set("fallback_model", e.target.value)} placeholder="找不到映射时使用的模型" /></FormField>
        <FormField label="超时 (ms)"><input className="input" type="number" value={data.timeout_ms} onChange={(e) => set("timeout_ms", Number(e.target.value))} min={1000} step={1000} /></FormField>
        <FormField label="单渠道重试次数">
          <input className="input" type="number" value={data.retry_count} onChange={(e) => set("retry_count", Number(e.target.value))} min={0} max={10} />
          <p className="text-[10px] text-th-text-m mt-1">0 = 直接切换下一个渠道</p>
        </FormField>
        <FormField label="重试间隔 (ms)">
          <input className="input" type="number" value={data.retry_delay_ms} onChange={(e) => set("retry_delay_ms", Number(e.target.value))} min={0} step={100} />
        </FormField>
        <FormField label="启用">
          <label className="flex items-center gap-2 mt-1 cursor-pointer">
            <div className={`w-8 h-4 rounded-full relative transition-colors ${data.enabled ? "" : "bg-th-text-m"}`} style={data.enabled ? { background: "var(--color-accent)" } : {}} onClick={() => set("enabled", !data.enabled)}>
              <div className={`w-3.5 h-3.5 rounded-full bg-white absolute top-0.5 transition-all ${data.enabled ? "left-4" : "left-0.5"}`} />
            </div>
            <span className="text-sm text-th-text-s">{data.enabled ? "已启用" : "已禁用"}</span>
          </label>
        </FormField>
        <FormField label="抹除 Thinking">
          <label className="flex items-center gap-2 mt-1 cursor-pointer">
            <div className={`w-8 h-4 rounded-full relative transition-colors ${data.strip_thinking ? "" : "bg-th-text-m"}`} style={data.strip_thinking ? { background: "var(--color-accent)" } : {}} onClick={() => set("strip_thinking", !data.strip_thinking)}>
              <div className={`w-3.5 h-3.5 rounded-full bg-white absolute top-0.5 transition-all ${data.strip_thinking ? "left-4" : "left-0.5"}`} />
            </div>
            <span className="text-sm text-th-text-s">{data.strip_thinking ? "移除 thinking 块" : "保留"}</span>
          </label>
        </FormField>
        <FormField label="自动缓存断点">
          <label className="flex items-center gap-2 mt-1 cursor-pointer">
            <div className={`w-8 h-4 rounded-full relative transition-colors ${data.auto_cache ? "" : "bg-th-text-m"}`} style={data.auto_cache ? { background: "var(--color-accent)" } : {}} onClick={() => set("auto_cache", !data.auto_cache)}>
              <div className={`w-3.5 h-3.5 rounded-full bg-white absolute top-0.5 transition-all ${data.auto_cache ? "left-4" : "left-0.5"}`} />
            </div>
            <span className="text-sm text-th-text-s">{data.auto_cache ? "转 Anthropic 时自动打断点" : "不注入"}</span>
          </label>
        </FormField>
        <FormField label="强制 Effort">
          <input className="input" value={data.force_effort ?? ""} onChange={(e) => set("force_effort", e.target.value || null)} placeholder="如 max（留空不覆盖）" />
          <p className="text-[10px] text-th-text-m mt-1">覆盖请求的 output_config.effort，无论原值</p>
        </FormField>
      </div>

      {/* 模型映射 */}
      <SectionTitle>模型映射</SectionTitle>
      <div className="rounded-lg overflow-hidden mb-1" style={{ border: "1px solid var(--color-border)" }}>
        <table className="w-full text-sm">
          <thead style={{ background: "var(--color-bg-elevated)" }}>
            <tr className="border-b border-th-border text-[10px] uppercase tracking-wider text-th-text-m">
              <th className="px-3 py-2 text-left">调用方模型名</th><th className="px-3 py-2 text-center w-10">→</th><th className="px-3 py-2 text-left">实际模型</th><th className="px-3 py-2 w-14"></th>
            </tr>
          </thead>
          <tbody>
            {Object.entries(data.model_mapping).map(([alias, actual]) => (
              <tr key={alias} className="border-b border-th-border">
                <td className="px-3 py-2 font-mono text-xs text-th-text">{alias}</td>
                <td className="px-3 py-2 text-center text-th-text-m">→</td>
                <td className="px-3 py-2 font-mono text-xs text-th-text">{actual}</td>
                <td className="px-3 py-2 text-center"><button onClick={() => onRemoveMapping(alias)} className="text-red-400 hover:text-red-300 text-xs">删除</button></td>
              </tr>
            ))}
            <tr style={{ background: "var(--color-bg-hover)" }}>
              <td className="px-3 py-2"><input className="input text-xs !py-1" value={newMappingAlias} onChange={(e) => onNewAliasChange(e.target.value)} placeholder="模型别名" onKeyDown={(e) => e.key === "Enter" && onAddMapping()} /></td>
              <td className="px-3 py-2 text-center text-th-text-m">→</td>
              <td className="px-3 py-2"><input className="input text-xs !py-1" value={newMappingModel} onChange={(e) => onNewModelChange(e.target.value)} placeholder="实际模型" onKeyDown={(e) => e.key === "Enter" && onAddMapping()} /></td>
              <td className="px-3 py-2 text-center"><button onClick={onAddMapping} className="text-th-accent text-xs font-medium hover:text-th-accent-d">添加</button></td>
            </tr>
          </tbody>
        </table>
      </div>
      <p className="text-[10px] text-th-text-m mb-6">兜底模型: {data.fallback_model || "(未设置)"}</p>

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
      <p className="text-[10px] text-th-text-m mb-6">自定义 header 会覆盖默认生成的同名 header</p>

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
            {headers.map((h, i) => <th key={i} className={`px-3 py-2 text-[10px] uppercase tracking-wider text-th-text-m ${h === "→" ? "text-center w-10" : "text-left"}`}>{h}</th>)}
          </tr>
        </thead>
        <tbody>
          {rows.map((row, i) => (
            <tr key={i} className="border-b border-th-border last:border-0">
              {row.map((cell, j) => <td key={j} className={`px-3 py-2 ${mono ? "font-mono" : ""} ${cell === "→" ? "text-center text-th-text-m" : "text-th-text"}`}>{cell}</td>)}
            </tr>
          ))}
          {rows.length === 0 && <tr><td colSpan={headers.length} className="px-3 py-4 text-center text-th-text-m">{empty}</td></tr>}
        </tbody>
      </table>
    </div>
  );
}
