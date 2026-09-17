import { useEffect, useState } from "react";
import type { ReactNode } from "react";
import { invoke } from "../lib/tauri";
import { formatLatency, formatTokens, formatCost } from "../lib/format";

interface Stats {
  total_requests: number;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_creation_tokens: number;
  error_count: number;
  success_count: number;
  cost_usd: number;
}

interface ChannelHealthInfo {
  id: string;
  name: string;
  state: string;
  avg_first_byte_ms: number | null;
  avg_latency_ms: number | null;
  success_rate: number | null;
  total_requests: number;
  recent_failures: number;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_creation_tokens: number;
  cost_usd: number;
}

interface AuditPreview {
  id: number;
  timestamp: number;
  path: string;
  alias_model: string;
  status_code: number | null;
  latency_ms: number | null;
}

type Period = "today" | "7d" | "30d";

interface TokenHeatDay {
  date: string;
  tokens: number;
}

export default function Dashboard() {
  const [stats, setStats] = useState<Stats | null>(null);
  const [period, setPeriod] = useState<Period>("today");
  const [channels, setChannels] = useState<{ id: string; name: string }[]>([]);
  const [channelHealth, setChannelHealth] = useState<ChannelHealthInfo[]>([]);
  const [audits, setAudits] = useState<AuditPreview[]>([]);
  const [selectedChannel, setSelectedChannel] = useState("");
  const [loading, setLoading] = useState(true);
  const [heatmap, setHeatmap] = useState<TokenHeatDay[]>([]);

  useEffect(() => {
    invoke<{ channels: { id: string; name: string }[] }>("get_full_config")
      .then((cfg) => setChannels(cfg.channels.map((channel) => ({ id: channel.id, name: channel.name }))))
      .catch(() => setChannels([]));
    invoke<TokenHeatDay[]>("get_token_heatmap", { days: 365 })
      .then((d) => setHeatmap(d))
      .catch(() => setHeatmap([]));
  }, []);

  useEffect(() => { void loadData(); }, [period, selectedChannel]);

  async function loadData() {
    setLoading(true);
    try {
      const [statsData, healthData, auditData] = await Promise.all([
        invoke<Stats>("get_stats", { period, channel: selectedChannel || null }),
        invoke<ChannelHealthInfo[]>("get_channel_health", { period }),
        invoke<AuditPreview[]>("get_audit_logs", { limit: 5 }),
      ]);
      setStats(statsData);
      setChannelHealth(healthData);
      setAudits(auditData.slice(0, 5));
    } catch (error) {
      console.error(error);
    } finally {
      setLoading(false);
    }
  }

  const totalTokens = (stats?.input_tokens ?? 0) + (stats?.output_tokens ?? 0) + (stats?.cache_read_tokens ?? 0);
  const cacheRate = stats && (stats.input_tokens + stats.cache_read_tokens) > 0
    ? (stats.cache_read_tokens / (stats.input_tokens + stats.cache_read_tokens)) * 100
    : 0;
  const costColor = (stats?.cost_usd ?? 0) > 0 ? "amber" : "slate";
  const periodLabels: Record<Period, string> = { today: "今天", "7d": "7 天", "30d": "30 天" };

  return (
    <div className="space-y-6 pb-8">
      <div className="flex items-end justify-between gap-4">
        <div>
          <h1 className="font-display text-[26px] font-semibold tracking-tight text-th-text">仪表盘</h1>
          <p className="mt-2 text-sm text-th-text-s">实时查看代理请求、延迟和渠道健康度。</p>
        </div>
        <div className="flex items-center gap-3">
          <button onClick={() => void loadData()} disabled={loading} className="btn-ghost text-xs bg-th-base">↻ 刷新</button>
          <select className="input !w-[190px] !py-2 text-xs bg-th-base" value={selectedChannel} onChange={(event) => setSelectedChannel(event.target.value)}>
            <option value="">全部渠道</option>
            {channels.map((channel) => <option key={channel.id} value={channel.id}>{channel.name}</option>)}
          </select>
          <div className="flex gap-0.5 rounded-lg border border-th-border bg-th-base p-0.5">
            {(["today", "7d", "30d"] as Period[]).map((item) => <button key={item} onClick={() => setPeriod(item)} className={`rounded-md px-3 py-1.5 text-xs ${period === item ? "bg-[#eaf2fb] text-th-accent font-medium" : "text-th-text-s hover:text-th-text"}`}>{periodLabels[item]}</button>)}
          </div>
        </div>
      </div>

      {loading && !stats ? <LoadingState /> : <>
        <div className="grid grid-cols-1 gap-3 sm:grid-cols-4">
          <MetricCard label="总请求" value={(stats?.total_requests ?? 0).toLocaleString()} tone="slate" />
          <MetricCard label="成功" value={(stats?.success_count ?? 0).toLocaleString()} tone="green" />
          <MetricCard label="失败" value={(stats?.error_count ?? 0).toLocaleString()} tone="red" />
          <MetricCard label="成本" value={formatCost(stats?.cost_usd)} tone={costColor} />
        </div>
        <div className="grid grid-cols-2 gap-3 sm:grid-cols-3 xl:grid-cols-6">
          <MetricCard compact label="总 Token" value={formatTokens(totalTokens)} tone="cyan" />
          <MetricCard compact label="输入" value={formatTokens(stats?.input_tokens ?? 0)} tone="blue" />
          <MetricCard compact label="输出" value={formatTokens(stats?.output_tokens ?? 0)} tone="violet" />
          <MetricCard compact label="缓存读取" value={formatTokens(stats?.cache_read_tokens ?? 0)} tone="green" />
          <MetricCard compact label="缓存创建" value={formatTokens(stats?.cache_creation_tokens ?? 0)} tone="amber" />
          <MetricCard compact label="CACHE 率" value={`${cacheRate.toFixed(1)}%`} tone="cyan" />
        </div>

        <TokenHeatmap data={heatmap} />

        <section>
          <div className="mb-3 flex items-center justify-between"><h2 className="font-display text-lg font-semibold text-th-text">渠道状态</h2><span className="text-xs text-th-text-m">（{periodLabels[period]}）</span></div>
          <div className="grid gap-4 lg:grid-cols-3">
            {channelHealth.slice(0, 6).map((channel) => <ChannelCard key={channel.id} channel={channel} />)}
            {channelHealth.length === 0 && <div className="page-surface col-span-full p-8 text-center text-sm text-th-text-m">无已启用的渠道</div>}
          </div>
        </section>

        <div className="grid gap-5 xl:grid-cols-2">
          <PreviewCard title="审计日志" link="查看全部" href="/audit">
            <div className="mb-2 grid grid-cols-[64px_70px_1fr_90px_70px] gap-2 px-3 text-[11px] text-th-text-m"><span>ID</span><span>状态</span><span>入口</span><span>请求模型</span><span className="text-right">延迟</span></div>
            {audits.map((audit) => <div key={audit.id} className="grid grid-cols-[64px_70px_1fr_90px_70px] items-center gap-2 border-t border-th-border px-3 py-3 text-xs"><span className="font-mono text-th-text-s">#{audit.id}</span><span className="flex items-center gap-1.5"><i className={`h-2 w-2 rounded-full ${audit.status_code && audit.status_code >= 200 && audit.status_code < 300 ? "bg-emerald-500" : "bg-red-400"}`} />{audit.status_code ?? "—"}</span><span className="truncate rounded bg-th-bg px-2 py-1 font-mono text-[11px] text-th-text-s">{shortPath(audit.path)}</span><span className="truncate text-th-text-s">{audit.alias_model || "—"}</span><span className="text-right font-mono text-th-text-s">{audit.latency_ms === null ? "—" : `${audit.latency_ms}ms`}</span></div>)}
            {audits.length === 0 && <EmptyPreview />}
          </PreviewCard>
        </div>

        <section><h2 className="mb-3 font-display text-lg font-semibold text-th-text">快捷入口</h2><div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3"><QuickLink icon="▱" title="MCP 中继" detail="管理 MCP 服务" href="/mcp" /><QuickLink icon="≡" title="审计日志" detail="查看访问日志" href="/audit" /><QuickLink icon="⚙" title="系统设置" detail="系统参数配置" href="/settings" /></div></section>
      </>}
    </div>
  );
}

function MetricCard({ label, value, tone, compact = false }: { label: string; value: string; tone: "slate" | "green" | "red" | "cyan" | "blue" | "violet" | "amber"; compact?: boolean }) {
  const colors: Record<string, string> = { slate: "text-slate-700", green: "text-emerald-500", red: "text-red-500", cyan: "text-cyan-500", blue: "text-blue-500", violet: "text-violet-500", amber: "text-amber-500" };
  return <div className={`page-surface ${compact ? "min-h-[104px] p-4" : "min-h-[136px] p-5"}`}><div className="text-sm font-medium tracking-wide text-th-text-s">{label}</div><div className={`mt-${compact ? "4" : "5"} font-display font-semibold ${compact ? "text-[27px]" : "text-[36px]"} leading-none ${colors[tone]}`}>{value}</div></div>;
}

function ChannelCard({ channel }: { channel: ChannelHealthInfo }) {
  const healthy = channel.state === "healthy";
  const status = channel.state === "degraded" ? "降级" : channel.state === "unhealthy" ? "不可用" : channel.state === "recovering" ? "恢复中" : "健康";
  const total = channel.input_tokens + channel.output_tokens + channel.cache_read_tokens;
  return <div className="page-surface p-5"><div className="flex items-center justify-between border-b border-th-border pb-4"><div className="flex min-w-0 items-center gap-2.5"><span className={`h-2.5 w-2.5 shrink-0 rounded-full ${healthy ? "bg-emerald-500" : channel.state === "unhealthy" ? "bg-red-400" : "bg-amber-400"}`} /><span className="truncate font-display text-sm font-semibold text-th-text">{channel.name}</span></div><span className={`rounded-full px-2.5 py-1 text-[11px] ${healthy ? "bg-emerald-50 text-emerald-600" : "bg-amber-50 text-amber-600"}`}>{status}</span></div><div className="space-y-3 pt-4"><InfoRow label="首 Token" value={channel.avg_first_byte_ms === null ? "—" : formatLatency(channel.avg_first_byte_ms)} /><InfoRow label="平均延迟" value={channel.avg_latency_ms === null ? "—" : formatLatency(channel.avg_latency_ms)} /><InfoRow label="成功率" value={channel.success_rate === null ? "—" : `${channel.success_rate}%`} /><InfoRow label="请求数" value={String(channel.total_requests)} /><InfoRow label="总 Token" value={formatTokens(total)} /><InfoRow label="缓存命中率" value={total ? `${((channel.cache_read_tokens / total) * 100).toFixed(1)}%` : "0%"} /><InfoRow label="成本" value={formatCost(channel.cost_usd)} /></div></div>;
}

function InfoRow({ label, value }: { label: string; value: string }) { return <div className="flex items-center justify-between text-xs"><span className="text-th-text-s">{label}</span><span className="font-mono text-th-text">{value}</span></div>; }
function PreviewCard({ title, link, href, children }: { title: string; link: string; href: string; children: ReactNode }) { return <section className="page-surface overflow-hidden"><div className="flex items-center justify-between px-4 py-4"><h2 className="font-display text-base font-semibold text-th-text">{title}</h2><a className="text-xs font-medium text-th-accent hover:underline" href={href}>{link} →</a></div>{children}<div className="border-t border-th-border px-4 py-3 text-right"><a className="text-xs font-medium text-th-accent hover:underline" href={href}>查看更多 →</a></div></section>; }
function QuickLink({ icon, title, detail, href }: { icon: string; title: string; detail: string; href: string }) { return <a href={href} className="page-surface flex items-center gap-3 p-3 hover:border-th-accent/40 hover:bg-[#fbfdff]"><span className="flex h-10 w-10 items-center justify-center rounded-lg bg-[#eef5fb] text-lg text-th-accent">{icon}</span><span className="min-w-0"><span className="block text-sm font-medium text-th-text">{title}</span><span className="mt-1 block text-[11px] text-th-text-m">{detail}</span></span></a>; }
function EmptyPreview() { return <div className="border-t border-th-border px-3 py-8 text-center text-xs text-th-text-m">暂无数据</div>; }
function LoadingState() { return <div className="page-surface flex h-48 items-center justify-center text-sm text-th-text-m">加载中...</div>; }
function shortPath(path: string) { if (path.includes("messages")) return "/v1/messages"; if (path.includes("responses")) return "/v1/responses"; if (path.includes("chat")) return "/v1/chat/completions"; return path; }

// ========== Token 用量活动热力图 ==========
function TokenHeatmap({ data }: { data: TokenHeatDay[] }) {
  const DAYS = 365;
  const today = new Date();
  today.setHours(0, 0, 0, 0);

  // 找到当前周的周日（weekStart），然后回退 DAYS-1 天
  const endDate = new Date(today);
  const startDate = new Date(today);
  startDate.setDate(startDate.getDate() - (DAYS - 1));
  // 向前推到周日
  const dayOfWeek = startDate.getDay();
  startDate.setDate(startDate.getDate() - dayOfWeek);

  // 构建日期→token 查找表
  const tokenMap = new Map<string, number>();
  for (const d of data) {
    tokenMap.set(d.date, d.tokens);
  }

  // 计算色阶阈值（只在有值的天里算分位）
  const values = data.map(d => d.tokens).filter(t => t > 0);
  const level1 = values.length > 0 ? values[Math.floor(values.length * 0.25)] : 0;
  const level2 = values.length > 0 ? values[Math.floor(values.length * 0.75)] : 0;

  function getLevel(tokens: number): number {
    if (tokens === 0) return 0;
    if (tokens <= level1) return 1;
    if (tokens <= level2) return 2;
    return 3;
  }

  function formatDate(d: Date): string {
    return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`;
  }

  // 构建网格：行=周日~周六(7)，列=周
  const weeks: { date: Date; key: string; tokens: number; level: number }[][] = [];
  let current = new Date(startDate);
  let week: typeof weeks[0] = [];
  const totalDays = (DAYS + 7); // 多取一点保证覆盖

  for (let i = 0; i < totalDays; i++) {
    const key = formatDate(current);
    const tokens = tokenMap.get(key) ?? 0;
    const level = getLevel(tokens);
    week.push({ date: new Date(current), key, tokens, level });
    if (current.getDay() === 6 || i === totalDays - 1) {
      weeks.push(week);
      week = [];
    }
    current.setDate(current.getDate() + 1);
  }

  const dayLabels = ['日', '一', '二', '三', '四', '五', '六'];
  const monthNames = ['1月', '2月', '3月', '4月', '5月', '6月', '7月', '8月', '9月', '10月', '11月', '12月'];

  const [hovered, setHovered] = useState<{ date: string; tokens: number; x: number; y: number } | null>(null);

  return (
    <section className="page-surface p-4">
      <div className="flex items-center justify-between mb-3">
        <h2 className="font-display text-base font-semibold text-th-text">Token 用量</h2>
        <span className="text-xs text-th-text-m">最近 {DAYS} 天</span>
      </div>
      <div className="overflow-x-auto">
        <div className="inline-block min-w-full">
          {/* 月份标签行：与网格列一一对应，每列要么标月份，要么空占位 */}
          <div className="flex text-[10px] text-th-text-m mb-1" style={{ paddingLeft: '28px' }}>
            {weeks.map((w, colIdx) => {
              const firstCell = w[0];
              if (!firstCell) return <span key={colIdx} className="block shrink-0 flex-1 min-w-0" />;
              const m = firstCell.date.getMonth();
              const isFirstWeekOfMonth = colIdx === 0 || (weeks[colIdx - 1]?.[0]?.date.getMonth() ?? -1) !== m;
              return (
                <span key={colIdx} className="block shrink-0 flex-1 min-w-0 overflow-visible whitespace-nowrap">
                  {isFirstWeekOfMonth ? monthNames[m] : ''}
                </span>
              );
            })}
          </div>
          {/* 主体：左边星期 + 右边格子 */}
          <div className="flex gap-[1px]">
            {/* 星期标签列 */}
            <div className="flex flex-col gap-[1px] mr-1 shrink-0" style={{ width: '24px' }}>
              {dayLabels.map((label, i) => (
                <div key={i} className="flex items-center justify-end aspect-[1/1] text-[9px] text-th-text-m">{label}</div>
              ))}
            </div>
            {/* 格子 */}
            <div className="flex gap-[1px] flex-1 min-w-0">
              {weeks.map((w, colIdx) => (
                <div key={colIdx} className="flex flex-col gap-[1px] flex-1 min-w-0">
                  {dayLabels.map((_, rowIdx) => {
                    const cell = w[rowIdx];
                    if (!cell) return <div key={rowIdx} className="aspect-square rounded-sm" />;
                    const isFuture = cell.date > endDate;
                    return (
                      <div
                        key={rowIdx}
                        className="aspect-square rounded-sm transition-colors duration-75"
                        style={{
                          backgroundColor: isFuture ? 'transparent' :
                            cell.level === 0 ? 'var(--color-bg-hover)' :
                            cell.level === 1 ? `rgba(16,185,129,0.25)` :
                            cell.level === 2 ? `rgba(16,185,129,0.55)` :
                            `rgba(16,185,129,0.85)`,
                        }}
                        title={!isFuture ? `${cell.key}: ${formatTokens(cell.tokens)}` : ''}
                        onMouseEnter={(e) => {
                          if (isFuture) return;
                          const rect = (e.target as HTMLElement).getBoundingClientRect();
                          setHovered({ date: cell.key, tokens: cell.tokens, x: rect.left, y: rect.top - 36 });
                        }}
                        onMouseLeave={() => setHovered(null)}
                      />
                    );
                  })}
                </div>
              ))}
            </div>
          </div>
          {/* 图例 */}
          <div className="flex items-center gap-1.5 mt-2 justify-end">
            <span className="text-[9px] text-th-text-m">Less</span>
            {[0, 1, 2, 3].map((lvl) => (
              <div
                key={lvl}
                className="w-2.5 h-2.5 rounded-sm"
                style={{
                  backgroundColor: lvl === 0 ? 'var(--color-bg-hover)' :
                    lvl === 1 ? 'rgba(16,185,129,0.25)' :
                    lvl === 2 ? 'rgba(16,185,129,0.55)' :
                    'rgba(16,185,129,0.85)',
                }}
              />
            ))}
            <span className="text-[9px] text-th-text-m ml-0.5">More</span>
          </div>
        </div>
      </div>
      {/* Tooltip */}
      {hovered && (
        <div
          className="fixed z-50 pointer-events-none px-2 py-1 rounded bg-th-base border border-th-border shadow-lg text-[11px] text-th-text"
          style={{ left: hovered.x - 40, top: hovered.y }}
        >
          <span className="text-th-text-s">{hovered.date}</span>
          <span className="ml-2 font-mono font-medium">{formatTokens(hovered.tokens)}</span>
        </div>
      )}
    </section>
  );
}
