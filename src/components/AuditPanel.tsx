/**
 * 审计日志面板
 */
import { useEffect, useState } from "react";
import { invoke } from "../lib/tauri";
import { formatLatency, formatTokens, formatCost } from "../lib/format";
import AuditDetail from "./AuditDetail";

export interface AuditListItem {
  id: number;
  timestamp: number;
  method: string;
  path: string;
  alias_model: string;
  actual_channel: string | null;
  actual_model: string | null;
  mapping_source: string | null;
  status_code: number | null;
  first_byte_ms: number | null;
  latency_ms: number | null;
  input_tokens: number | null;
  output_tokens: number | null;
  cache_read_tokens: number | null;
  cache_creation_tokens: number | null;
  cost_usd: number | null;
  retry_count: number;
  error_message: string | null;
}

export interface AuditEntry {
  id: number;
  timestamp: number;
  method: string;
  path: string;
  request_headers: string | null;
  forwarded_request_headers: string | null;
  request_body: string | null;
  forwarded_request_body: string | null;
  alias_model: string;
  actual_channel: string | null;
  actual_model: string | null;
  mapping_source: string | null;
  upstream_response_headers: string | null;
  response_headers: string | null;
  upstream_response_body: string | null;
  response_body: string | null;
  status_code: number | null;
  first_byte_ms: number | null;
  latency_ms: number | null;
  input_tokens: number | null;
  output_tokens: number | null;
  cache_read_tokens: number | null;
  cache_creation_tokens: number | null;
  cost_usd: number | null;
  retry_count: number;
  error_message: string | null;
  failover_chain: string | null;
  user_agent: string | null;
}

interface ChannelOption {
  id: string;
  name: string;
}

const PATH_OPTIONS = [
  { label: "全部", value: "" },
  { label: "Chat", value: "/v1/chat/completions" },
  { label: "Messages", value: "/v1/messages" },
  { label: "Responses", value: "/v1/responses" },
];

const COLUMNS = [
  { id: "id", label: "ID" },
  { id: "status", label: "状态" },
  { id: "time", label: "时间" },
  { id: "path", label: "入口" },
  { id: "req_model", label: "请求模型" },
  { id: "channel", label: "渠道" },
  { id: "actual_model", label: "实际模型" },
  { id: "first_byte", label: "首字节" },
  { id: "latency", label: "总耗时" },
  { id: "input", label: "输入 Token" },
  { id: "output", label: "输出 Token" },
  { id: "cache_read", label: "缓存读" },
  { id: "cache_write", label: "缓存写" },
  { id: "cost", label: "成本" },
];

const DEFAULT_COLS = COLUMNS.map(c => c.id);

export default function AuditPanel() {
  const [entries, setEntries] = useState<AuditListItem[]>([]);
  const [detail, setDetail] = useState<AuditEntry | null>(null);
  const [detailLoading, setDetailLoading] = useState(false);
  const [showModal, setShowModal] = useState(false);
  const [filterModel, setFilterModel] = useState("");
  const [filterChannel, setFilterChannel] = useState("");
  const [filterStatus, setFilterStatus] = useState("");
  const [filterActualModel, setFilterActualModel] = useState("");
  const [filterPath, setFilterPath] = useState("");
  const [channels, setChannels] = useState<ChannelOption[]>([]);
  const [showColPicker, setShowColPicker] = useState(false);
  const [visibleCols, setVisibleCols] = useState<string[]>(() => {
    try {
      const saved = localStorage.getItem("audit_cols");
      if (saved) {
        const parsed = JSON.parse(saved);
        if (Array.isArray(parsed) && parsed.length > 0) return parsed;
      }
    } catch (e) {}
    return DEFAULT_COLS;
  });

  useEffect(() => {
    localStorage.setItem("audit_cols", JSON.stringify(visibleCols));
  }, [visibleCols]);

  const toggleCol = (id: string) => {
    setVisibleCols((prev) => 
      prev.includes(id) ? prev.filter(x => x !== id) : [...prev, id]
    );
  };

  useEffect(() => {
    invoke<ChannelOption[]>("get_channels")
      .then((data) => setChannels(data))
      .catch(() => setChannels([]));
  }, []);

  useEffect(() => { loadEntries(); }, [filterModel, filterChannel, filterStatus, filterActualModel, filterPath]);

  const loadEntries = async () => {
    try {
      const data = await invoke<AuditListItem[]>("get_audit_logs", {
        model_filter: filterModel || null,
        channel_filter: filterChannel || null,
        status_filter: filterStatus || null,
        actual_model_filter: filterActualModel || null,
        path_filter: filterPath || null,
        limit: 100,
      });
      setEntries(data);
    } catch (e) { console.error(e); }
  };

  const openDetail = async (id: number) => {
    setShowModal(true);
    setDetailLoading(true);
    try {
      const data = await invoke<AuditEntry>("get_audit_detail", { id });
      setDetail(data);
    } catch (e) { console.error(e); setDetail(null); }
    finally { setDetailLoading(false); }
  };

  const statusDot = (code: number | null) => {
    if (code === null || code === undefined) return "bg-amber-400 animate-pulse";
    if (code >= 200 && code < 300) return "bg-emerald-400";
    return "bg-red-400";
  };

  const pathShort = (path: string) => {
    if (path.includes("messages")) return "Messages";
    if (path.includes("responses")) return "Responses";
    if (path.includes("chat")) return "Chat";
    return path;
  };

  return (
    <div className="flex flex-col h-full">
      {/* Header + Filters */}
      <div className="flex items-center gap-3 mb-4 flex-wrap">
        <h2 className="font-display text-2xl font-semibold tracking-tight">审计日志</h2>
        <div className="flex-1" />
        <select className="input !w-auto text-xs" value={filterPath} onChange={(e) => setFilterPath(e.target.value)}>
          {PATH_OPTIONS.map((opt) => (
            <option key={opt.value} value={opt.value}>{opt.label}</option>
          ))}
        </select>
        <input
          type="text"
          placeholder="请求模型..."
          className="input !w-28 text-xs"
          value={filterModel}
          onChange={(e) => setFilterModel(e.target.value)}
        />
        <select className="input !w-auto text-xs" value={filterChannel} onChange={(e) => setFilterChannel(e.target.value)}>
          <option value="">全部渠道</option>
          {channels.map((c) => (
            <option key={c.id} value={c.id}>{c.name}</option>
          ))}
        </select>
        <input
          type="text"
          placeholder="实际模型..."
          className="input !w-28 text-xs"
          value={filterActualModel}
          onChange={(e) => setFilterActualModel(e.target.value)}
        />
        <select className="input !w-auto text-xs" value={filterStatus} onChange={(e) => setFilterStatus(e.target.value)}>
          <option value="">全部状态</option>
          <option value="success">成功</option>
          <option value="error">失败</option>
          <option value="pending">进行中</option>
        </select>
        <button onClick={loadEntries} className="btn-primary text-xs !px-3 !py-1.5">↻ 刷新</button>
        <div className="relative">
          <button onClick={() => setShowColPicker(!showColPicker)} className="btn-ghost text-xs !px-3 !py-1.5 flex items-center gap-1.5">
            <span>显示列 ▾</span>
          </button>
          {showColPicker && (
            <>
              <div className="fixed inset-0 z-30" onClick={() => setShowColPicker(false)} />
              <div className="absolute right-0 top-full mt-2 w-48 glass-card p-3 z-40 shadow-xl animate-fade-in-up">
                <div className="text-[10px] text-th-text-m mb-2 uppercase tracking-wider">自定义列</div>
                <div className="space-y-2 max-h-64 overflow-y-auto">
                  {COLUMNS.map(c => (
                    <label key={c.id} className="flex items-center gap-2 text-xs cursor-pointer hover:text-th-accent transition-colors">
                      <input 
                        type="checkbox" 
                        className="rounded border-th-border text-th-accent focus:ring-th-accent-glow"
                        checked={visibleCols.includes(c.id)}
                        onChange={() => toggleCol(c.id)}
                        disabled={visibleCols.length === 1 && visibleCols.includes(c.id)}
                      />
                      <span>{c.label}</span>
                    </label>
                  ))}
                </div>
              </div>
            </>
          )}
        </div>
      </div>

      {/* Table */}
      <div className="flex-1 overflow-auto relative -mx-6 px-6">
        <table className="w-full text-sm min-w-max whitespace-nowrap">
          <thead className="sticky top-0 text-[10px] uppercase tracking-wider text-th-text-m z-10" style={{ background: "var(--color-bg)", backdropFilter: "blur(8px)" }}>
            <tr>
              {visibleCols.includes("id") && <th className="px-4 py-3 text-left">ID</th>}
              {visibleCols.includes("status") && <th className="px-4 py-3 text-left">状态</th>}
              {visibleCols.includes("time") && <th className="px-4 py-3 text-left">时间</th>}
              {visibleCols.includes("path") && <th className="px-4 py-3 text-left">入口</th>}
              {visibleCols.includes("req_model") && <th className="px-4 py-3 text-left">请求模型</th>}
              {visibleCols.includes("channel") && <th className="px-4 py-3 text-left">渠道</th>}
              {visibleCols.includes("actual_model") && <th className="px-4 py-3 text-left">实际模型</th>}
              {visibleCols.includes("first_byte") && <th className="px-4 py-3 text-right">首字节</th>}
              {visibleCols.includes("latency") && <th className="px-4 py-3 text-right">总耗时</th>}
              {visibleCols.includes("input") && <th className="px-4 py-3 text-right">输入</th>}
              {visibleCols.includes("output") && <th className="px-4 py-3 text-right">输出</th>}
              {visibleCols.includes("cache_read") && <th className="px-4 py-3 text-right">缓存读</th>}
              {visibleCols.includes("cache_write") && <th className="px-4 py-3 text-right">缓存写</th>}
              {visibleCols.includes("cost") && <th className="px-4 py-3 text-right">成本</th>}
            </tr>
          </thead>
          <tbody>
            {entries.map((e) => (
              <tr
                key={e.id}
                onClick={() => openDetail(e.id)}
                className="cursor-pointer hover:bg-th-hover/50 transition-colors group"
              >
              {visibleCols.includes("id") && <td className="px-4 py-3 font-mono text-xs text-th-text-s">#{e.id}</td>}
              {visibleCols.includes("status") && (
                <td className="px-4 py-3">
                  <div className="flex items-center gap-2">
                    <span className={`w-2 h-2 rounded-full ${statusDot(e.status_code)}`} />
                    <span className="text-xs text-th-text-m font-mono">{e.status_code ?? "..."}</span>
                  </div>
                </td>
              )}
              {visibleCols.includes("time") && <td className="px-4 py-3 font-mono text-xs text-th-text-s">{new Date(e.timestamp).toLocaleTimeString()}</td>}
              {visibleCols.includes("path") && (
                <td className="px-4 py-3">
                  <span className="text-[10px] px-2 py-0.5 rounded-md text-th-text-s" style={{ background: "var(--color-bg-elevated)", border: "1px solid var(--color-border)" }}>
                    {pathShort(e.path)}
                  </span>
                </td>
              )}
              {visibleCols.includes("req_model") && <td className="px-4 py-3 font-mono text-xs text-th-text">{e.alias_model}</td>}
              {visibleCols.includes("channel") && <td className="px-4 py-3 text-xs text-th-text-m">{e.actual_channel ?? "—"}</td>}
              {visibleCols.includes("actual_model") && <td className="px-4 py-3 font-mono text-xs text-th-text-m">{e.actual_model ?? "—"}</td>}
              {visibleCols.includes("first_byte") && <td className="px-4 py-3 text-right text-xs text-th-text-m font-mono">{formatLatency(e.first_byte_ms)}</td>}
              {visibleCols.includes("latency") && <td className="px-4 py-3 text-right text-xs font-mono text-th-text">{formatLatency(e.latency_ms)}</td>}
              {visibleCols.includes("input") && <td className="px-4 py-3 text-right text-xs text-th-text-m font-mono">{formatTokens(e.input_tokens)}</td>}
              {visibleCols.includes("output") && <td className="px-4 py-3 text-right text-xs text-th-text-m font-mono">{formatTokens(e.output_tokens)}</td>}
              {visibleCols.includes("cache_read") && <td className="px-4 py-3 text-right text-xs text-emerald-500 font-mono">{(e.cache_read_tokens ?? 0) > 0 ? formatTokens(e.cache_read_tokens) : "—"}</td>}
              {visibleCols.includes("cache_write") && <td className="px-4 py-3 text-right text-xs text-amber-500 font-mono">{(e.cache_creation_tokens ?? 0) > 0 ? formatTokens(e.cache_creation_tokens) : "—"}</td>}
              {visibleCols.includes("cost") && <td className="px-4 py-3 text-right text-xs text-amber-500 font-mono">{formatCost(e.cost_usd)}</td>}
              </tr>
            ))}
            {entries.length === 0 && (
              <tr><td colSpan={visibleCols.length} className="px-4 py-12 text-center text-th-text-m">暂无记录</td></tr>
            )}
          </tbody>
        </table>
      </div>

      {/* Modal */}
      {showModal && (
        <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/40 backdrop-blur-sm" onClick={() => setShowModal(false)}>
          <div className="bg-th-base border border-th-border rounded-2xl shadow-2xl shadow-black/20 w-[90vw] max-w-5xl h-[85vh] flex flex-col animate-slide-up overflow-hidden" onClick={(ev) => ev.stopPropagation()}>
            <div className="flex items-center justify-between px-5 py-3 border-b border-th-border shrink-0 bg-th-elev">
              <span className="font-display font-medium text-th-text">
                审计详情 {detail ? <span className="text-th-text-m font-mono text-sm">#{detail.id}</span> : ""}
              </span>
              <button onClick={() => setShowModal(false)} className="text-th-text-m hover:text-th-text transition-colors text-lg px-2">✕</button>
            </div>
            <div className="flex-1 overflow-auto">
              {detailLoading ? (
                <div className="flex items-center justify-center h-full text-th-text-m">
                  <div className="flex items-center gap-3">
                    <div className="w-4 h-4 border-2 border-accent/30 border-t-accent rounded-full animate-spin" />
                    加载中...
                  </div>
                </div>
              ) : detail ? (
                <AuditDetail entry={detail} />
              ) : (
                <div className="flex items-center justify-center h-full text-th-text-m">加载失败</div>
              )}
            </div>
          </div>
        </div>
      )}
    </div>
  );
}
