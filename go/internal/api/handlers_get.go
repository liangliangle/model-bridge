// GET 类管理端点，逐个对应 Rust `src-tauri/src/commands.rs` 的同名函数。
package api

import (
	"log"
	"net/http"
	"strconv"
	"time"

	"modelbridge/internal/app"
	"modelbridge/internal/audit"
	"modelbridge/internal/config"
)

// ---------- /api/auth/status ----------

// authStatusResp 对应 Rust `get_auth_status` 的 `json!({"required":…,"valid":…})`。
type authStatusResp struct {
	Required bool `json:"required"`
	Valid    bool `json:"valid"`
}

// handleAuthStatus 对应 Rust `get_auth_status`（commands.rs:135）：报告是否启用管理端
// 鉴权、以及当前请求是否携带有效 token；永不返回 token 本身，也不做鉴权拦截。
func handleAuthStatus(state *app.State) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		adminToken := state.Config().Auth.AdminToken
		required := adminToken != nil && *adminToken != ""
		valid := !required || adminAuthOK(state, r)
		writeOK(w, authStatusResp{Required: required, Valid: valid})
	}
}

// ---------- /api/stats ----------

// handleStats 对应 Rust `get_stats`（commands.rs:152）：
// 查询参数 period（缺省 today）与 channel（可选）→ 统计概览。
func handleStats(state *app.State) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireAdmin(w, r, state) {
			return
		}
		q := r.URL.Query()
		period := q.Get("period")
		if period == "" {
			period = "today"
		}
		var channel *string
		if c := q.Get("channel"); c != "" {
			channel = &c
		}
		stats, err := state.Audit.GetStats(period, channel)
		if err != nil {
			log.Printf("api: get_stats 失败: %v", err)
			writeErr(w, err)
			return
		}
		writeOK(w, stats)
	}
}

// ---------- /api/stats/heatmap ----------

// handleTokenHeatmap 对应 Rust `get_token_heatmap`（commands.rs:274）：
// 按天聚合 token 用量，days 缺省 91。
func handleTokenHeatmap(state *app.State) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireAdmin(w, r, state) {
			return
		}
		days := uint32(91)
		if raw := r.URL.Query().Get("days"); raw != "" {
			if v, err := strconv.ParseUint(raw, 10, 32); err == nil {
				days = uint32(v)
			}
		}
		data, err := state.Audit.GetTokenHeatmap(days)
		if err != nil {
			log.Printf("api: get_token_heatmap 失败: %v", err)
			writeErr(w, err)
			return
		}
		writeOK(w, data)
	}
}

// ---------- /api/audit ----------

// handleAuditLogs 对应 Rust `get_audit_logs`（commands.rs:177）：审计列表（轻量）。
//
// 查询参数：model_filter / channel_filter / status_filter / actual_model_filter /
// path_filter / time_from / time_to / limit（默认 100）。
func handleAuditLogs(state *app.State) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireAdmin(w, r, state) {
			return
		}
		q := r.URL.Query()
		limit := uint32(100)
		if raw := q.Get("limit"); raw != "" {
			if v, err := strconv.ParseUint(raw, 10, 32); err == nil {
				limit = uint32(v)
			}
		}
		items, err := state.Audit.QueryList(audit.QueryListParams{
			ModelFilter:       q.Get("model_filter"),
			ChannelFilter:     q.Get("channel_filter"),
			StatusFilter:      q.Get("status_filter"),
			ActualModelFilter: q.Get("actual_model_filter"),
			PathFilter:        q.Get("path_filter"),
			TimeFrom:          q.Get("time_from"),
			TimeTo:            q.Get("time_to"),
			Limit:             limit,
		})
		if err != nil {
			log.Printf("api: get_audit_logs 失败: %v", err)
			writeErr(w, err)
			return
		}
		writeOK(w, items)
	}
}

// ---------- /api/audit/detail ----------

// handleAuditDetail 对应 Rust `get_audit_detail`（commands.rs:203）：
// `?id=<i64>`，不存在时返回 `{"error":"Not found"}`（HTTP 200）。
func handleAuditDetail(state *app.State) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireAdmin(w, r, state) {
			return
		}
		raw := r.URL.Query().Get("id")
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			// Rust 侧由 axum 的 Query 提取器拒绝（400）；这里保持 JSON 错误体。
			writeAPIError(w, http.StatusBadRequest, "Bad request: invalid id")
			return
		}
		entry, err := state.Audit.GetByID(id)
		if err != nil {
			log.Printf("api: get_audit_detail 失败: %v", err)
			writeErr(w, err)
			return
		}
		if entry == nil {
			writeAPIError(w, http.StatusOK, "Not found")
			return
		}
		writeOK(w, entry)
	}
}

// ---------- /api/audit/db/status ----------

// handleAuditDBStatus 对应 Rust `get_audit_db_status`（commands.rs:216）：
// 审计库概况 `{size_bytes, total_records, detail_records}`。
func handleAuditDBStatus(state *app.State) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireAdmin(w, r, state) {
			return
		}
		st, err := state.Audit.Status()
		if err != nil {
			log.Printf("api: get_audit_db_status 失败: %v", err)
			writeErr(w, err)
			return
		}
		writeOK(w, st)
	}
}

// ---------- /api/channels ----------

// channelInfo 对应 Rust `ChannelInfo`（commands.rs:17-27）：不含 API Key 的渠道概要。
type channelInfo struct {
	ID            string            `json:"id"`
	Name          string            `json:"name"`
	Provider      string            `json:"provider"`
	URL           string            `json:"url"`
	Priority      uint32            `json:"priority"`
	Enabled       bool              `json:"enabled"`
	FallbackModel string            `json:"fallback_model"`
	ModelMapping  map[string]string `json:"model_mapping"`
}

// handleChannels 对应 Rust `get_channels`（commands.rs:246）：渠道简略列表。
func handleChannels(state *app.State) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireAdmin(w, r, state) {
			return
		}
		cfg := state.Config()
		out := make([]channelInfo, 0, len(cfg.Channels))
		for i := range cfg.Channels {
			c := &cfg.Channels[i]
			out = append(out, channelInfo{
				ID:            c.ID,
				Name:          c.Name,
				Provider:      string(c.Provider),
				URL:           c.Endpoint.URL,
				Priority:      c.Priority,
				Enabled:       c.Enabled,
				FallbackModel: c.FallbackModel,
				ModelMapping:  nonNilMap(c.ModelMapping),
			})
		}
		writeOK(w, out)
	}
}

// ---------- /api/channels/health ----------

// channelHealthInfo 对应 Rust `ChannelHealthInfo`（commands.rs:30-43）。
// 可选数值字段一律序列化成 null（Rust 侧是 Option<u64>）。
type channelHealthInfo struct {
	ID                  string  `json:"id"`
	Name                string  `json:"name"`
	State               string  `json:"state"`
	AvgFirstByteMs      *uint64 `json:"avg_first_byte_ms"`
	AvgLatencyMs        *uint64 `json:"avg_latency_ms"`
	SuccessRate         *uint64 `json:"success_rate"`
	TotalRequests       uint64  `json:"total_requests"`
	RecentFailures      uint64  `json:"recent_failures"`
	InputTokens         uint64  `json:"input_tokens"`
	OutputTokens        uint64  `json:"output_tokens"`
	CacheReadTokens     uint64  `json:"cache_read_tokens"`
	CacheCreationTokens uint64  `json:"cache_creation_tokens"`
}

// handleChannelHealth 对应 Rust `get_channel_health`（commands.rs:292）：
// 按已启用渠道（优先级升序）返回熔断状态机状态 + 该 period 窗口内的 DB 统计。
func handleChannelHealth(state *app.State) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireAdmin(w, r, state) {
			return
		}
		period := r.URL.Query().Get("period")
		if period == "" {
			period = "today"
		}
		cfg := state.Config()
		sorted := cfg.SortedChannels()

		out := make([]channelHealthInfo, 0, len(sorted))
		for _, c := range sorted {
			circuitState := "healthy"
			if h, ok := state.Health.Get(c.ID); ok {
				circuitState = h.State.String()
			}
			st := channelStatsByPeriod(state, c.ID, period)
			out = append(out, channelHealthInfo{
				ID:                  c.ID,
				Name:                c.Name,
				State:               circuitState,
				AvgFirstByteMs:      st.avgFirstByteMs,
				AvgLatencyMs:        st.avgLatencyMs,
				SuccessRate:         st.successRate,
				TotalRequests:       st.totalRequests,
				RecentFailures:      st.errorCount,
				InputTokens:         st.inputTokens,
				OutputTokens:        st.outputTokens,
				CacheReadTokens:     st.cacheReadTokens,
				CacheCreationTokens: st.cacheCreationTokens,
			})
		}
		writeOK(w, out)
	}
}

// channelStats 是 Rust `ChannelDbStats`（audit/db.rs:911 get_channel_stats_by_period）的子集。
type channelStats struct {
	totalRequests       uint64
	avgFirstByteMs      *uint64
	avgLatencyMs        *uint64
	successRate         *uint64
	errorCount          uint64
	inputTokens         uint64
	outputTokens        uint64
	cacheReadTokens     uint64
	cacheCreationTokens uint64
}

// channelStatsByPeriod 聚合某渠道在 period（today/7d/30d）窗口内的统计。
//
// Rust 用一条 SQL 完成聚合；Go 侧 audit 包未暴露等价的单渠道聚合方法，这里用
// QueryList 在 Go 里复刻同一口径（同一张表、同一批条件）：
//   - total_requests = 窗口内全部行数（COUNT(*)）；
//   - avg_first_byte_ms / avg_latency_ms = 只对 > 0 的值取平均后向下取整，
//     无样本时为 null；
//   - success_rate = 2xx 行数 / status_code 非 NULL 的行数 × 100 取整，分母为 0 时 null；
//   - recent_failures = status_code 非 NULL 且（>= 400 或 error_message 非 NULL）的行数；
//   - 各类 token 求和（NULL 视为 0）。
//
// 查询失败时与 Rust 的 `.unwrap_or_default()` 一致，返回全零（success_rate 为 null）。
func channelStatsByPeriod(state *app.State, channelID, period string) channelStats {
	var out channelStats
	items, err := state.Audit.QueryList(audit.QueryListParams{
		ChannelFilter: channelID,
		TimeFrom:      strconv.FormatInt(periodCutoffMs(period), 10),
		Limit:         ^uint32(0), // 不限条数：窗口内全量参与聚合
	})
	if err != nil {
		log.Printf("api: 渠道 %s 的 period 统计查询失败: %v", channelID, err)
		return out
	}

	var fbSum, latSum, fbN, latN, success, finished uint64
	for i := range items {
		it := &items[i]
		out.totalRequests++
		if it.FirstByteMs != nil && *it.FirstByteMs > 0 {
			fbSum += uint64(*it.FirstByteMs)
			fbN++
		}
		if it.LatencyMs != nil && *it.LatencyMs > 0 {
			latSum += uint64(*it.LatencyMs)
			latN++
		}
		if it.StatusCode != nil {
			finished++
			if *it.StatusCode >= 200 && *it.StatusCode < 300 {
				success++
			}
			if *it.StatusCode >= 400 || it.ErrorMessage != nil {
				out.errorCount++
			}
		}
		if it.InputTokens != nil {
			out.inputTokens += uint64(*it.InputTokens)
		}
		if it.OutputTokens != nil {
			out.outputTokens += uint64(*it.OutputTokens)
		}
		if it.CacheReadTokens != nil {
			out.cacheReadTokens += uint64(*it.CacheReadTokens)
		}
		if it.CacheCreationTokens != nil {
			out.cacheCreationTokens += uint64(*it.CacheCreationTokens)
		}
	}
	if fbN > 0 {
		v := fbSum / fbN
		out.avgFirstByteMs = &v
	}
	if latN > 0 {
		v := latSum / latN
		out.avgLatencyMs = &v
	}
	if finished > 0 {
		v := uint64(float64(success) / float64(finished) * 100.0)
		out.successRate = &v
	}
	return out
}

// periodCutoffMs 复刻 Rust `get_channel_stats_by_period` 的窗口起点：
// 按本地时区，today = 今天 0 点，7d = 7 天前 0 点，30d = 30 天前 0 点。
func periodCutoffMs(period string) int64 {
	now := time.Now()
	daysBack := 0
	switch period {
	case "7d":
		daysBack = 7
	case "30d":
		daysBack = 30
	}
	target := now.AddDate(0, 0, -daysBack)
	midnight := time.Date(target.Year(), target.Month(), target.Day(), 0, 0, 0, 0, time.Local)
	return midnight.UnixMilli()
}

// ---------- /api/config ----------

// channelEditData 对应 Rust `ChannelEditData`（commands.rs:47-71）：含 API Key 的渠道编辑数据。
//
// 注意字段与 Rust 逐字对应：可选字段（force_effort）序列化成 null 而不是省略；
// 不含 rate_limit（Rust 的编辑视图不暴露它）。
type channelEditData struct {
	ID            string            `json:"id"`
	Name          string            `json:"name"`
	Provider      string            `json:"provider"`
	URL           string            `json:"url"`
	APIKey        string            `json:"api_key"`
	Priority      uint32            `json:"priority"`
	Enabled       bool              `json:"enabled"`
	FallbackModel string            `json:"fallback_model"`
	ModelMapping  map[string]string `json:"model_mapping"`
	TimeoutMs     uint64            `json:"timeout_ms"`
	CustomHeaders map[string]string `json:"custom_headers"`
	StripThinking bool              `json:"strip_thinking"`
	RetryCount    uint32            `json:"retry_count"`
	RetryDelayMs  uint64            `json:"retry_delay_ms"`
	ForceEffort   *string           `json:"force_effort"`
	AutoCache     bool              `json:"auto_cache"`
}

// failoverEditData 对应 Rust `FailoverEditData`（commands.rs:76-86）。
type failoverEditData struct {
	MaxFailoverChannels uint32 `json:"max_failover_channels"`
	RetryTimeoutMs      uint64 `json:"retry_timeout_ms"`
	FailureThreshold    uint32 `json:"failure_threshold"`
	RecoveryIntervalSec uint64 `json:"recovery_interval_sec"`
	ProbeRequests       uint32 `json:"probe_requests"`
}

// authEditData 对应 Rust `AuthEditData`（commands.rs:126-130）。
type authEditData struct {
	ProxyTokens []string `json:"proxy_tokens"`
	AdminToken  *string  `json:"admin_token"`
}

// mcpServerEditData 对应 Rust `McpServerEditData`（commands.rs:103-121）。
type mcpServerEditData struct {
	ID            string            `json:"id"`
	Name          string            `json:"name"`
	Endpoint      string            `json:"endpoint"`
	AuthToken     *string           `json:"auth_token"`
	CustomHeaders map[string]string `json:"custom_headers"`
	BlockedTools  []string          `json:"blocked_tools"`
	Enabled       bool              `json:"enabled"`
	OAuthEnabled  bool              `json:"oauth_enabled"`
	CachedTools   []config.ToolInfo `json:"cached_tools"`
}

// configViewData 对应 Rust `ConfigViewData`（commands.rs:88-101）。
type configViewData struct {
	ListenPort              uint16              `json:"listen_port"`
	ListenHost              string              `json:"listen_host"`
	PublicURL               *string             `json:"public_url"`
	Models                  []string            `json:"models"`
	Channels                []channelEditData   `json:"channels"`
	Failover                failoverEditData    `json:"failover"`
	Auth                    authEditData        `json:"auth"`
	UltimateFallbackChannel *string             `json:"ultimate_fallback_channel"`
	McpServers              []mcpServerEditData `json:"mcp_servers"`
	AuditRetentionDays      uint32              `json:"audit_retention_days"`
}

// handleFullConfig 对应 Rust `get_full_config`（commands.rs:332）：
// 完整配置（含渠道 API Key、MCP server、模型价格以外的全部可编辑字段）。
func handleFullConfig(state *app.State) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireAdmin(w, r, state) {
			return
		}
		cfg := state.Config()

		data := configViewData{
			ListenPort: cfg.ListenPort,
			ListenHost: cfg.ListenHost,
			PublicURL:  cfg.PublicURL,
			Models:     nonNilSlice(cfg.Models),
			Channels:   make([]channelEditData, 0, len(cfg.Channels)),
			Failover: failoverEditData{
				MaxFailoverChannels: cfg.Failover.MaxFailoverChannels,
				RetryTimeoutMs:      cfg.Failover.RetryTimeoutMs,
				FailureThreshold:    cfg.Failover.CircuitBreaker.FailureThreshold,
				RecoveryIntervalSec: cfg.Failover.CircuitBreaker.RecoveryIntervalSec,
				ProbeRequests:       cfg.Failover.CircuitBreaker.ProbeRequests,
			},
			Auth: authEditData{
				ProxyTokens: nonNilSlice(cfg.Auth.ProxyTokens),
				AdminToken:  cfg.Auth.AdminToken,
			},
			McpServers:         make([]mcpServerEditData, 0, len(cfg.McpServers)),
			AuditRetentionDays: retentionDays(cfg),
		}
		if cfg.UltimateFallback != nil {
			ch := cfg.UltimateFallback.Channel
			data.UltimateFallbackChannel = &ch
		}
		for i := range cfg.Channels {
			c := &cfg.Channels[i]
			data.Channels = append(data.Channels, channelEditData{
				ID:            c.ID,
				Name:          c.Name,
				Provider:      string(c.Provider),
				URL:           c.Endpoint.URL,
				APIKey:        c.APIKey,
				Priority:      c.Priority,
				Enabled:       c.Enabled,
				FallbackModel: c.FallbackModel,
				ModelMapping:  nonNilMap(c.ModelMapping),
				TimeoutMs:     c.TimeoutMs,
				CustomHeaders: nonNilMap(c.CustomHeaders),
				StripThinking: c.StripThinking,
				RetryCount:    c.RetryCount,
				RetryDelayMs:  c.RetryDelayMs,
				ForceEffort:   c.ForceEffort,
				AutoCache:     c.AutoCache,
			})
		}
		for i := range cfg.McpServers {
			s := &cfg.McpServers[i]
			data.McpServers = append(data.McpServers, mcpServerEditData{
				ID:            s.ID,
				Name:          s.Name,
				Endpoint:      s.Endpoint,
				AuthToken:     s.AuthToken,
				CustomHeaders: nonNilMap(s.CustomHeaders),
				BlockedTools:  nonNilSlice(s.BlockedTools),
				Enabled:       s.Enabled,
				OAuthEnabled:  s.OAuthEnabled,
				CachedTools:   nonNilTools(s.CachedTools),
			})
		}
		writeOK(w, data)
	}
}

// nonNilTools 保证 cached_tools 序列化成 `[]`（Rust 侧 Vec<ToolInfo>）。
func nonNilTools(v []config.ToolInfo) []config.ToolInfo {
	if v == nil {
		return []config.ToolInfo{}
	}
	return v
}

// retentionDays 对应 Rust `config.audit_retention_days.unwrap_or(0)`。
func retentionDays(cfg *config.AppConfig) uint32 {
	if cfg.AuditRetentionDays == nil {
		return 0
	}
	return *cfg.AuditRetentionDays
}

// ---------- /api/config/mcp/oauth/status ----------

// handleMCPOAuthStatus 对应 Rust `get_mcp_oauth_status`（commands.rs:640）：
// `?serverId=<id>` → `{authorized, expiresAt, needsReauth}`。
func handleMCPOAuthStatus(state *app.State) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireAdmin(w, r, state) {
			return
		}
		serverID := r.URL.Query().Get("serverId")
		info := MCPSession(state).OAuthStatus(serverID)
		writeOK(w, info)
	}
}

// ---------- /api/model-prices（GET） ----------

// modelPriceView 对应 Rust `ModelPrice`（channel/config.rs:42-59）。
//
// 单独定义而不直接用 `config.ModelPrice`，是因为后者的 cache_* 字段带 omitempty，
// 会把 `null` 变成「字段缺失」，与 Rust 的序列化结果不一致。
type modelPriceView struct {
	Model             string   `json:"model"`
	InputPerMtok      float64  `json:"input_per_mtok"`
	OutputPerMtok     float64  `json:"output_per_mtok"`
	CacheReadPerMtok  *float64 `json:"cache_read_per_mtok"`
	CacheWritePerMtok *float64 `json:"cache_write_per_mtok"`
	Enabled           bool     `json:"enabled"`
}

// handleModelPrices 对应 Rust `get_model_prices`（commands.rs:828）：全部模型定价。
func handleModelPrices(state *app.State) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireAdmin(w, r, state) {
			return
		}
		prices := state.Config().ModelPrices
		out := make([]modelPriceView, 0, len(prices))
		for _, p := range prices {
			out = append(out, modelPriceView{
				Model:             p.Model,
				InputPerMtok:      p.InputPerMtok,
				OutputPerMtok:     p.OutputPerMtok,
				CacheReadPerMtok:  p.CacheReadPerMtok,
				CacheWritePerMtok: p.CacheWritePerMtok,
				Enabled:           p.Enabled,
			})
		}
		writeOK(w, out)
	}
}
