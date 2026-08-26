/**
 * 审计日志详情面板
 */
import { useMemo, useState } from "react";
import type { AuditEntry } from "./AuditPanel";
import { formatLatency } from "../lib/format";

type Tab = "request" | "forwarded_request" | "upstream_response" | "response" | "headers" | "failover" | "meta";

/**
 * 正文预览上限。审计请求体常为 1~3MB 的单行 minified JSON，
 * 直接以 white-space:pre-wrap 渲染会让浏览器对整行做换行计算（实测 2.6MB 布局耗时约 25 秒）。
 * 先 pretty-print 拆成短行，再默认截断为预览，显著降低布局成本；完整内容由用户按需加载。
 */
const PREVIEW_LIMIT = 256 * 1024;

/** 尝试把 JSON 文本 pretty-print 成多行（短行布局远快于单行），非 JSON 原样返回 */
function prettyPrint(raw: string | null): string {
  if (!raw) return "(空)";
  try {
    return JSON.stringify(JSON.parse(raw), null, 1);
  } catch {
    return raw;
  }
}

export default function AuditDetail({ entry }: { entry: AuditEntry }) {
  const [tab, setTab] = useState<Tab>("request");

  const tabs: { key: Tab; label: string }[] = [
    { key: "request", label: "原始请求" },
    { key: "forwarded_request", label: "转发请求" },
    { key: "upstream_response", label: "原始响应" },
    { key: "response", label: "转发响应" },
    { key: "headers", label: "Headers" },
    { key: "failover", label: "故障转移" },
    { key: "meta", label: "元信息" },
  ];

  const copyToClipboard = (text: string) => { navigator.clipboard.writeText(text); };

  return (
    <div className="flex flex-col h-full">
      {/* Tab 导航 */}
      <div className="flex items-center border-b border-th-border px-4 py-2 gap-1 flex-wrap">
        {tabs.map((t) => (
          <button
            key={t.key}
            onClick={() => setTab(t.key)}
            className={`text-xs px-3 py-1.5 rounded-md transition-all duration-200 ${
              tab === t.key
                ? "bg-accent/15 text-th-accent font-medium"
                : "text-th-text-m hover:text-th-text hover:bg-th-hover"
            }`}
          >
            {t.label}
          </button>
        ))}
      </div>

      <div className="flex-1 overflow-auto p-4">
        {tab === "request" && (
          <JsonPanel title={`${entry.method} ${entry.path}`} content={entry.request_body} onCopy={copyToClipboard} />
        )}

        {tab === "forwarded_request" && (
          <JsonPanel title={`→ ${entry.actual_channel ?? "unknown"} (${entry.actual_model ?? "unknown"})`} content={entry.forwarded_request_body} onCopy={copyToClipboard} />
        )}

        {tab === "upstream_response" && (
          <JsonPanel title={`← ${entry.actual_channel ?? "unknown"}`} content={entry.upstream_response_body} onCopy={copyToClipboard} />
        )}

        {tab === "response" && (
          <div className="h-full flex flex-col">
            <div className="flex items-center justify-between mb-3 shrink-0">
              <span className="text-xs text-th-text-m font-mono bg-th-hover px-2.5 py-1 rounded-md border border-th-border">
                HTTP {entry.status_code} · {formatLatency(entry.latency_ms)} ·{" "}
                输入:{entry.input_tokens ?? 0} 输出:{entry.output_tokens ?? 0}{(entry.cache_read_tokens ?? 0) > 0 ? ` 缓存:${entry.cache_read_tokens}` : ""} tokens
              </span>
              <button onClick={() => copyToClipboard(entry.response_body ?? "")} className="text-xs text-th-accent hover:text-th-accent-d transition-colors bg-th-accent/10 hover:bg-th-accent/20 px-2.5 py-1 rounded-md">复制</button>
            </div>
            {entry.error_message && (
              <div className="mb-3 p-3 rounded-lg text-xs text-red-400 bg-red-400/10 border border-red-400/20 shrink-0">
                {entry.error_message}
              </div>
            )}
            <BodyView raw={entry.response_body} />
          </div>
        )}

        {tab === "headers" && (
          <div className="space-y-4">
            <HeaderSection title="原始请求 Headers" subtitle="调用方发来的请求头" raw={entry.request_headers} onCopy={copyToClipboard} />
            <HeaderSection title="转发请求 Headers" subtitle="实际发给渠道的请求头" raw={entry.forwarded_request_headers} onCopy={copyToClipboard} />
            <HeaderSection title="原始响应 Headers" subtitle="渠道返回的响应头" raw={entry.upstream_response_headers} onCopy={copyToClipboard} />
            <HeaderSection title="转发响应 Headers" subtitle="返回给调用方的响应头" raw={entry.response_headers} onCopy={copyToClipboard} />
          </div>
        )}

        {tab === "failover" && (
          <div className="h-full flex flex-col">
            <div className="flex items-center justify-between mb-3 shrink-0">
              <h4 className="text-sm font-medium text-th-text">重试次数: <span className="text-th-accent font-mono bg-th-accent/10 px-2 py-0.5 rounded-md ml-1">{entry.retry_count}</span></h4>
            </div>
            <BodyView raw={entry.failover_chain} />
          </div>
        )}

        {tab === "meta" && (
          <div className="space-y-2 text-sm">
            <MetaRow label="时间" value={new Date(entry.timestamp).toLocaleString()} />
            <MetaRow label="入口路径" value={entry.path} />
            <MetaRow label="调用方模型" value={entry.alias_model} />
            <MetaRow label="实际渠道" value={entry.actual_channel ?? "—"} />
            <MetaRow label="实际模型" value={entry.actual_model ?? "—"} />
            <MetaRow label="映射来源" value={entry.mapping_source ?? "—"} />
            <MetaRow label="状态码" value={String(entry.status_code ?? "—")} />
            <MetaRow label="首字节" value={formatLatency(entry.first_byte_ms)} />
            <MetaRow label="总耗时" value={formatLatency(entry.latency_ms)} />
            <MetaRow label="输入 Tokens" value={String(entry.input_tokens ?? 0)} />
            <MetaRow label="输出 Tokens" value={String(entry.output_tokens ?? 0)} />
            {(entry.cache_read_tokens ?? 0) > 0 && <MetaRow label="缓存读取" value={String(entry.cache_read_tokens)} />}
            {(entry.cache_creation_tokens ?? 0) > 0 && <MetaRow label="缓存创建" value={String(entry.cache_creation_tokens)} />}
            {entry.user_agent && <MetaRow label="User-Agent" value={entry.user_agent} />}
          </div>
        )}
      </div>
    </div>
  );
}

function JsonPanel({ title, content, onCopy }: {
  title: string; content: string | null; onCopy: (text: string) => void;
}) {
  return (
    <div className="h-full flex flex-col">
      <div className="flex items-center justify-between mb-3 shrink-0">
        <span className="text-sm font-medium text-th-text font-display">{title}</span>
        <button onClick={() => onCopy(content ?? "")} className="text-xs text-th-accent hover:text-th-accent-d transition-colors bg-th-accent/10 hover:bg-th-accent/20 px-2.5 py-1 rounded-md">复制</button>
      </div>
      <BodyView raw={content} />
    </div>
  );
}

/**
 * 报文正文视图：pretty-print JSON 拆成短行，超过预览上限时默认截断，
 * 提供「显示完整内容」按钮按需加载全文，避免大单行文本阻塞浏览器布局。
 */
function BodyView({ raw }: { raw: string | null }) {
  const [expanded, setExpanded] = useState(false);
  const text = useMemo(() => prettyPrint(raw), [raw]);
  const truncated = text.length > PREVIEW_LIMIT;
  const shown = truncated && !expanded ? text.slice(0, PREVIEW_LIMIT) : text;

  return (
    <div className="flex flex-col flex-1 min-h-0">
      <pre className="flex-1 text-xs font-mono p-4 rounded-xl overflow-auto whitespace-pre-wrap text-th-text bg-th-elev border border-th-border shadow-inner">
        {shown}
        {truncated && !expanded ? "\n…（内容过长，仅显示前 256 KB）" : ""}
      </pre>
      {truncated && (
        <button
          onClick={() => setExpanded((e) => !e)}
          className="mt-2 shrink-0 text-xs text-th-accent hover:text-th-accent-d transition-colors bg-th-accent/10 hover:bg-th-accent/20 px-2.5 py-1.5 rounded-md self-start"
        >
          {expanded ? "收起" : `显示完整内容（${Math.ceil(text.length / 1024)} KB，渲染可能较慢）`}
        </button>
      )}
    </div>
  );
}

function MetaRow({ label, value }: { label: string; value: string }) {
  return (
    <div className="flex py-1.5 border-b border-th-border last:border-0">
      <span className="w-28 text-th-text-m text-xs shrink-0">{label}</span>
      <span className="font-mono text-xs text-th-text">{value}</span>
    </div>
  );
}

function HeaderSection({ title, subtitle, raw, onCopy }: {
  title: string; subtitle: string; raw: string | null | undefined; onCopy: (text: string) => void;
}) {
  return (
    <div className="mb-6 last:mb-0">
      <div className="flex items-center justify-between mb-3">
        <div className="flex flex-col">
          <h4 className="text-sm font-medium text-th-text font-display">{title}</h4>
          <p className="text-[10px] text-th-text-m mt-0.5">{subtitle}</p>
        </div>
        <button onClick={() => onCopy(raw ?? "")} className="text-xs text-th-accent hover:text-th-accent-d transition-colors bg-th-accent/10 hover:bg-th-accent/20 px-2.5 py-1 rounded-md">复制</button>
      </div>
      <HeaderTable raw={raw} />
    </div>
  );
}

function HeaderTable({ raw }: { raw: string | null | undefined }) {
  if (!raw) return <div className="text-xs text-th-text-m py-2">(无数据)</div>;

  let headers: Record<string, string> = {};
  try { headers = JSON.parse(raw); } catch {
    return (
      <pre className="text-xs font-mono p-4 rounded-xl whitespace-pre-wrap text-th-text bg-th-elev border border-th-border">
        {raw}
      </pre>
    );
  }

  return (
    <div className="rounded-xl overflow-hidden border border-th-border bg-th-elev shadow-sm">
      <table className="w-full text-xs text-left">
        <thead className="bg-th-hover text-th-text-m border-b border-th-border">
          <tr>
            <th className="px-4 py-2.5 font-medium w-1/3">名称</th>
            <th className="px-4 py-2.5 font-medium">值</th>
          </tr>
        </thead>
        <tbody className="divide-y divide-th-border">
          {Object.entries(headers).map(([key, value]) => (
            <tr key={key} className="hover:bg-th-hover transition-colors">
              <td className="px-4 py-2.5 font-mono text-th-accent">{key}</td>
              <td className="px-4 py-2.5 font-mono break-all text-th-text">{value}</td>
            </tr>
          ))}
          {Object.keys(headers).length === 0 && (
            <tr><td colSpan={2} className="px-4 py-4 text-center text-th-text-m">无 Headers</td></tr>
          )}
        </tbody>
      </table>
    </div>
  );
}
