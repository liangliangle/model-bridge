/**
 * 模型价格管理页面：对模型价格进行增删改查。
 * 成本计算按「实际模型」精确匹配价格表（美元 / 百万 token）。
 */
import { useEffect, useState } from "react";
import { invoke } from "../lib/tauri";

interface ModelPrice {
  model: string;
  input_per_mtok: number;
  output_per_mtok: number;
  cache_read_per_mtok: number | null;
  cache_write_per_mtok: number | null;
  enabled: boolean;
}

function emptyPrice(): ModelPrice {
  return {
    model: "",
    input_per_mtok: 0,
    output_per_mtok: 0,
    cache_read_per_mtok: null,
    cache_write_per_mtok: null,
    enabled: true,
  };
}

export default function ModelPriceConfig() {
  const [prices, setPrices] = useState<ModelPrice[]>([]);
  const [editData, setEditData] = useState<ModelPrice | null>(null);
  const [isNew, setIsNew] = useState(false);
  const [saving, setSaving] = useState(false);
  const [message, setMessage] = useState("");

  useEffect(() => { loadPrices(); }, []);

  const loadPrices = async () => {
    try {
      const data = await invoke<ModelPrice[]>("get_model_prices");
      setPrices(data);
    } catch (e) { console.error(e); setPrices([]); }
  };

  const showMessage = (msg: string) => { setMessage(msg); setTimeout(() => setMessage(""), 3000); };

  const handleNew = () => { setEditData(emptyPrice()); setIsNew(true); };
  const handleEdit = (p: ModelPrice) => { setEditData({ ...p }); setIsNew(false); };

  const handleDelete = async (p: ModelPrice) => {
    if (!confirm(`确定删除模型 "${p.model}" 的价格配置？`)) return;
    try {
      await invoke("delete_model_price", { model: p.model });
      setEditData(null);
      showMessage("删除成功");
      loadPrices();
    } catch (e) { showMessage(`删除失败: ${e}`); }
  };

  const handleSave = async () => {
    if (!editData) return;
    if (!editData.model.trim()) { showMessage("模型名不能为空"); return; }
    setSaving(true);
    try {
      await invoke("save_model_price", { price: editData });
      setEditData(null);
      showMessage("保存成功");
      loadPrices();
    } catch (e) { showMessage(`保存失败: ${e}`); }
    finally { setSaving(false); }
  };

  return (
    <div>
      <div className="flex items-end justify-between mb-5">
        <div>
          <div className="section-label">Configuration</div>
          <h2 className="mt-1 font-display text-2xl font-semibold tracking-tight">模型价格</h2>
          <p className="mt-1 text-sm text-th-text-m">按实际模型精确匹配定价，用于成本计算。单价单位：美元 / 百万 token。</p>
        </div>
        <div className="flex items-center gap-3">
          {message && <span className="text-xs px-3 py-1.5 rounded-lg bg-emerald-500/10 text-emerald-400 border border-emerald-500/20">{message}</span>}
          <button onClick={handleNew} className="btn-primary text-xs">+ 新增价格</button>
        </div>
      </div>

      <div className="page-surface overflow-hidden">
        <div className="overflow-auto">
          <table className="w-full text-xs">
            <thead>
              <tr className="border-b border-th-border text-[10px] uppercase tracking-wider text-th-text-m" style={{ background: "var(--color-bg-elevated)" }}>
                <th className="px-2.5 py-2 text-left">模型</th>
                <th className="px-2.5 py-2 text-right">输入 ($/M)</th>
                <th className="px-2.5 py-2 text-right">输出 ($/M)</th>
                <th className="px-2.5 py-2 text-right">缓存读 ($/M)</th>
                <th className="px-2.5 py-2 text-right">缓存写 ($/M)</th>
                <th className="px-2.5 py-2 text-center">启用</th>
                <th className="px-2.5 py-2 text-right w-40">操作</th>
              </tr>
            </thead>
            <tbody>
              {prices.map((p) => (
                <tr key={p.model} className="border-b border-th-border last:border-0">
                  <td className="px-2.5 py-2 font-mono text-th-text">{p.model}</td>
                  <td className="px-2.5 py-2 text-right font-mono text-th-text-s">{p.input_per_mtok}</td>
                  <td className="px-2.5 py-2 text-right font-mono text-th-text-s">{p.output_per_mtok}</td>
                  <td className="px-2.5 py-2 text-right font-mono text-th-text-s">{p.cache_read_per_mtok ?? "—"}</td>
                  <td className="px-2.5 py-2 text-right font-mono text-th-text-s">{p.cache_write_per_mtok ?? "—"}</td>
                  <td className="px-2.5 py-2 text-center">
                    <span className={`inline-block w-1.5 h-1.5 rounded-full ${p.enabled ? "bg-emerald-400" : "bg-th-text-m"}`} title={p.enabled ? "已启用" : "已禁用"} />
                  </td>
                  <td className="px-2.5 py-2">
                    <div className="flex gap-1 justify-end">
                      <button onClick={() => handleEdit(p)} className="btn-primary text-[11px] !px-2 !py-0.5">编辑</button>
                      <button onClick={() => handleDelete(p)} className="btn-danger text-[11px] !px-2 !py-0.5">删除</button>
                    </div>
                  </td>
                </tr>
              ))}
              {prices.length === 0 && (
                <tr><td colSpan={7} className="px-3 py-12 text-center text-th-text-m">暂无价格配置，点击右上角"新增价格"</td></tr>
              )}
            </tbody>
          </table>
        </div>
      </div>

      {editData && (
        <Modal title={isNew ? "新增价格" : `编辑: ${editData.model}`} onClose={() => setEditData(null)}>
          <PriceForm data={editData} onChange={setEditData} onSave={handleSave} onCancel={() => setEditData(null)} saving={saving} isNew={isNew} />
        </Modal>
      )}
    </div>
  );
}

function Modal({ title, onClose, children }: { title: string; onClose: () => void; children: React.ReactNode }) {
  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/40 backdrop-blur-sm" onClick={onClose}>
      <div className="bg-th-base border border-th-border rounded-2xl shadow-2xl shadow-black/20 w-[92vw] max-w-2xl max-h-[85vh] flex flex-col animate-slide-up overflow-hidden" onClick={(ev) => ev.stopPropagation()}>
        <div className="flex items-center justify-between px-5 py-3 border-b border-th-border shrink-0">
          <span className="font-display font-medium text-th-text">{title}</span>
          <button onClick={onClose} className="text-th-text-m hover:text-th-text transition-colors text-lg px-2">✕</button>
        </div>
        <div className="flex-1 overflow-auto p-5">{children}</div>
      </div>
    </div>
  );
}

function PriceForm({ data, onChange, onSave, onCancel, saving, isNew }: {
  data: ModelPrice; onChange: (d: ModelPrice) => void; onSave: () => void; onCancel: () => void; saving: boolean; isNew: boolean;
}) {
  const set = (field: keyof ModelPrice, value: unknown) => onChange({ ...data, [field]: value });
  return (
    <div>
      <div className="grid grid-cols-2 gap-4 mb-6">
        <FormField label="模型名" required>
          <input className="input" value={data.model} onChange={(e) => set("model", e.target.value)} placeholder="如 gpt-4o" disabled={!isNew} />
        </FormField>
        <FormField label="输入单价 ($/M)">
          <input className="input" type="number" step="0.0001" min={0} value={data.input_per_mtok} onChange={(e) => set("input_per_mtok", Number(e.target.value))} />
        </FormField>
        <FormField label="输出单价 ($/M)">
          <input className="input" type="number" step="0.0001" min={0} value={data.output_per_mtok} onChange={(e) => set("output_per_mtok", Number(e.target.value))} />
        </FormField>
        <FormField label="缓存读单价 ($/M)">
          <input className="input" type="number" step="0.0001" min={0} value={data.cache_read_per_mtok ?? ""} onChange={(e) => set("cache_read_per_mtok", e.target.value === "" ? null : Number(e.target.value))} placeholder="留空回退到输入价" />
        </FormField>
        <FormField label="缓存写单价 ($/M)">
          <input className="input" type="number" step="0.0001" min={0} value={data.cache_write_per_mtok ?? ""} onChange={(e) => set("cache_write_per_mtok", e.target.value === "" ? null : Number(e.target.value))} placeholder="留空回退到输入价" />
        </FormField>
        <FormField label="启用">
          <label className="flex items-center gap-2 mt-1 cursor-pointer">
            <div className={`w-8 h-4 rounded-full relative transition-colors ${data.enabled ? "" : "bg-th-text-m"}`} style={data.enabled ? { background: "var(--color-accent)" } : {}} onClick={() => set("enabled", !data.enabled)}>
              <div className={`w-3.5 h-3.5 rounded-full bg-white absolute top-0.5 transition-all ${data.enabled ? "left-4" : "left-0.5"}`} />
            </div>
            <span className="text-sm text-th-text-s">{data.enabled ? "已启用" : "已禁用"}</span>
          </label>
        </FormField>
      </div>

      <div className="flex gap-3">
        <button onClick={onSave} disabled={saving} className="btn-primary">{saving ? "保存中..." : "保存"}</button>
        <button onClick={onCancel} className="btn-ghost">取消</button>
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
