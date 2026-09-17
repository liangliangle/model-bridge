/**
 * 后端 API 调用封装
 *
 * 将前端组件的 invoke(command, args) 调用映射为 HTTP 请求。
 * GET 请求用于读取数据，POST 请求用于修改数据。
 * 开发模式下通过 Vite proxy 转发，生产模式下直接请求同源。
 */

const API_BASE = "/api";
const ADMIN_TOKEN_STORAGE_KEY = "model-bridge-admin-token";

export interface AuthStatus {
  required: boolean;
  valid: boolean;
}

export function getAdminToken(): string {
  return localStorage.getItem(ADMIN_TOKEN_STORAGE_KEY) ?? "";
}

export function setAdminToken(token: string): void {
  localStorage.setItem(ADMIN_TOKEN_STORAGE_KEY, token);
}

export function clearAdminToken(): void {
  localStorage.removeItem(ADMIN_TOKEN_STORAGE_KEY);
}

class ApiAuthError extends Error {
  readonly status = 401;

  constructor(message: string) {
    super(message);
    this.name = "ApiAuthError";
  }
}

/** GET 请求的命令 → 端点映射 */
const GET_ENDPOINTS: Record<string, string> = {
  get_auth_status: "/auth/status",
  get_stats: "/stats",                 // 获取统计概览（24h 内请求数、token 等）
  get_token_heatmap: "/stats/heatmap", // 获取 token 用量热力图数据（按天聚合）
  get_audit_logs: "/audit",            // 获取审计日志列表（轻量）
  get_audit_detail: "/audit/detail",   // 获取单条审计详情（完整报文）
  get_audit_db_status: "/audit/db/status", // 审计数据库概况（大小、记录数）
  get_channels: "/channels",           // 获取渠道列表（简略信息）
  get_channel_health: "/channels/health", // 获取渠道健康状态
  get_full_config: "/config",          // 获取完整配置（含渠道详情和 API Key）
  get_mcp_oauth_status: "/config/mcp/oauth/status", // 查询 MCP server OAuth 授权状态
  get_model_prices: "/model-prices",   // 获取模型价格列表
};

/** POST 请求的命令 → 端点映射 */
const POST_ENDPOINTS: Record<string, string> = {
  save_channel: "/config/channel",         // 保存/新建渠道
  delete_channel: "/config/channel/delete", // 删除渠道
  save_failover_config: "/config/failover", // 保存故障转移配置
  save_settings: "/config/settings",       // 保存系统设置（端口、host、模型列表）
  test_channel: "/config/channel/test",    // 测试渠道连通性
  save_mcp_server: "/config/mcp",          // 保存/新建 MCP 中继 server
  delete_mcp_server: "/config/mcp/delete", // 删除 MCP 中继 server
  start_mcp_oauth: "/config/mcp/oauth/start", // 触发 MCP server OAuth 授权
  fetch_mcp_tools: "/config/mcp/tools/fetch", // 主动拉取上游工具列表
  toggle_mcp_tool: "/config/mcp/tools/toggle", // 启用/禁用单个工具
  save_model_price: "/model-prices",       // 保存/新建模型价格
  delete_model_price: "/model-prices/delete", // 删除模型价格
  force_cleanup_audit: "/audit/cleanup",       // 强制清理审计数据并回收磁盘空间
};

/**
 * 调用后端 API
 *
 * @param cmd - 命令名称（对应上面的映射表）
 * @param args - 参数对象。GET 请求作为 query string，POST 请求取第一个值作为 body
 * @returns 后端返回的 JSON 数据
 */
export async function invoke<T>(cmd: string, args?: Record<string, unknown>): Promise<T> {
  const headers: Record<string, string> = {};
  const adminToken = getAdminToken();
  if (adminToken) headers.Authorization = `Bearer ${adminToken}`;

  const request = async (url: string, init?: RequestInit): Promise<T> => {
    const response = await fetch(url, {
      ...init,
      headers: { ...headers, ...(init?.headers ?? {}) },
    });
    const data = await response.json() as T & { error?: { message?: string } };
    if (response.status === 401) {
      clearAdminToken();
      window.dispatchEvent(new CustomEvent("model-bridge-auth-expired"));
      throw new ApiAuthError(data.error?.message ?? "Invalid or missing admin token");
    }
    return data;
  };

  // GET 请求
  if (GET_ENDPOINTS[cmd]) {
    let params = "";
    if (args) {
      // 过滤掉 null/undefined/空字符串，避免传入无效参数
      const filtered = Object.entries(args)
        .filter(([_, v]) => v !== null && v !== undefined && v !== "")
        .map(([k, v]) => [k, String(v)]);
      if (filtered.length > 0) {
        params = "?" + new URLSearchParams(filtered).toString();
      }
    }
    return request(`${API_BASE}${GET_ENDPOINTS[cmd]}${params}`);
  }

  // POST 请求
  if (POST_ENDPOINTS[cmd]) {
    // 大多数 POST 命令：args = { channel: {...} }，取第一个 value 作为 body
    // 特殊命令（如 delete_channel）：args = { channelId: "xxx" }，需要整个 args 作为 body
    let body: unknown;
    if (args) {
      const firstValue = Object.values(args)[0];
      // 如果第一个值是对象，直接用它作为请求体；否则用整个 args 对象
      body = typeof firstValue === "object" && firstValue !== null ? firstValue : args;
    } else {
      body = {};
    }
    return request(`${API_BASE}${POST_ENDPOINTS[cmd]}`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    });
  }

  throw new Error(`Unknown command: ${cmd}`);
}
