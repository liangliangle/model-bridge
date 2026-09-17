import { useEffect, useState } from "react";
import { invoke } from "../lib/tauri";

interface FullConfig {
  listen_port: number;
  listen_host: string;
  public_url: string | null;
  models: string[];
  failover: { max_failover_channels: number; retry_timeout_ms: number; failure_threshold: number; recovery_interval_sec: number; probe_requests: number };
  auth: { proxy_tokens: string[]; admin_token: string | null };
  ultimate_fallback_channel: string | null;
  channels: { id: string; name: string }[];
  audit_retention_days: number;
}

interface DbStatus {
  size_bytes: number;
  total_records: number;
  detail_records: number;
}

interface CleanupResult {
  deleted_old: number;
  pruned: number;
  size_before: number;
  size_after: number;
}

function formatBytes(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  const units = ["KB", "MB", "GB", "TB"];
  let value = bytes / 1024;
  let i = 0;
  while (value >= 1024 && i < units.length - 1) { value /= 1024; i++; }
  return `${value.toFixed(value >= 100 ? 0 : 1)} ${units[i]}`;
}

export default function Settings() {
  const [port, setPort] = useState(8080);
  const [host, setHost] = useState("127.0.0.1");
  const [publicUrl, setPublicUrl] = useState("");
  const [models, setModels] = useState("");
  const [failover, setFailover] = useState({ max_failover_channels: 3, retry_timeout_ms: 5000, failure_threshold: 3, recovery_interval_sec: 30, probe_requests: 2 });
  const [proxyTokens, setProxyTokens] = useState("");
  const [adminToken, setAdminToken] = useState("");
  const [ultimateFallback, setUltimateFallback] = useState("");
  const [channelIds, setChannelIds] = useState<{ id: string; name: string }[]>([]);
  const [retentionDays, setRetentionDays] = useState(0);
  const [saving, setSaving] = useState(false);
  const [message, setMessage] = useState("");
  const [dbStatus, setDbStatus] = useState<DbStatus | null>(null);
  const [cleaning, setCleaning] = useState(false);
  const [cleanupResult, setCleanupResult] = useState<CleanupResult | null>(null);

  useEffect(() => { loadConfig(); void loadDbStatus(); }, []);

  const loadConfig = async () => {
    try {
      const data = await invoke<FullConfig>("get_full_config");
      setPort(data.listen_port); setHost(data.listen_host || "127.0.0.1");
      setPublicUrl(data.public_url || "");
      setModels((data.models || []).join("\n")); setFailover(data.failover);
      setProxyTokens((data.auth?.proxy_tokens || []).join("\n")); setAdminToken(data.auth?.admin_token || "");
      setUltimateFallback(data.ultimate_fallback_channel || ""); setChannelIds(data.channels || []);
      setRetentionDays(data.audit_retention_days ?? 0);
    } catch (e) { console.error(e); }
  };

  const handleSave = async () => {
    setSaving(true);
    try {
      await invoke("save_settings", {
        settings: {
          listen_port: port, listen_host: host,
          public_url: publicUrl.trim() || null,
          models: models.split("\n").map((m) => m.trim()).filter((m) => m.length > 0),
          auth: { proxy_tokens: proxyTokens.split("\n").map((t) => t.trim()).filter((t) => t.length > 0), admin_token: adminToken.trim() || null },
          ultimate_fallback_channel: ultimateFallback.trim() || null,
          audit_retention_days: retentionDays,
        },
      });
      await invoke("save_failover_config", { failover });
      showMessage("保存成功，部分设置需要重启后生效");
    } catch (e) { showMessage(`保存失败: ${e}`); }
    finally { setSaving(false); }
  };

  const showMessage = (msg: string) => { setMessage(msg); setTimeout(() => setMessage(""), 4000); };

  const loadDbStatus = async () => {
    try {
      const data = await invoke<DbStatus>("get_audit_db_status");
      setDbStatus(data);
    } catch (e) { console.error(e); }
  };

  const handleCleanup = async () => {
    const confirmed = window.confirm(
      "将删除 30 天前的审计记录，并将请求/响应详情只保留最近 1000 条，随后重建数据库以回收磁盘空间。\n\n" +
      "列表与看板统计数据不受影响，但较早记录的详情页将显示为空。\n" +
      "数据库较大时此操作可能持续数分钟，期间代理请求会短暂受阻。\n\n确定继续吗？"
    );
    if (!confirmed) return;
    setCleaning(true);
    setCleanupResult(null);
    try {
      const result = await invoke<CleanupResult>("force_cleanup_audit");
      setCleanupResult(result);
      await loadDbStatus();
      showMessage("清理完成");
    } catch (e) {
      showMessage(`清理失败: ${e}`);
    } finally {
      setCleaning(false);
    }
  };

  return (
    <div>
      <div className="flex items-center justify-between mb-6">
        <h2 className="font-display text-2xl font-semibold tracking-tight">系统设置</h2>
        {message && <span className="text-xs px-3 py-1.5 rounded-lg bg-emerald-500/10 text-emerald-400 border border-emerald-500/20">{message}</span>}
      </div>

      <div className="space-y-5 max-w-2xl">
        <Section title="网络设置" description="修改后需要重启服务才能生效">
          <div className="grid grid-cols-2 gap-4">
            <FormField label="监听地址">
              <input className="input" value={host} onChange={(e) => setHost(e.target.value)} placeholder="127.0.0.1" />
              <p className="text-[10px] text-th-text-m mt-1">0.0.0.0 = 所有网卡，127.0.0.1 = 仅本机</p>
            </FormField>
            <FormField label="监听端口">
              <input className="input" type="number" value={port} onChange={(e) => setPort(Number(e.target.value))} min={1} max={65535} />
            </FormField>
            <div className="col-span-2">
              <FormField label="公网地址 (可选)">
                <input className="input" value={publicUrl} onChange={(e) => setPublicUrl(e.target.value)} placeholder="https://api.example.com" />
                <p className="text-[10px] text-th-text-m mt-1">仪表盘中的代理端点复制地址将优先使用此配置。留空则默认使用本地 IP 和端口。</p>
              </FormField>
            </div>
          </div>
        </Section>

        <Section title="鉴权" description="控制代理端点和管理 API 的访问权限。留空则不启用鉴权。">
          <div className="space-y-4">
            <FormField label="代理端点访问令牌">
              <textarea className="input h-20 font-mono text-xs" value={proxyTokens} onChange={(e) => setProxyTokens(e.target.value)} placeholder={"每行一个 token\nsk-my-token-1\nsk-my-token-2"} />
              <p className="text-[10px] text-th-text-m mt-1">客户端需在 Authorization: Bearer &lt;token&gt; 中携带</p>
            </FormField>
            <FormField label="管理后台令牌">
              <input className="input" type="password" value={adminToken} onChange={(e) => setAdminToken(e.target.value)} placeholder="留空则管理 API 不需要鉴权" />
            </FormField>
          </div>
        </Section>

        <Section title="终极兜底" description="当所有渠道都被熔断时，使用此渠道作为最后的兜底。">
          <FormField label="兜底渠道">
            <select className="input" value={ultimateFallback} onChange={(e) => setUltimateFallback(e.target.value)}>
              <option value="">不设置</option>
              {channelIds.map((ch) => <option key={ch.id} value={ch.id}>{ch.name} ({ch.id})</option>)}
            </select>
            <p className="text-[10px] text-th-text-m mt-1">即使该渠道已被熔断，也会作为最后手段尝试</p>
          </FormField>
        </Section>

        <Section title="模型列表" description="/v1/models 接口返回的模型名称。留空则自动从渠道映射中收集。">
          <textarea className="input h-32 font-mono text-xs" value={models} onChange={(e) => setModels(e.target.value)} placeholder={"留空自动收集，或每行一个模型名\ngpt-4\ngpt-4o\nclaude-sonnet"} />
        </Section>

        <Section title="故障转移" description="控制渠道故障时的切换策略">
          <div className="grid grid-cols-2 gap-4">
            <FormField label="候选渠道上限">
              <input className="input" type="number" value={failover.max_failover_channels} onChange={(e) => setFailover({ ...failover, max_failover_channels: Number(e.target.value) })} min={0} max={50} />
              <p className="text-[10px] text-th-text-m mt-1">单次请求最多尝试几个渠道；0 表示不限制。单渠道内的重试次数请在各渠道配置中单独设置</p>
            </FormField>
            <FormField label="重试超时 (ms)">
              <input className="input" type="number" value={failover.retry_timeout_ms} onChange={(e) => setFailover({ ...failover, retry_timeout_ms: Number(e.target.value) })} min={1000} step={1000} />
            </FormField>
            <FormField label="熔断失败阈值">
              <input className="input" type="number" value={failover.failure_threshold} onChange={(e) => setFailover({ ...failover, failure_threshold: Number(e.target.value) })} min={1} />
              <p className="text-[10px] text-th-text-m mt-1">连续失败几次后熔断该渠道</p>
            </FormField>
            <FormField label="恢复间隔 (秒)">
              <input className="input" type="number" value={failover.recovery_interval_sec} onChange={(e) => setFailover({ ...failover, recovery_interval_sec: Number(e.target.value) })} min={5} />
              <p className="text-[10px] text-th-text-m mt-1">熔断后多久尝试恢复</p>
            </FormField>
            <FormField label="恢复探测次数">
              <input className="input" type="number" value={failover.probe_requests} onChange={(e) => setFailover({ ...failover, probe_requests: Number(e.target.value) })} min={1} />
              <p className="text-[10px] text-th-text-m mt-1">恢复探测需要连续成功几次</p>
            </FormField>
          </div>
        </Section>

        <Section title="数据清理" description="审计记录按保留天数清理，请求/响应详情只保留最近 1000 条。列表与看板统计不受影响。">
          <div className="flex items-center gap-6 mb-4">
            <Stat label="数据库大小" value={dbStatus ? formatBytes(dbStatus.size_bytes) : "—"} />
            <Stat label="审计记录" value={dbStatus ? String(dbStatus.total_records) : "—"} />
            <Stat label="详情记录" value={dbStatus ? String(dbStatus.detail_records) : "—"} />
          </div>
          <FormField label="审计记录保留天数">
            <input className="input" type="number" value={retentionDays} onChange={(e) => setRetentionDays(Math.max(0, Number(e.target.value)))} min={0} placeholder="0" />
            <p className="text-[10px] text-th-text-m mt-1">0 = 永久留存。保存后生效：超过该天数的审计记录会在下次启动或每日清理时删除。</p>
          </FormField>
          <button onClick={handleCleanup} disabled={cleaning} className="btn-primary mt-4">
            {cleaning ? "清理中，请耐心等待..." : "立即清理并回收空间"}
          </button>
          {cleanupResult && (
            <p className="text-[11px] text-th-text-m mt-3">
              已删除超期记录 {cleanupResult.deleted_old} 条，裁剪详情 {cleanupResult.pruned} 条；
              数据库大小 {formatBytes(cleanupResult.size_before)} → {formatBytes(cleanupResult.size_after)}
              （释放 {formatBytes(Math.max(0, cleanupResult.size_before - cleanupResult.size_after))}）
            </p>
          )}
        </Section>

        <div className="pt-4 border-t border-th-border">
          <button onClick={handleSave} disabled={saving} className="btn-primary">{saving ? "保存中..." : "保存设置"}</button>
          <span className="text-[10px] text-th-text-m ml-3">网络设置修改后需要重启服务</span>
        </div>
      </div>
    </div>
  );
}

function Section({ title, description, children }: { title: string; description?: string; children: React.ReactNode }) {
  return (
    <div className="glass-card p-5">
      <h3 className="font-display font-medium text-th-text mb-1">{title}</h3>
      {description && <p className="text-[10px] text-th-text-m mb-4">{description}</p>}
      {children}
    </div>
  );
}

function FormField({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div>
      <label className="block text-[10px] text-th-text-m mb-1 uppercase tracking-wider">{label}</label>
      {children}
    </div>
  );
}

function Stat({ label, value }: { label: string; value: string }) {
  return (
    <div>
      <div className="text-[10px] text-th-text-m uppercase tracking-wider">{label}</div>
      <div className="font-display text-lg font-semibold text-th-text">{value}</div>
    </div>
  );
}
