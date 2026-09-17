// POST 类管理端点，逐个对应 Rust `src-tauri/src/commands.rs` 的同名函数。
//
// 所有写接口都通过 `app.State.Update` 落盘（等价于 Rust 的 save_config_to_disk），
// 保证改动写进 `~/.model-bridge/config.yaml`。
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"modelbridge/internal/app"
	"modelbridge/internal/audit"
	"modelbridge/internal/auth"
	"modelbridge/internal/config"
	"modelbridge/internal/mcp"
)

// ---------- /api/audit/cleanup ----------

// handleForceCleanupAudit 对应 Rust `force_cleanup_audit`（commands.rs:232）：
// 删除超期记录（保留天数取配置，0 = 永久留存则跳过）+ 详情只保留最近 1000 条 + VACUUM。
func handleForceCleanupAudit(state *app.State) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !auth.RequireAdmin(w, r, state) {
			return
		}
		retention := retentionDays(state.Config())
		result, err := state.Audit.ForceCleanup(retention, audit.BODIESRetainLatest)
		if err != nil {
			log.Printf("api: force_cleanup_audit 失败: %v", err)
			writeErr(w, err)
			return
		}
		writeOK(w, result)
	}
}

// ---------- /api/config/channel ----------

// channelSaveReq 对应 Rust `ChannelEditData` 的反序列化形态（commands.rs:47-71）。
//
// 与 Rust 的 serde 默认值保持一致：retry_delay_ms 缺省 500、auto_cache 缺省 true、
// custom_headers 缺省 {}、strip_thinking/retry_count 缺省 false/0、force_effort 缺省 null。
type channelSaveReq struct {
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
	RetryDelayMs  *uint64           `json:"retry_delay_ms"`
	ForceEffort   *string           `json:"force_effort"`
	AutoCache     *bool             `json:"auto_cache"`
}

// parseProvider 对应 Rust `parse_provider`（commands.rs:908）。
func parseProvider(s string) (config.ProviderType, error) {
	switch strings.ToLower(s) {
	case "openai":
		return config.ProviderOpenAI, nil
	case "openai_responses", "openairesponses":
		return config.ProviderOpenAIResponses, nil
	case "anthropic":
		return config.ProviderAnthropic, nil
	default:
		return "", fmt.Errorf("Unknown provider: %s", s)
	}
}

// handleSaveChannel 对应 Rust `save_channel`（commands.rs:389）：
// 按 id upsert（存在则整体替换），并为该渠道补建健康记录；失败返回 {"error": …}。
func handleSaveChannel(state *app.State) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !auth.RequireAdmin(w, r, state) {
			return
		}
		var req channelSaveReq
		if !decodeBody(w, r, &req) {
			return
		}
		provider, err := parseProvider(req.Provider)
		if err != nil {
			writeErr(w, err)
			return
		}
		retryDelay := config.DefaultRetryDelayMs
		if req.RetryDelayMs != nil {
			retryDelay = *req.RetryDelayMs
		}
		autoCache := true
		if req.AutoCache != nil {
			autoCache = *req.AutoCache
		}

		newChannel := config.ChannelConfig{
			ID:            req.ID,
			Name:          req.Name,
			Provider:      provider,
			Endpoint:      config.EndpointConfig{URL: req.URL},
			APIKey:        req.APIKey,
			Priority:      req.Priority,
			Enabled:       req.Enabled,
			FallbackModel: req.FallbackModel,
			ModelMapping:  nonNilMap(req.ModelMapping),
			TimeoutMs:     req.TimeoutMs,
			CustomHeaders: nonNilMap(req.CustomHeaders),
			StripThinking: req.StripThinking,
			// Rust 保存时把 rate_limit 置为 None（编辑视图不暴露该字段）。
			RateLimit:    nil,
			RetryCount:   req.RetryCount,
			RetryDelayMs: retryDelay,
			ForceEffort:  filterEmpty(req.ForceEffort),
			AutoCache:    autoCache,
		}

		if err := state.Update(func(cfg *config.AppConfig) error {
			for i := range cfg.Channels {
				if cfg.Channels[i].ID == newChannel.ID {
					cfg.Channels[i] = newChannel
					return nil
				}
			}
			cfg.Channels = append(cfg.Channels, newChannel)
			return nil
		}); err != nil {
			log.Printf("api: save_channel 落盘失败: %v", err)
			writeErr(w, err)
			return
		}
		// Rust: health.entry(id).or_insert_with(ChannelHealth::new)
		state.Health.Ensure(newChannel.ID)
		writeSuccess(w)
	}
}

// ---------- /api/config/channel/delete ----------

// deleteChannelReq 对应 Rust `DeleteChannelReq`（commands.rs:442，rename_all = "camelCase"）。
type deleteChannelReq struct {
	ChannelID string `json:"channelId"`
}

// handleDeleteChannel 对应 Rust `delete_channel`（commands.rs:448）：
// 按 id 删除渠道，并移除该渠道的健康记录，然后落盘。
func handleDeleteChannel(state *app.State) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !auth.RequireAdmin(w, r, state) {
			return
		}
		var req deleteChannelReq
		if !decodeBody(w, r, &req) {
			return
		}
		if err := state.Update(func(cfg *config.AppConfig) error {
			out := cfg.Channels[:0]
			for i := range cfg.Channels {
				if cfg.Channels[i].ID != req.ChannelID {
					out = append(out, cfg.Channels[i])
				}
			}
			cfg.Channels = out
			return nil
		}); err != nil {
			log.Printf("api: delete_channel 落盘失败: %v", err)
			writeErr(w, err)
			return
		}
		state.Health.Remove(req.ChannelID)
		writeSuccess(w)
	}
}

// ---------- /api/config/failover ----------

// failoverSaveReq 对应 Rust `FailoverEditData`（commands.rs:76-86）。
// `max_retries` 是 `max_failover_channels` 的兼容别名（serde alias）。
type failoverSaveReq struct {
	MaxFailoverChannels *uint32 `json:"max_failover_channels"`
	MaxRetriesAlias     *uint32 `json:"max_retries"`
	RetryTimeoutMs      *uint64 `json:"retry_timeout_ms"`
	FailureThreshold    *uint32 `json:"failure_threshold"`
	RecoveryIntervalSec *uint64 `json:"recovery_interval_sec"`
	ProbeRequests       *uint32 `json:"probe_requests"`
}

// handleSaveFailover 对应 Rust `save_failover_config`（commands.rs:653）。
func handleSaveFailover(state *app.State) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !auth.RequireAdmin(w, r, state) {
			return
		}
		var req failoverSaveReq
		if !decodeBody(w, r, &req) {
			return
		}
		// Rust 的字段没有 serde 默认值，缺字段会被反序列化拒绝；这里同样拒绝（400）。
		maxFailover := req.MaxFailoverChannels
		if maxFailover == nil {
			maxFailover = req.MaxRetriesAlias
		}
		switch {
		case maxFailover == nil:
			writeBadRequest(w, "missing field `max_failover_channels`")
			return
		case req.RetryTimeoutMs == nil:
			writeBadRequest(w, "missing field `retry_timeout_ms`")
			return
		case req.FailureThreshold == nil:
			writeBadRequest(w, "missing field `failure_threshold`")
			return
		case req.RecoveryIntervalSec == nil:
			writeBadRequest(w, "missing field `recovery_interval_sec`")
			return
		case req.ProbeRequests == nil:
			writeBadRequest(w, "missing field `probe_requests`")
			return
		}

		if err := state.Update(func(cfg *config.AppConfig) error {
			cfg.Failover.MaxFailoverChannels = *maxFailover
			cfg.Failover.RetryTimeoutMs = *req.RetryTimeoutMs
			cfg.Failover.CircuitBreaker.FailureThreshold = *req.FailureThreshold
			cfg.Failover.CircuitBreaker.RecoveryIntervalSec = *req.RecoveryIntervalSec
			cfg.Failover.CircuitBreaker.ProbeRequests = *req.ProbeRequests
			return nil
		}); err != nil {
			log.Printf("api: save_failover_config 落盘失败: %v", err)
			writeErr(w, err)
			return
		}
		writeSuccess(w)
	}
}

// writeBadRequest 写出 400 + `{"error": "<指定原因>"}`，用于缺少必填字段等参数错误。
func writeBadRequest(w http.ResponseWriter, reason string) {
	writeAPIError(w, http.StatusBadRequest, "Bad request: "+reason)
}

// ---------- /api/config/settings ----------

// settingsSaveReq 对应 Rust `SystemSettings`（commands.rs:676-691）。
//
// listen_port / listen_host / models 无 serde 默认值（必填）；
// public_url / auth / ultimate_fallback_channel / audit_retention_days 可缺省。
type settingsSaveReq struct {
	ListenPort              *uint16       `json:"listen_port"`
	ListenHost              *string       `json:"listen_host"`
	PublicURL               *string       `json:"public_url"`
	Models                  *[]string     `json:"models"`
	Auth                    *authEditData `json:"auth"`
	UltimateFallbackChannel *string       `json:"ultimate_fallback_channel"`
	AuditRetentionDays      *uint32       `json:"audit_retention_days"`
}

// handleSaveSettings 对应 Rust `save_settings`（commands.rs:694）：
// 端口 / host / public_url / 模型列表 / 鉴权 / 最终兜底渠道 / 审计保留天数。
func handleSaveSettings(state *app.State) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !auth.RequireAdmin(w, r, state) {
			return
		}
		var req settingsSaveReq
		if !decodeBody(w, r, &req) {
			return
		}
		switch {
		case req.ListenPort == nil:
			writeBadRequest(w, "missing field `listen_port`")
			return
		case req.ListenHost == nil:
			writeBadRequest(w, "missing field `listen_host`")
			return
		case req.Models == nil:
			writeBadRequest(w, "missing field `models`")
			return
		}

		if err := state.Update(func(cfg *config.AppConfig) error {
			cfg.ListenPort = *req.ListenPort
			cfg.ListenHost = *req.ListenHost
			// Rust: public_url.filter(|s| !s.is_empty())
			cfg.PublicURL = filterEmpty(req.PublicURL)
			cfg.Models = nonNilSlice(*req.Models)
			if req.Auth != nil {
				// Rust 这里不做空串过滤，原样写入（admin_token 为空串时视为免鉴权）。
				cfg.Auth = config.AuthConfig{
					ProxyTokens: nonNilSlice(req.Auth.ProxyTokens),
					AdminToken:  req.Auth.AdminToken,
				}
			}
			// Rust: ultimate_fallback_channel.filter(|s| !s.is_empty()).map(...)
			if ch := filterEmpty(req.UltimateFallbackChannel); ch != nil {
				cfg.UltimateFallback = &config.UltimateFallback{Channel: *ch}
			} else {
				cfg.UltimateFallback = nil
			}
			cfg.AuditRetentionDays = req.AuditRetentionDays
			return nil
		}); err != nil {
			log.Printf("api: save_settings 落盘失败: %v", err)
			writeErr(w, err)
			return
		}
		writeSuccess(w)
	}
}

// ---------- /api/config/channel/test ----------

// testChannelReq 对应 Rust `TestChannelReq`（commands.rs:725，rename_all = "camelCase"）。
type testChannelReq struct {
	ChannelID string `json:"channelId"`
}

// testChannelOK 对应 Rust `test_channel` 的成功分支响应。
type testChannelOK struct {
	Success      bool   `json:"success"`
	StatusCode   uint16 `json:"status_code"`
	LatencyMs    uint64 `json:"latency_ms"`
	ResponseBody string `json:"response_body"`
}

// testChannelFail 对应 Rust `test_channel` 的传输层失败分支响应。
type testChannelFail struct {
	Success   bool   `json:"success"`
	Error     string `json:"error"`
	LatencyMs uint64 `json:"latency_ms"`
}

// testChannelTimeout 单次上游探测的超时，与 Rust `.timeout(Duration::from_secs(15))` 一致。
const testChannelTimeout = 15 * time.Second

// handleTestChannel 对应 Rust `test_channel`（commands.rs:731）：
// 按 channel_id 从配置里取渠道（不看 enabled），用该渠道协议对应的报文与认证头
// 真发一次请求，返回 `{success, status_code, latency_ms, response_body}`。
func handleTestChannel(state *app.State) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !auth.RequireAdmin(w, r, state) {
			return
		}
		raw, ok := readBody(w, r)
		if !ok {
			return
		}
		var req testChannelReq
		if err := json.Unmarshal(raw, &req); err != nil {
			// Rust: body: String → serde_json::from_str 失败同样返回该形状。
			writeJSON(w, http.StatusOK, map[string]any{
				"success": false,
				"error":   "Bad request: " + err.Error(),
			})
			return
		}

		channel := state.Config().FindChannel(req.ChannelID)
		if channel == nil {
			writeJSON(w, http.StatusOK, map[string]any{
				"success": false,
				"error":   "Channel not found",
			})
			return
		}

		// 按渠道类型构造测试报文与认证头，与真实转发请求保持一致。
		payload := map[string]any{
			"model":      channel.FallbackModel,
			"max_tokens": 16,
			"messages":   []map[string]string{{"role": "user", "content": "ping"}},
		}
		headers := http.Header{}
		headers.Set("Content-Type", "application/json")
		switch channel.Provider {
		case config.ProviderAnthropic:
			headers.Set("x-api-key", channel.APIKey)
			headers.Set("anthropic-version", "2023-06-01")
		case config.ProviderOpenAIResponses:
			// Responses API 用必填的 input 字段（不是 Chat Completions 的 messages）。
			payload = map[string]any{"model": channel.FallbackModel, "input": "ping"}
			if channel.APIKey != "" {
				headers.Set("Authorization", "Bearer "+channel.APIKey)
			}
		default:
			if channel.APIKey != "" {
				headers.Set("Authorization", "Bearer "+channel.APIKey)
			}
		}
		// 叠加渠道自定义请求头（与真实转发一致，可覆盖上面的默认头）。
		for k, v := range channel.CustomHeaders {
			headers.Set(k, v)
		}

		bodyBytes, err := json.Marshal(payload)
		if err != nil {
			log.Printf("api: test_channel 构造请求体失败: %v", err)
			writeJSON(w, http.StatusOK, testChannelFail{Success: false, Error: err.Error()})
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), testChannelTimeout)
		defer cancel()
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, channel.Endpoint.URL, bytes.NewReader(bodyBytes))
		if err != nil {
			writeJSON(w, http.StatusOK, testChannelFail{Success: false, Error: err.Error()})
			return
		}
		httpReq.Header = headers

		start := time.Now()
		resp, err := state.HTTP.Do(httpReq)
		latency := uint64(time.Since(start).Milliseconds()) // Rust 在 send() 返回后立即取耗时
		if err != nil {
			writeJSON(w, http.StatusOK, testChannelFail{Success: false, Error: err.Error(), LatencyMs: latency})
			return
		}
		defer func() { _ = resp.Body.Close() }()
		respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
		if readErr != nil {
			log.Printf("api: test_channel 读取上游响应失败: %v", readErr)
		}
		writeJSON(w, http.StatusOK, testChannelOK{
			Success:      resp.StatusCode < 400,
			StatusCode:   uint16(resp.StatusCode),
			LatencyMs:    latency,
			ResponseBody: string(respBody),
		})
	}
}

// ---------- /api/config/mcp ----------

// mcpSaveReq 对应 Rust `McpServerEditData` 的反序列化形态（commands.rs:103-121）。
// enabled 缺省 true（serde default = "default_true_edit"）。
type mcpSaveReq struct {
	ID            string            `json:"id"`
	Name          string            `json:"name"`
	Endpoint      string            `json:"endpoint"`
	AuthToken     *string           `json:"auth_token"`
	CustomHeaders map[string]string `json:"custom_headers"`
	BlockedTools  []string          `json:"blocked_tools"`
	Enabled       *bool             `json:"enabled"`
	OAuthEnabled  bool              `json:"oauth_enabled"`
}

// handleSaveMCPServer 对应 Rust `save_mcp_server`（commands.rs:472）：
// 按 id upsert；更新时保留已有 oauth 授权数据，新增时 oauth = None、cached_tools = []。
func handleSaveMCPServer(state *app.State) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !auth.RequireAdmin(w, r, state) {
			return
		}
		var req mcpSaveReq
		if !decodeBody(w, r, &req) {
			return
		}
		enabled := true
		if req.Enabled != nil {
			enabled = *req.Enabled
		}

		if err := state.Update(func(cfg *config.AppConfig) error {
			for i := range cfg.McpServers {
				if cfg.McpServers[i].ID != req.ID {
					continue
				}
				s := &cfg.McpServers[i]
				s.Name = req.Name
				s.Endpoint = req.Endpoint
				s.AuthToken = filterEmpty(req.AuthToken)
				s.CustomHeaders = nonNilMap(req.CustomHeaders)
				s.BlockedTools = nonNilSlice(req.BlockedTools)
				s.Enabled = enabled
				s.OAuthEnabled = req.OAuthEnabled
				// existing.oauth 保持不变；cached_tools 也保留（由 tools/fetch 更新）
				return nil
			}
			cfg.McpServers = append(cfg.McpServers, config.McpServerConfig{
				ID:            req.ID,
				Name:          req.Name,
				Endpoint:      req.Endpoint,
				AuthToken:     filterEmpty(req.AuthToken),
				CustomHeaders: nonNilMap(req.CustomHeaders),
				BlockedTools:  nonNilSlice(req.BlockedTools),
				Enabled:       enabled,
				OAuthEnabled:  req.OAuthEnabled,
				OAuth:         nil,
				CachedTools:   []config.ToolInfo{},
			})
			return nil
		}); err != nil {
			log.Printf("api: save_mcp_server 落盘失败: %v", err)
			writeErr(w, err)
			return
		}
		writeSuccess(w)
	}
}

// deleteMCPServerReq 对应 Rust `DeleteMcpServerReq`（commands.rs:515）。
type deleteMCPServerReq struct {
	ServerID string `json:"serverId"`
}

// handleDeleteMCPServer 对应 Rust `delete_mcp_server`（commands.rs:519）。
func handleDeleteMCPServer(state *app.State) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !auth.RequireAdmin(w, r, state) {
			return
		}
		var req deleteMCPServerReq
		if !decodeBody(w, r, &req) {
			return
		}
		if err := state.Update(func(cfg *config.AppConfig) error {
			out := cfg.McpServers[:0]
			for i := range cfg.McpServers {
				if cfg.McpServers[i].ID != req.ServerID {
					out = append(out, cfg.McpServers[i])
				}
			}
			cfg.McpServers = out
			return nil
		}); err != nil {
			log.Printf("api: delete_mcp_server 落盘失败: %v", err)
			writeErr(w, err)
			return
		}
		writeSuccess(w)
	}
}

// ---------- /api/config/mcp/tools/fetch ----------

// mcpServerIDReq 对应 Rust `FetchToolsReq` / `StartOauthReq`（都是 `{server_id}` + camelCase）。
type mcpServerIDReq struct {
	ServerID string `json:"serverId"`
}

// handleFetchMCPTools 对应 Rust `fetch_mcp_tools`（commands.rs:546）：
// 按 id 直接查（不带 enabled 过滤）→ 握手拉取工具并写入 cached_tools → 落盘。
func handleFetchMCPTools(state *app.State, mcpState *mcp.State) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !auth.RequireAdmin(w, r, state) {
			return
		}
		var req mcpServerIDReq
		if !decodeBody(w, r, &req) {
			return
		}
		var server *config.McpServerConfig
		cfg := state.Config()
		for i := range cfg.McpServers {
			if cfg.McpServers[i].ID == req.ServerID {
				cp := cfg.McpServers[i]
				server = &cp
				break
			}
		}
		if server == nil {
			writeAPIError(w, http.StatusOK, "MCP server 不存在")
			return
		}

		tools, err := mcp.FetchTools(r.Context(), mcpState, server)
		if err != nil {
			var fetchErr *mcp.FetchToolsError
			if errors.As(err, &fetchErr) && fetchErr.Kind == mcp.FetchToolsNeedsAuth {
				writeJSON(w, http.StatusOK, struct {
					Error     string `json:"error"`
					NeedsAuth bool   `json:"needsAuth"`
				}{Error: fetchErr.Message, NeedsAuth: true})
				return
			}
			log.Printf("api: fetch_mcp_tools 失败: %v", err)
			writeAPIError(w, http.StatusOK, err.Error())
			return
		}

		if err := state.Update(func(c *config.AppConfig) error {
			for i := range c.McpServers {
				if c.McpServers[i].ID == req.ServerID {
					c.McpServers[i].CachedTools = tools
				}
			}
			return nil
		}); err != nil {
			log.Printf("api: fetch_mcp_tools 落盘失败: %v", err)
			writeErr(w, err)
			return
		}
		writeOK(w, struct {
			Success bool              `json:"success"`
			Tools   []config.ToolInfo `json:"tools"`
		}{Success: true, Tools: nonNilTools(tools)})
	}
}

// ---------- /api/config/mcp/tools/toggle ----------

// toggleToolReq 对应 Rust `ToggleToolReq`（commands.rs:578-584）。
type toggleToolReq struct {
	ServerID string `json:"serverId"`
	Tool     string `json:"tool"`
	Enabled  bool   `json:"enabled"`
}

// handleToggleMCPTool 对应 Rust `toggle_mcp_tool`（commands.rs:589）：
// 启用 = 从 blocked_tools 移除；禁用 = 加入 blocked_tools（去重）。
func handleToggleMCPTool(state *app.State) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !auth.RequireAdmin(w, r, state) {
			return
		}
		var req toggleToolReq
		if !decodeBody(w, r, &req) {
			return
		}
		found := false
		if err := state.Update(func(cfg *config.AppConfig) error {
			for i := range cfg.McpServers {
				if cfg.McpServers[i].ID != req.ServerID {
					continue
				}
				found = true
				s := &cfg.McpServers[i]
				if req.Enabled {
					out := s.BlockedTools[:0]
					for _, t := range s.BlockedTools {
						if t != req.Tool {
							out = append(out, t)
						}
					}
					s.BlockedTools = out
				} else if !containsString(s.BlockedTools, req.Tool) {
					s.BlockedTools = append(s.BlockedTools, req.Tool)
				}
				return nil
			}
			return nil
		}); err != nil {
			log.Printf("api: toggle_mcp_tool 落盘失败: %v", err)
			writeErr(w, err)
			return
		}
		if !found {
			writeAPIError(w, http.StatusOK, "MCP server 不存在")
			return
		}
		writeSuccess(w)
	}
}

// containsString 判断切片是否包含某字符串。
func containsString(list []string, v string) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}

// ---------- /api/config/mcp/oauth/start ----------

// handleStartMCPOAuth 对应 Rust `start_mcp_oauth`（commands.rs:621）：
// 触发 OAuth 2.1 授权流程，返回 `{authorizeUrl}` 供前端打开。
func handleStartMCPOAuth(state *app.State, mcpState *mcp.State) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !auth.RequireAdmin(w, r, state) {
			return
		}
		var req mcpServerIDReq
		if !decodeBody(w, r, &req) {
			return
		}
		url, err := mcpState.StartOAuthFlow(r.Context(), req.ServerID)
		if err != nil {
			log.Printf("api: start_mcp_oauth 失败: %v", err)
			writeAPIError(w, http.StatusOK, err.Error())
			return
		}
		writeOK(w, struct {
			AuthorizeURL string `json:"authorizeUrl"`
		}{AuthorizeURL: url})
	}
}

// ---------- /api/model-prices（POST） ----------

// modelPriceSaveReq 对应 Rust `ModelPriceReq`（commands.rs:835-850）：
// enabled 缺省 true，input/output 缺省 0，cache_* 缺省 null。
type modelPriceSaveReq struct {
	Model             string   `json:"model"`
	InputPerMtok      float64  `json:"input_per_mtok"`
	OutputPerMtok     float64  `json:"output_per_mtok"`
	CacheReadPerMtok  *float64 `json:"cache_read_per_mtok"`
	CacheWritePerMtok *float64 `json:"cache_write_per_mtok"`
	Enabled           *bool    `json:"enabled"`
}

// handleSaveModelPrice 对应 Rust `save_model_price`（commands.rs:853）：
// 模型名去空格后非空校验 + 按 model upsert。
func handleSaveModelPrice(state *app.State) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !auth.RequireAdmin(w, r, state) {
			return
		}
		var req modelPriceSaveReq
		if !decodeBody(w, r, &req) {
			return
		}
		if strings.TrimSpace(req.Model) == "" {
			writeAPIError(w, http.StatusOK, "模型名不能为空")
			return
		}
		enabled := true
		if req.Enabled != nil {
			enabled = *req.Enabled
		}
		price := config.ModelPrice{
			Model:             strings.TrimSpace(req.Model),
			InputPerMtok:      req.InputPerMtok,
			OutputPerMtok:     req.OutputPerMtok,
			CacheReadPerMtok:  req.CacheReadPerMtok,
			CacheWritePerMtok: req.CacheWritePerMtok,
			Enabled:           enabled,
		}
		if err := state.Update(func(cfg *config.AppConfig) error {
			for i := range cfg.ModelPrices {
				if cfg.ModelPrices[i].Model == price.Model {
					cfg.ModelPrices[i] = price
					return nil
				}
			}
			cfg.ModelPrices = append(cfg.ModelPrices, price)
			return nil
		}); err != nil {
			log.Printf("api: save_model_price 落盘失败: %v", err)
			writeErr(w, err)
			return
		}
		writeSuccess(w)
	}
}

// ---------- /api/model-prices/delete ----------

// deleteModelPriceReq 对应 Rust `DeleteModelPriceReq`（commands.rs:883，rename_all 未涉及单字段名）。
type deleteModelPriceReq struct {
	Model string `json:"model"`
}

// handleDeleteModelPrice 对应 Rust `delete_model_price`（commands.rs:892）。
func handleDeleteModelPrice(state *app.State) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !auth.RequireAdmin(w, r, state) {
			return
		}
		var req deleteModelPriceReq
		if !decodeBody(w, r, &req) {
			return
		}
		if err := state.Update(func(cfg *config.AppConfig) error {
			out := cfg.ModelPrices[:0]
			for i := range cfg.ModelPrices {
				if cfg.ModelPrices[i].Model != req.Model {
					out = append(out, cfg.ModelPrices[i])
				}
			}
			cfg.ModelPrices = out
			return nil
		}); err != nil {
			log.Printf("api: delete_model_price 落盘失败: %v", err)
			writeErr(w, err)
			return
		}
		writeSuccess(w)
	}
}
