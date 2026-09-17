// MCP 上游 OAuth 2.1 支持（对应 src-tauri/src/mcp/oauth.rs）：中继作为 OAuth client
// 完成完整授权流程。
//
// 流程：发现(401→PRM→AS metadata) → DCR 动态注册 → PKCE 授权 → 回调换 token
// → 转发时注入 Bearer，过期自动刷新。
//
// token 持久化在 config.McpServerConfig.OAuth（随 config.yaml 落盘，通过 ConfigStore.Update）；
// 仅 PKCE/state 的 pending 表与 per-server 刷新锁放内存（进程重启即失效，与 Rust 一致）。
//
// # 可达范围与未实现说明
//
// 已实现并且可被自动化验证：
//   - 发现链：探测上游 401 → WWW-Authenticate 的 resource_metadata → RFC9728 PRM
//     → RFC8414/OIDC AS metadata（回退顺序与 Rust 一致）
//   - DCR（RFC7591）动态客户端注册
//   - PKCE(S256) + state 生成、authorize URL 构造（返回给调用方/前端打开）
//   - 回调换 token（authorization_code）、refresh_token 刷新、per-server 刷新锁
//   - token 快照持久化进 config、[State.OAuthStatus]（`GET /api/config/mcp/oauth/status`）
//   - [State.HandleOAuthCallback]：`GET /oauth/callback` 的 handler（含结果 HTML 页）
//
// 沙箱内无法验证的部分（不影响上述语义，仅涉及「人」的那一步）：
//   - 真实浏览器打开 authorize URL、用户在 AS 页面授权并跳回 loopback 回调。
//     Go 侧不自动打开浏览器（Rust 也不打开：URL 由 `POST /api/config/mcp/oauth/start`
//     返回给前端），因此这里同样只返回 URL，由调用方决定如何展示。
//   - 结果页 HTML 与 Rust 完全同构，但样式细节未与浏览器逐像素核对。
package mcp

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"modelbridge/internal/config"
)

const (
	// expirySkewSecs access token 提前刷新的余量（秒）（oauth.rs:26）。
	expirySkewSecs int64 = 60
	// pendingTTLSecs pending 授权请求的存活时间（秒），超时作废（oauth.rs:28）。
	pendingTTLSecs int64 = 600
	// TokenRefreshIntervalSecs 后台刷新间隔（oauth.rs:32 TOKEN_REFRESH_INTERVAL_SECS）。
	// 故意远小于常见 access token 生命周期，同时网络开销可忽略。
	TokenRefreshIntervalSecs uint64 = 300
)

// PendingAuth 一次进行中的授权请求（start 创建，callback 消费）（oauth.rs:34-48）。
type PendingAuth struct {
	ServerID      string
	CodeVerifier  string
	RedirectURI   string
	Resource      string
	TokenEndpoint string
	ClientID      string
	ClientSecret  *string
	Scope         *string
	CreatedAt     int64
}

// OAuthStore OAuth 运行时存储（oauth.rs:50-57）。
type OAuthStore struct {
	mu sync.Mutex
	// pending state → 进行中的授权请求（按 state 隔离，天然支持多 server 并发授权）
	pending map[string]*PendingAuth
	// refreshLocks server_id → 刷新锁（防并发双刷新使 AS 轮换的 refresh_token 失效）
	refreshLocks map[string]*sync.Mutex
}

// NewOAuthStore 构造空 store（token 在 config 里，无需预热）（oauth.rs:62-64）。
func NewOAuthStore() *OAuthStore {
	return &OAuthStore{
		pending:      map[string]*PendingAuth{},
		refreshLocks: map[string]*sync.Mutex{},
	}
}

func (s *State) oauthStore() *OAuthStore {
	s.oauthMu.Lock()
	defer s.oauthMu.Unlock()
	if s.OAuth == nil {
		s.OAuth = NewOAuthStore()
	}
	return s.OAuth
}

// putPending 存入 pending 并顺手清理过期项（oauth.rs:286-290）。
func (o *OAuthStore) putPending(state string, p *PendingAuth) {
	now := nowSecs()
	o.mu.Lock()
	defer o.mu.Unlock()
	for k, v := range o.pending {
		if now-v.CreatedAt >= pendingTTLSecs {
			delete(o.pending, k)
		}
	}
	o.pending[state] = p
}

// takePending 取出并消费 pending（一次性）（oauth.rs:366-370）。
func (o *OAuthStore) takePending(state string) (*PendingAuth, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	p, ok := o.pending[state]
	if ok {
		delete(o.pending, state)
	}
	return p, ok
}

// refreshLock 返回某 server 的刷新锁（oauth.rs:445-450）。
func (o *OAuthStore) refreshLock(serverID string) *sync.Mutex {
	o.mu.Lock()
	defer o.mu.Unlock()
	l, ok := o.refreshLocks[serverID]
	if !ok {
		l = &sync.Mutex{}
		o.refreshLocks[serverID] = l
	}
	return l
}

// PendingCount 返回当前未消费的 pending 数量（测试与诊断用）。
func (o *OAuthStore) PendingCount() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.pending)
}

// OAuthStatusInfo `GET /api/config/mcp/oauth/status` 的响应体（commands.rs:646-651）。
type OAuthStatusInfo struct {
	Authorized  bool   `json:"authorized"`
	ExpiresAt   *int64 `json:"expiresAt"`
	NeedsReauth bool   `json:"needsReauth"`
}

// CallbackParams OAuth 回调查询参数（oauth.rs:326-336）。
type CallbackParams struct {
	Code             string
	State            string
	Error            string
	ErrorDescription string
}

func nowSecs() int64 { return time.Now().Unix() }

// ========== PKCE / state ==========

func base64url(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}

// randomToken 生成 len 字节随机数的 base64url 编码（oauth.rs:80-85）。
func randomToken(length int) (string, error) {
	buf := make([]byte, length)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64url(buf), nil
}

// genPKCE 生成 PKCE (code_verifier, code_challenge=S256)（oauth.rs:87-94）。
func genPKCE() (verifier, challenge string, err error) {
	verifier, err = randomToken(48) // base64url 后约 64 字符
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64url(sum[:]), nil
}

// ========== 发现链 ==========

// originOf 取 URL 的 origin（scheme://host[:port]）（oauth.rs:96-106）。
func originOf(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if u.Scheme == "" || u.Hostname() == "" {
		return "", fmt.Errorf("无法解析 endpoint origin")
	}
	return u.Scheme + "://" + u.Host, nil
}

// discoverPRMURL 步骤1：探测上游获取 Protected Resource Metadata URL
// （oauth.rs:108-128）。发最小请求触发 401，从 WWW-Authenticate 解析 resource_metadata；
// 无则回退 well-known。
func discoverPRMURL(ctx context.Context, client *http.Client, endpoint string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(`{"jsonrpc":"2.0","id":0,"method":"ping"}`))
	if err != nil {
		return "", fmt.Errorf("探测上游失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("探测上游失败: %w", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	if wa := resp.Header.Get("WWW-Authenticate"); wa != "" {
		if u, ok := parseResourceMetadata(wa); ok {
			return u, nil
		}
	}
	// 回退：<origin>/.well-known/oauth-protected-resource
	origin, err := originOf(endpoint)
	if err != nil {
		return "", err
	}
	return origin + "/.well-known/oauth-protected-resource", nil
}

// parseResourceMetadata 从 WWW-Authenticate 头解析 resource_metadata="..." 的值
// （oauth.rs:130-139）。
func parseResourceMetadata(header string) (string, bool) {
	const key = "resource_metadata="
	idx := strings.Index(header, key)
	if idx < 0 {
		return "", false
	}
	rest := header[idx+len(key):]
	rest = strings.TrimLeft(rest, "\"")
	end := strings.Index(rest, "\"")
	if end >= 0 {
		rest = rest[:end]
	}
	if rest == "" {
		return "", false
	}
	return rest, true
}

// prmDoc RFC9728 Protected Resource Metadata（oauth.rs:141-145）。
type prmDoc struct {
	AuthorizationServers []string `json:"authorization_servers"`
}

// fetchPRM 步骤2：拉 Protected Resource Metadata（RFC9728）（oauth.rs:147-163）。
func fetchPRM(ctx context.Context, client *http.Client, prmURL, endpoint string) (asURL, resource string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, prmURL, nil)
	if err != nil {
		return "", "", fmt.Errorf("拉取 PRM 失败: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("拉取 PRM 失败: %w", err)
	}
	defer resp.Body.Close()
	var doc prmDoc
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return "", "", fmt.Errorf("解析 PRM 失败: %w", err)
	}
	if len(doc.AuthorizationServers) == 0 {
		return "", "", fmt.Errorf("PRM 未提供 authorization_servers")
	}
	// resource 始终用 MCP endpoint 本身（精确到 server 路径，RFC8707 最精确 URI）。
	// 不采用 PRM 的 resource 字段：多 server 网关的 PRM 可能只返回域名级 resource，
	// 导致网关无法区分具体 server 而报错（oauth.rs:156-159）。
	return doc.AuthorizationServers[0], strings.TrimRight(endpoint, "/"), nil
}

// asMetadata RFC8414 Authorization Server Metadata（oauth.rs:165-170）。
type asMetadata struct {
	AuthorizationEndpoint string  `json:"authorization_endpoint"`
	TokenEndpoint         string  `json:"token_endpoint"`
	RegistrationEndpoint  *string `json:"registration_endpoint"`
}

// fetchASMetadata 步骤3：拉 AS Metadata（RFC8414，回退 OIDC）（oauth.rs:172-194）。
func fetchASMetadata(ctx context.Context, client *http.Client, asURL string) (*asMetadata, error) {
	base := strings.TrimRight(asURL, "/")
	candidates := []string{
		base + "/.well-known/oauth-authorization-server",
		base + "/.well-known/openid-configuration",
	}
	lastErr := ""
	for _, u := range candidates {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			lastErr = fmt.Sprintf("请求 AS metadata 失败: %v", err)
			continue
		}
		req.Header.Set("Accept", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			lastErr = fmt.Sprintf("请求 AS metadata 失败: %v", err)
			continue
		}
		if resp.StatusCode >= 200 && resp.StatusCode <= 299 {
			var meta asMetadata
			decErr := json.NewDecoder(resp.Body).Decode(&meta)
			_ = resp.Body.Close()
			if decErr != nil {
				lastErr = fmt.Sprintf("解析 AS metadata 失败: %v", decErr)
				continue
			}
			return &meta, nil
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		lastErr = fmt.Sprintf("AS metadata %s 返回 %d", u, resp.StatusCode)
	}
	return nil, fmt.Errorf("%s", lastErr)
}

// dcrResponse RFC7591 动态注册响应（oauth.rs:196-200）。
type dcrResponse struct {
	ClientID     string  `json:"client_id"`
	ClientSecret *string `json:"client_secret"`
}

// registerClient 步骤4：动态客户端注册（RFC7591）（oauth.rs:202-227）。
func registerClient(ctx context.Context, client *http.Client, registrationEndpoint, redirectURI string) (string, *string, error) {
	body, _ := json.Marshal(map[string]any{
		"client_name":                "Model Bridge",
		"redirect_uris":              []string{redirectURI},
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, registrationEndpoint, strings.NewReader(string(body)))
	if err != nil {
		return "", nil, fmt.Errorf("DCR 请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", nil, fmt.Errorf("DCR 请求失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		text, _ := io.ReadAll(resp.Body)
		return "", nil, fmt.Errorf("DCR 返回 %d: %s", resp.StatusCode, string(text))
	}
	var dcr dcrResponse
	if err := json.NewDecoder(resp.Body).Decode(&dcr); err != nil {
		return "", nil, fmt.Errorf("解析 DCR 响应失败: %w", err)
	}
	if dcr.ClientID == "" {
		return "", nil, fmt.Errorf("DCR 响应缺少 client_id")
	}
	return dcr.ClientID, dcr.ClientSecret, nil
}

// ========== 触发授权（start）==========

// RedirectURI 计算回调 redirect_uri（loopback 用 127.0.0.1，RFC8252）（oauth.rs:229-234）。
func (s *State) RedirectURI() string {
	port := uint16(config.DefaultListenPort)
	if cfg := s.snapshot(); cfg != nil && cfg.ListenPort != 0 {
		port = cfg.ListenPort
	}
	return fmt.Sprintf("http://127.0.0.1:%d/oauth/callback", port)
}

// StartOAuthFlow 触发 OAuth 授权流程，返回供前端打开的 authorize URL（oauth.rs:236-323）。
func (s *State) StartOAuthFlow(ctx context.Context, serverID string) (string, error) {
	cfg := s.snapshot()
	if cfg == nil {
		return "", fmt.Errorf("配置不可用")
	}
	var server *config.McpServerConfig
	for i := range cfg.McpServers {
		if cfg.McpServers[i].ID == serverID {
			cp := cfg.McpServers[i]
			server = &cp
			break
		}
	}
	if server == nil {
		return "", fmt.Errorf("MCP server 不存在: %s", serverID)
	}

	client := s.HTTPClient()
	redirectURI := s.RedirectURI()

	// 发现链（全部锁外）
	prmURL, err := discoverPRMURL(ctx, client, server.Endpoint)
	if err != nil {
		return "", err
	}
	asURL, resource, err := fetchPRM(ctx, client, prmURL, server.Endpoint)
	if err != nil {
		return "", err
	}
	meta, err := fetchASMetadata(ctx, client, asURL)
	if err != nil {
		return "", err
	}
	if meta.RegistrationEndpoint == nil || *meta.RegistrationEndpoint == "" {
		return "", fmt.Errorf("上游 AS 不支持动态注册（无 registration_endpoint），当前仅支持 DCR")
	}
	registrationEndpoint := *meta.RegistrationEndpoint

	// 复用已注册的 client_id（若有）
	existing := server.OAuth
	if existing == nil {
		existing = &config.OAuthData{}
	}
	clientID := ""
	var clientSecret *string
	if existing.ClientID != nil && *existing.ClientID != "" {
		clientID = *existing.ClientID
		clientSecret = existing.ClientSecret
	} else {
		clientID, clientSecret, err = registerClient(ctx, client, registrationEndpoint, redirectURI)
		if err != nil {
			return "", err
		}
	}

	// PKCE + state
	verifier, challenge, err := genPKCE()
	if err != nil {
		return "", fmt.Errorf("生成 PKCE 失败: %w", err)
	}
	stateToken, err := randomToken(24)
	if err != nil {
		return "", fmt.Errorf("生成 state 失败: %w", err)
	}

	// 持久化发现/注册结果（token 暂空）
	authEndpoint := meta.AuthorizationEndpoint
	tokenEndpoint := meta.TokenEndpoint
	regEndpoint := registrationEndpoint
	oauthData := config.OAuthData{
		AuthorizationEndpoint: &authEndpoint,
		TokenEndpoint:         &tokenEndpoint,
		RegistrationEndpoint:  &regEndpoint,
		Resource:              &resource,
		Scope:                 existing.Scope,
		RedirectURI:           &redirectURI,
		ClientID:              &clientID,
		ClientSecret:          clientSecret,
		AccessToken:           existing.AccessToken,
		RefreshToken:          existing.RefreshToken,
		TokenExpiresAt:        existing.TokenExpiresAt,
	}
	if err := s.persistOAuth(serverID, oauthData); err != nil {
		return "", err
	}

	// 存 pending（内存）
	s.oauthStore().putPending(stateToken, &PendingAuth{
		ServerID:      serverID,
		CodeVerifier:  verifier,
		RedirectURI:   redirectURI,
		Resource:      resource,
		TokenEndpoint: tokenEndpoint,
		ClientID:      clientID,
		ClientSecret:  clientSecret,
		Scope:         existing.Scope,
		CreatedAt:     nowSecs(),
	})

	// 构造 authorize URL
	authURL, err := url.Parse(meta.AuthorizationEndpoint)
	if err != nil || meta.AuthorizationEndpoint == "" {
		return "", fmt.Errorf("authorization_endpoint 非法: %s", meta.AuthorizationEndpoint)
	}
	q := authURL.Query()
	q.Set("response_type", "code")
	q.Set("client_id", clientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("state", stateToken)
	q.Set("resource", resource)
	if existing.Scope != nil && *existing.Scope != "" {
		q.Set("scope", *existing.Scope)
	}
	authURL.RawQuery = q.Encode()
	return authURL.String(), nil
}

// ========== 回调（callback）==========

// tokenResponse token endpoint 响应（oauth.rs:338-346）。
type tokenResponse struct {
	AccessToken  string  `json:"access_token"`
	RefreshToken *string `json:"refresh_token"`
	ExpiresIn    *int64  `json:"expires_in"`
}

// CompleteOAuthFlow 用回调参数换取 token 并持久化，返回 server 名称（oauth.rs:357-417）。
func (s *State) CompleteOAuthFlow(ctx context.Context, params CallbackParams) (string, error) {
	if params.Error != "" {
		return "", fmt.Errorf("授权服务器返回错误: %s %s", params.Error, params.ErrorDescription)
	}
	if params.Code == "" {
		return "", fmt.Errorf("回调缺少 code 参数")
	}
	if params.State == "" {
		return "", fmt.Errorf("回调缺少 state 参数")
	}

	// 取出并消费 pending（一次性）
	pending, ok := s.oauthStore().takePending(params.State)
	if !ok {
		return "", fmt.Errorf("无效或已过期的 state（可能是 CSRF 或超时）")
	}
	if nowSecs()-pending.CreatedAt >= pendingTTLSecs {
		return "", fmt.Errorf("授权请求已超时，请重新发起")
	}

	// 换 token（锁外）
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {params.Code},
		"redirect_uri":  {pending.RedirectURI},
		"code_verifier": {pending.CodeVerifier},
		"client_id":     {pending.ClientID},
		"resource":      {pending.Resource},
	}
	if pending.ClientSecret != nil {
		form.Set("client_secret", *pending.ClientSecret)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, pending.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("换取 token 失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := s.HTTPClient().Do(req)
	if err != nil {
		return "", fmt.Errorf("换取 token 失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		text, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("token endpoint 返回 %d: %s", resp.StatusCode, string(text))
	}
	var tok tokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil {
		return "", fmt.Errorf("解析 token 响应失败: %w", err)
	}

	// 写回 config 并持久化
	serverName := pending.ServerID
	if cfg := s.snapshot(); cfg != nil {
		for i := range cfg.McpServers {
			if cfg.McpServers[i].ID == pending.ServerID {
				serverName = cfg.McpServers[i].Name
				break
			}
		}
	}

	data := s.currentOAuth(pending.ServerID)
	access := tok.AccessToken
	data.AccessToken = &access
	if tok.RefreshToken != nil {
		data.RefreshToken = tok.RefreshToken
	}
	if tok.ExpiresIn != nil {
		exp := nowSecs() + *tok.ExpiresIn
		data.TokenExpiresAt = &exp
	}
	if err := s.persistOAuth(pending.ServerID, data); err != nil {
		return "", err
	}
	return serverName, nil
}

// HandleOAuthCallback 是 `GET /oauth/callback` 的 handler（oauth.rs:344-355）。
func (s *State) HandleOAuthCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	params := CallbackParams{
		Code:             q.Get("code"),
		State:            q.Get("state"),
		Error:            q.Get("error"),
		ErrorDescription: q.Get("error_description"),
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if name, err := s.CompleteOAuthFlow(r.Context(), params); err == nil {
		_, _ = w.Write([]byte(resultPage("授权成功", fmt.Sprintf("MCP server「%s」已授权，可关闭此窗口。", name), true)))
	} else {
		_, _ = w.Write([]byte(resultPage("授权失败", err.Error(), false)))
	}
}

// ========== 转发期 token 获取 / 刷新 ==========

// EnsureValidToken 转发前调用：返回应注入的 access token（oauth.rs:419-509）。
//   - 未启用 OAuth → (nil, nil)（走原有 auth_token/透传逻辑）
//   - token 有效 → (token, nil)
//   - 临近过期且有 refresh → 刷新后返回
//   - 无 token 或刷新失败 → (nil, err)（调用方应提示重新授权）
func (s *State) EnsureValidToken(ctx context.Context, server *config.McpServerConfig) (*string, error) {
	if server == nil || !server.OAuthEnabled {
		return nil, nil
	}
	data := server.OAuth
	if data == nil {
		data = &config.OAuthData{}
	}
	access := data.AccessToken
	expiresAt := data.TokenExpiresAt

	needsRefresh := false
	switch {
	case access == nil:
		needsRefresh = true
	case expiresAt != nil:
		needsRefresh = nowSecs()+expirySkewSecs >= *expiresAt
	default:
		needsRefresh = false // 无过期信息，乐观使用
	}
	if !needsRefresh {
		return access, nil
	}

	// 需要刷新：必须有 refresh_token
	if data.RefreshToken == nil {
		return nil, fmt.Errorf("server %s 未授权或 token 已过期", server.ID)
	}

	// per-server 刷新锁
	lock := s.oauthStore().refreshLock(server.ID)
	lock.Lock()
	defer lock.Unlock()

	// 拿到锁后重新读 config：可能已被其它请求刷新过
	fresh := s.currentOAuth(server.ID)
	if fresh.AccessToken != nil && fresh.TokenExpiresAt != nil && nowSecs()+expirySkewSecs < *fresh.TokenExpiresAt {
		return fresh.AccessToken, nil
	}

	if fresh.TokenEndpoint == nil {
		return nil, fmt.Errorf("server %s 缺少 token_endpoint", server.ID)
	}
	if fresh.ClientID == nil {
		return nil, fmt.Errorf("server %s 缺少 client_id", server.ID)
	}
	resource := ""
	if fresh.Resource != nil {
		resource = *fresh.Resource
	}

	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {*data.RefreshToken},
		"client_id":     {*fresh.ClientID},
	}
	if resource != "" {
		form.Set("resource", resource)
	}
	if fresh.ClientSecret != nil {
		form.Set("client_secret", *fresh.ClientSecret)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, *fresh.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("刷新 token 网络错误: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := s.HTTPClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("刷新 token 网络错误: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		text := string(body)
		if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnauthorized ||
			strings.Contains(text, "invalid_client") ||
			strings.Contains(text, "client expired") ||
			strings.Contains(text, "客户端已过期") {
			// DCR clients can have a shorter lifetime than refresh tokens. Remove
			// the stale registration so the next authorization flow performs DCR
			// again instead of reusing the expired client_id forever.
			if clearErr := s.ClearOAuthRegistration(server.ID); clearErr != nil {
				return nil, clearErr
			}
		}
		return nil, fmt.Errorf("刷新 token 失败（%d），请重新授权", resp.StatusCode)
	}
	var tok tokenResponse
	decErr := json.NewDecoder(resp.Body).Decode(&tok)
	_ = resp.Body.Close()
	if decErr != nil {
		return nil, fmt.Errorf("解析刷新响应失败: %w", decErr)
	}

	updated := fresh
	newAccess := tok.AccessToken
	updated.AccessToken = &newAccess
	if tok.RefreshToken != nil {
		updated.RefreshToken = tok.RefreshToken
	}
	if tok.ExpiresIn != nil {
		exp := nowSecs() + *tok.ExpiresIn
		updated.TokenExpiresAt = &exp
	}
	if err := s.persistOAuth(server.ID, updated); err != nil {
		return nil, err
	}
	return updated.AccessToken, nil
}

// RefreshExpiringTokens 刷新所有「临近过期」的 OAuth server，让低流量 server
// 在下次用户请求前就完成续期（oauth.rs:511-541）。
func (s *State) RefreshExpiringTokens(ctx context.Context) {
	cfg := s.snapshot()
	if cfg == nil {
		return
	}
	servers := make([]config.McpServerConfig, 0, len(cfg.McpServers))
	for i := range cfg.McpServers {
		if cfg.McpServers[i].OAuthEnabled {
			servers = append(servers, cfg.McpServers[i])
		}
	}

	now := nowSecs()
	for i := range servers {
		server := servers[i]
		oauth := server.OAuth
		if oauth == nil {
			continue
		}
		shouldRefresh := oauth.AccessToken != nil && oauth.RefreshToken != nil &&
			oauth.TokenExpiresAt != nil && now+expirySkewSecs >= *oauth.TokenExpiresAt
		if !shouldRefresh {
			continue
		}
		if _, err := s.EnsureValidToken(ctx, &server); err != nil {
			continue // Rust 侧只记 warn 日志
		}
	}
}

// RunTokenRefresher 后台按 [TokenRefreshIntervalSecs] 周期刷新 token，ctx 取消后退出
// （oauth.rs:30-32 的常量在 Rust 侧由外部定时任务使用）。
func (s *State) RunTokenRefresher(ctx context.Context) {
	ticker := time.NewTicker(time.Duration(TokenRefreshIntervalSecs) * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.RefreshExpiringTokens(ctx)
		}
	}
}

// StartTokenRefresher 启动后台刷新 goroutine，返回的 stop 保证 goroutine 已退出。
func (s *State) StartTokenRefresher(ctx context.Context) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.RunTokenRefresher(ctx)
	}()
	return func() {
		cancel()
		<-done
	}
}

// ClearOAuthRegistration 清除动态注册与 token，保留发现元数据；
// 下一次 StartOAuthFlow 会重新 DCR（oauth.rs:543-555）。
func (s *State) ClearOAuthRegistration(serverID string) error {
	data := s.currentOAuth(serverID)
	data.ClientID = nil
	data.ClientSecret = nil
	data.AccessToken = nil
	data.RefreshToken = nil
	data.TokenExpiresAt = nil
	return s.persistOAuth(serverID, data)
}

// OAuthStatus 查询某 server 的授权状态（oauth.rs:576-589）。
// authorized = 有 access_token 且（未过期 或 有 refresh_token 可刷新）。
func (s *State) OAuthStatus(serverID string) OAuthStatusInfo {
	data := s.currentOAuth(serverID)
	hasToken := data.AccessToken != nil
	expiresAt := data.TokenExpiresAt
	expired := false
	if expiresAt != nil {
		expired = nowSecs() >= *expiresAt
	}
	return OAuthStatusInfo{
		Authorized:  hasToken && (!expired || data.RefreshToken != nil),
		ExpiresAt:   expiresAt,
		NeedsReauth: !hasToken || (expired && data.RefreshToken == nil),
	}
}

// ========== 辅助：读写 config 中的 oauth ==========

// currentOAuth 读取某 server 当前的 OAuthData（oauth.rs:557-560）；不存在时返回空结构。
func (s *State) currentOAuth(serverID string) config.OAuthData {
	cfg := s.snapshot()
	if cfg == nil {
		return config.OAuthData{}
	}
	for i := range cfg.McpServers {
		if cfg.McpServers[i].ID == serverID {
			if cfg.McpServers[i].OAuth == nil {
				return config.OAuthData{}
			}
			return *cfg.McpServers[i].OAuth
		}
	}
	return config.OAuthData{}
}

// persistOAuth 写入某 server 的 OAuthData 并落盘（oauth.rs:562-574）。
func (s *State) persistOAuth(serverID string, data config.OAuthData) error {
	if s.Config == nil {
		return fmt.Errorf("配置存储不可用")
	}
	notFound := false
	err := s.Config.Update(func(cfg *config.AppConfig) error {
		for i := range cfg.McpServers {
			if cfg.McpServers[i].ID == serverID {
				cp := data
				cfg.McpServers[i].OAuth = &cp
				return nil
			}
		}
		notFound = true
		return fmt.Errorf("MCP server 不存在: %s", serverID)
	})
	if err != nil {
		if notFound {
			return fmt.Errorf("MCP server 不存在: %s", serverID)
		}
		return err
	}
	return nil
}

// ========== 回调结果页 ==========

// resultPage 与 oauth.rs:591-603 的 result_page 同构（内联 CSS，成功/失败两色）。
func resultPage(title, message string, ok bool) string {
	color := "#059669"
	if !ok {
		color = "#dc2626"
	}
	esc := func(s string) string {
		replacer := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
		return replacer.Replace(s)
	}
	return `<!DOCTYPE html><html><head><meta charset="utf-8"><title>` + esc(title) + `</title>
<style>body{font-family:system-ui,sans-serif;display:flex;align-items:center;justify-content:center;height:100vh;margin:0;background:#f1f5f9}
.card{background:#fff;padding:32px 40px;border-radius:12px;box-shadow:0 2px 12px rgba(0,0,0,.08);text-align:center;max-width:420px}
h1{color:` + color + `;font-size:20px;margin:0 0 12px}p{color:#475569;font-size:14px;line-height:1.6;margin:0}</style></head>
<body><div class="card"><h1>` + esc(title) + `</h1><p>` + esc(message) + `</p></div></body></html>`
}
