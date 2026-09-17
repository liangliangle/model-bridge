// Package config 负责配置的加载、默认值与模型解析。
//
// 字段与语义必须与 Rust 侧 src-tauri/src/channel/config.rs 保持一致：
// 配置文件路径、YAML 字段名、默认值、${VAR} 展开规则、以及 endpoint 的新旧两种写法。
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// ProviderType 渠道侧协议。
type ProviderType string

const (
	ProviderOpenAI          ProviderType = "openai"
	ProviderOpenAIResponses ProviderType = "openai_responses"
	ProviderAnthropic       ProviderType = "anthropic"
)

// MappingSource 模型名的来源，用于审计展示。
type MappingSource string

const (
	SourceExplicit MappingSource = "explicit"
	SourceFallback MappingSource = "fallback"
)

// EndpointConfig 渠道端点。
//
// 兼容两种写法：
//   - 新格式：url: "https://..."
//   - 旧格式：chat_completions: "https://..."（可选叠加 responses，优先取 responses）
type EndpointConfig struct {
	URL string `json:"url" yaml:"url"`
}

// UnmarshalYAML 实现新旧两种写法的兼容解析。
func (e *EndpointConfig) UnmarshalYAML(value *yaml.Node) error {
	var raw map[string]string
	if err := value.Decode(&raw); err != nil {
		return err
	}
	if url, ok := raw["url"]; ok {
		e.URL = url
		return nil
	}
	if cc, ok := raw["chat_completions"]; ok {
		if resp, ok := raw["responses"]; ok && resp != "" {
			e.URL = resp
			return nil
		}
		e.URL = cc
		return nil
	}
	return fmt.Errorf("endpoint must have 'url' or 'chat_completions' field")
}

// UnmarshalJSON 同样兼容两种写法（管理 API 会回传端点）。
func (e *EndpointConfig) UnmarshalJSON(data []byte) error {
	var raw map[string]string
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if url, ok := raw["url"]; ok {
		e.URL = url
		return nil
	}
	if cc, ok := raw["chat_completions"]; ok {
		if resp, ok := raw["responses"]; ok && resp != "" {
			e.URL = resp
			return nil
		}
		e.URL = cc
		return nil
	}
	return fmt.Errorf("endpoint must have 'url' or 'chat_completions' field")
}

// ChannelConfig 单个渠道。
type ChannelConfig struct {
	ID            string            `json:"id" yaml:"id"`
	Name          string            `json:"name" yaml:"name"`
	Provider      ProviderType      `json:"provider" yaml:"provider"`
	Endpoint      EndpointConfig    `json:"endpoint" yaml:"endpoint"`
	APIKey        string            `json:"api_key" yaml:"api_key"`
	Priority      uint32            `json:"priority" yaml:"priority"`
	Enabled       bool              `json:"enabled" yaml:"enabled"`
	FallbackModel string            `json:"fallback_model" yaml:"fallback_model"`
	ModelMapping  map[string]string `json:"model_mapping" yaml:"model_mapping"`
	TimeoutMs     uint64            `json:"timeout_ms" yaml:"timeout_ms"`
	RateLimit     *uint32           `json:"rate_limit" yaml:"rate_limit"`
	CustomHeaders map[string]string `json:"custom_headers" yaml:"custom_headers"`
	StripThinking bool              `json:"strip_thinking" yaml:"strip_thinking"`
	RetryCount    uint32            `json:"retry_count" yaml:"retry_count"`
	RetryDelayMs  uint64            `json:"retry_delay_ms" yaml:"retry_delay_ms"`
	ForceEffort   *string           `json:"force_effort" yaml:"force_effort"`
	AutoCache     bool              `json:"auto_cache" yaml:"auto_cache"`
}

// ResolveModel 解析模型名：优先精确映射，未命中用兜底模型。
func (c *ChannelConfig) ResolveModel(alias string) (string, MappingSource) {
	if mapped, ok := c.ModelMapping[alias]; ok {
		return mapped, SourceExplicit
	}
	return c.FallbackModel, SourceFallback
}

// CircuitBreakerConfig 熔断器配置。
type CircuitBreakerConfig struct {
	FailureThreshold    uint32 `json:"failure_threshold" yaml:"failure_threshold"`
	RecoveryIntervalSec uint64 `json:"recovery_interval_sec" yaml:"recovery_interval_sec"`
	ProbeRequests       uint32 `json:"probe_requests" yaml:"probe_requests"`
}

// FailoverConfig 故障转移配置。
type FailoverConfig struct {
	// MaxFailoverChannels 单次请求最多尝试几个候选渠道；0 = 不限制。
	// 注意：这是「候选渠道数量」上限，不是重试次数（重试次数见 ChannelConfig.RetryCount）。
	// 兼容旧字段名 max_retries。
	MaxFailoverChannels uint32               `json:"max_failover_channels" yaml:"max_failover_channels"`
	RetryTimeoutMs      uint64               `json:"retry_timeout_ms" yaml:"retry_timeout_ms"`
	CircuitBreaker      CircuitBreakerConfig `json:"circuit_breaker" yaml:"circuit_breaker"`
}

// UltimateFallback 全部失败后的最终兜底渠道。
type UltimateFallback struct {
	Channel string `json:"channel" yaml:"channel"`
}

// AuthConfig 鉴权配置。
type AuthConfig struct {
	ProxyTokens []string `json:"proxy_tokens" yaml:"proxy_tokens"`
	AdminToken  *string  `json:"admin_token" yaml:"admin_token"`
}

// ToolInfo 上游工具的缓存快照。
type ToolInfo struct {
	Name        string         `json:"name" yaml:"name"`
	Description *string        `json:"description,omitempty" yaml:"description,omitempty"`
	InputSchema map[string]any `json:"input_schema,omitempty" yaml:"input_schema,omitempty"`
}

// OAuthData OAuth 2.1 的持久化快照（发现 + DCR 注册 + token）。
type OAuthData struct {
	AuthorizationEndpoint *string `json:"authorization_endpoint,omitempty" yaml:"authorization_endpoint,omitempty"`
	TokenEndpoint         *string `json:"token_endpoint,omitempty" yaml:"token_endpoint,omitempty"`
	RegistrationEndpoint  *string `json:"registration_endpoint,omitempty" yaml:"registration_endpoint,omitempty"`
	Resource              *string `json:"resource,omitempty" yaml:"resource,omitempty"`
	Scope                 *string `json:"scope,omitempty" yaml:"scope,omitempty"`
	RedirectURI           *string `json:"redirect_uri,omitempty" yaml:"redirect_uri,omitempty"`
	ClientID              *string `json:"client_id,omitempty" yaml:"client_id,omitempty"`
	ClientSecret          *string `json:"client_secret,omitempty" yaml:"client_secret,omitempty"`
	AccessToken           *string `json:"access_token,omitempty" yaml:"access_token,omitempty"`
	RefreshToken          *string `json:"refresh_token,omitempty" yaml:"refresh_token,omitempty"`
	TokenExpiresAt        *int64  `json:"token_expires_at,omitempty" yaml:"token_expires_at,omitempty"`
}

// McpServerConfig MCP 中继上游 server。
type McpServerConfig struct {
	ID            string            `json:"id" yaml:"id"`
	Name          string            `json:"name" yaml:"name"`
	Endpoint      string            `json:"endpoint" yaml:"endpoint"`
	AuthToken     *string           `json:"auth_token,omitempty" yaml:"auth_token,omitempty"`
	CustomHeaders map[string]string `json:"custom_headers" yaml:"custom_headers"`
	BlockedTools  []string          `json:"blocked_tools" yaml:"blocked_tools"`
	Enabled       bool              `json:"enabled" yaml:"enabled"`
	OAuthEnabled  bool              `json:"oauth_enabled" yaml:"oauth_enabled"`
	OAuth         *OAuthData        `json:"oauth,omitempty" yaml:"oauth,omitempty"`
	CachedTools   []ToolInfo        `json:"cached_tools" yaml:"cached_tools"`
}

// ModelPrice 单个模型定价（每百万 token，USD）。
type ModelPrice struct {
	Model             string   `json:"model" yaml:"model"`
	InputPerMtok      float64  `json:"input_per_mtok" yaml:"input_per_mtok"`
	OutputPerMtok     float64  `json:"output_per_mtok" yaml:"output_per_mtok"`
	CacheReadPerMtok  *float64 `json:"cache_read_per_mtok,omitempty" yaml:"cache_read_per_mtok,omitempty"`
	CacheWritePerMtok *float64 `json:"cache_write_per_mtok,omitempty" yaml:"cache_write_per_mtok,omitempty"`
	Enabled           bool     `json:"enabled" yaml:"enabled"`
}

// AppConfig 应用全局配置。
type AppConfig struct {
	ListenPort       uint16            `json:"listen_port" yaml:"listen_port"`
	ListenHost       string            `json:"listen_host" yaml:"listen_host"`
	PublicURL        *string           `json:"public_url" yaml:"public_url"`
	Debug            bool              `json:"debug" yaml:"debug"`
	Models           []string          `json:"models" yaml:"models"`
	Channels         []ChannelConfig   `json:"channels" yaml:"channels"`
	Failover         FailoverConfig    `json:"failover" yaml:"failover"`
	UltimateFallback *UltimateFallback `json:"ultimate_fallback" yaml:"ultimate_fallback"`
	Auth             AuthConfig        `json:"auth" yaml:"auth"`
	McpServers       []McpServerConfig `json:"mcp_servers" yaml:"mcp_servers"`
	ModelPrices      []ModelPrice      `json:"model_prices" yaml:"model_prices"`
	// AuditRetentionDays nil 或 0 = 永久留存；>0 = 删除超过 N 天的记录。
	AuditRetentionDays *uint32 `json:"audit_retention_days" yaml:"audit_retention_days"`

	// maxRetriesAlias 承接旧字段名 max_retries；仅在 MaxFailoverChannels 未显式给出时生效。
	maxRetriesAlias *uint32
}

// 默认值，与 Rust 侧 default_* 函数一一对应。
const (
	DefaultListenPort          uint16 = 8080
	DefaultListenHost                 = "127.0.0.1"
	DefaultPriority            uint32 = 10
	DefaultTimeoutMs           uint64 = 300000
	DefaultRetryDelayMs        uint64 = 500
	DefaultMaxFailoverChannels uint32 = 3
	DefaultRetryTimeoutMs      uint64 = 5000
	DefaultFailureThreshold    uint32 = 3
	DefaultRecoveryIntervalSec uint64 = 30
	DefaultProbeRequests       uint32 = 2
)

// DefaultConfig 生成一份带默认值的配置（channels 为空）。
func DefaultConfig() *AppConfig {
	return &AppConfig{
		ListenPort: DefaultListenPort,
		ListenHost: DefaultListenHost,
		Failover: FailoverConfig{
			MaxFailoverChannels: DefaultMaxFailoverChannels,
			RetryTimeoutMs:      DefaultRetryTimeoutMs,
			CircuitBreaker: CircuitBreakerConfig{
				FailureThreshold:    DefaultFailureThreshold,
				RecoveryIntervalSec: DefaultRecoveryIntervalSec,
				ProbeRequests:       DefaultProbeRequests,
			},
		},
	}
}

// ConfigPath 返回用户配置路径（与 Rust 侧一致）。
func ConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".model-bridge", "config.yaml")
}

// AuditDBPath 审计数据库路径。
func AuditDBPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".model-bridge", "audit.db")
}

// LoadFromFile 从 YAML 文件加载配置，支持 ${VAR} 与 ${VAR:-default} 展开。
// 文件不存在时写出默认模板并返回默认配置。
func LoadFromFile(path string) (*AppConfig, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			cfg := DefaultConfig()
			if werr := cfg.SaveToFile(path); werr != nil {
				return nil, werr
			}
			return cfg, nil
		}
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}
	return Parse(content)
}

// Parse 解析配置内容（已含环境变量展开）。
func Parse(content []byte) (*AppConfig, error) {
	expanded := ExpandEnvVars(string(content))

	// 先取旧字段名 max_retries，再决定是否覆盖新名。
	var probe struct {
		Failover struct {
			MaxRetries *uint32 `yaml:"max_retries"`
		} `yaml:"failover"`
	}
	_ = yaml.Unmarshal([]byte(expanded), &probe)

	// yaml.v3 对未出现的字段保留零值，无法区分「未给出」与「显式 0」，
	// 因此用一个中间结构记录哪些字段出现过。
	var raw map[string]yaml.Node
	if err := yaml.Unmarshal([]byte(expanded), &raw); err != nil {
		return nil, fmt.Errorf("failed to parse config: %w", err)
	}

	cfg := DefaultConfig()
	if err := yaml.Unmarshal([]byte(expanded), cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config: %w", err)
	}
	applyDefaults(cfg, raw)
	if probe.Failover.MaxRetries != nil {
		cfg.maxRetriesAlias = probe.Failover.MaxRetries
		// 新字段名未显式出现时，旧名生效
		if !failoverHasMaxFailoverChannels(raw) {
			cfg.Failover.MaxFailoverChannels = *probe.Failover.MaxRetries
		}
	}
	return cfg, nil
}

func failoverHasMaxFailoverChannels(raw map[string]yaml.Node) bool {
	node, ok := raw["failover"]
	if !ok {
		return false
	}
	var m map[string]yaml.Node
	if err := node.Decode(&m); err != nil {
		return false
	}
	_, ok = m["max_failover_channels"]
	return ok
}

// applyDefaults 对「未显式给出」的字段补齐默认值。
// yaml.v3 会把缺失的 bool 解成 false、缺失的数值解成 0，
// 因此这里按「字段是否出现在原文」判断，避免把用户的显式 false/0 覆盖掉。
func applyDefaults(cfg *AppConfig, raw map[string]yaml.Node) {
	if cfg.ListenHost == "" {
		cfg.ListenHost = DefaultListenHost
	}
	if cfg.ListenPort == 0 {
		cfg.ListenPort = DefaultListenPort
	}
	if cfg.Models == nil {
		cfg.Models = []string{}
	}
	if cfg.McpServers == nil {
		cfg.McpServers = []McpServerConfig{}
	}
	if cfg.ModelPrices == nil {
		cfg.ModelPrices = []ModelPrice{}
	}
	if cfg.Auth.ProxyTokens == nil {
		cfg.Auth.ProxyTokens = []string{}
	}

	// failover
	fo := raw["failover"]
	var foMap map[string]yaml.Node
	if fo.Kind != 0 {
		_ = fo.Decode(&foMap)
	}
	if _, ok := foMap["max_failover_channels"]; !ok {
		cfg.Failover.MaxFailoverChannels = DefaultMaxFailoverChannels
	}
	if _, ok := foMap["retry_timeout_ms"]; !ok {
		cfg.Failover.RetryTimeoutMs = DefaultRetryTimeoutMs
	}
	var cbMap map[string]yaml.Node
	if cb, ok := foMap["circuit_breaker"]; ok {
		_ = cb.Decode(&cbMap)
	}
	if _, ok := cbMap["failure_threshold"]; !ok {
		cfg.Failover.CircuitBreaker.FailureThreshold = DefaultFailureThreshold
	}
	if _, ok := cbMap["recovery_interval_sec"]; !ok {
		cfg.Failover.CircuitBreaker.RecoveryIntervalSec = DefaultRecoveryIntervalSec
	}
	if _, ok := cbMap["probe_requests"]; !ok {
		cfg.Failover.CircuitBreaker.ProbeRequests = DefaultProbeRequests
	}

	// channels
	var chNodes []yaml.Node
	if ch, ok := raw["channels"]; ok {
		_ = ch.Decode(&chNodes)
	}
	for i := range cfg.Channels {
		var single map[string]yaml.Node
		if i < len(chNodes) {
			_ = chNodes[i].Decode(&single)
		}
		setIfMissing(single, "priority", &cfg.Channels[i].Priority, DefaultPriority)
		setIfMissing(single, "enabled", &cfg.Channels[i].Enabled, true)
		setIfMissing(single, "timeout_ms", &cfg.Channels[i].TimeoutMs, DefaultTimeoutMs)
		setIfMissing(single, "retry_count", &cfg.Channels[i].RetryCount, uint32(0))
		setIfMissing(single, "retry_delay_ms", &cfg.Channels[i].RetryDelayMs, DefaultRetryDelayMs)
		setIfMissing(single, "auto_cache", &cfg.Channels[i].AutoCache, true)
		if cfg.Channels[i].ModelMapping == nil {
			cfg.Channels[i].ModelMapping = map[string]string{}
		}
		if cfg.Channels[i].CustomHeaders == nil {
			cfg.Channels[i].CustomHeaders = map[string]string{}
		}
	}

	// mcp servers
	var mcpNodes []yaml.Node
	if m, ok := raw["mcp_servers"]; ok {
		_ = m.Decode(&mcpNodes)
	}
	for i := range cfg.McpServers {
		var single map[string]yaml.Node
		if i < len(mcpNodes) {
			_ = mcpNodes[i].Decode(&single)
		}
		setIfMissing(single, "enabled", &cfg.McpServers[i].Enabled, true)
		if cfg.McpServers[i].BlockedTools == nil {
			cfg.McpServers[i].BlockedTools = []string{}
		}
		if cfg.McpServers[i].CustomHeaders == nil {
			cfg.McpServers[i].CustomHeaders = map[string]string{}
		}
		if cfg.McpServers[i].CachedTools == nil {
			cfg.McpServers[i].CachedTools = []ToolInfo{}
		}
	}

	// model prices
	var priceNodes []yaml.Node
	if p, ok := raw["model_prices"]; ok {
		_ = p.Decode(&priceNodes)
	}
	for i := range cfg.ModelPrices {
		var single map[string]yaml.Node
		if i < len(priceNodes) {
			_ = priceNodes[i].Decode(&single)
		}
		setIfMissing(single, "enabled", &cfg.ModelPrices[i].Enabled, true)
	}
}

func setIfMissing[T any](present map[string]yaml.Node, key string, dst *T, def T) {
	if _, ok := present[key]; !ok {
		*dst = def
	}
}

// SaveToFile 将配置写回磁盘（YAML），并同步更新内存中的旧字段别名。
func (c *AppConfig) SaveToFile(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("failed to create config dir: %w", err)
	}
	out, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("failed to serialize config: %w", err)
	}
	return os.WriteFile(path, out, 0o644)
}

// SortedChannels 返回按优先级升序排列的已启用渠道（稳定排序，相同优先级保持配置顺序）。
func (c *AppConfig) SortedChannels() []*ChannelConfig {
	out := make([]*ChannelConfig, 0, len(c.Channels))
	for i := range c.Channels {
		if c.Channels[i].Enabled {
			out = append(out, &c.Channels[i])
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Priority < out[j].Priority })
	return out
}

// FindChannel 按 id 查找渠道。
func (c *AppConfig) FindChannel(id string) *ChannelConfig {
	for i := range c.Channels {
		if c.Channels[i].ID == id {
			return &c.Channels[i]
		}
	}
	return nil
}

// FindMCPServer 按 id 查找已启用的 MCP server。
func (c *AppConfig) FindMCPServer(id string) *McpServerConfig {
	for i := range c.McpServers {
		if c.McpServers[i].ID == id && c.McpServers[i].Enabled {
			return &c.McpServers[i]
		}
	}
	return nil
}

// FindModelPrice 按实际模型名精确查找已启用的定价。
func (c *AppConfig) FindModelPrice(model string) *ModelPrice {
	for i := range c.ModelPrices {
		if c.ModelPrices[i].Enabled && c.ModelPrices[i].Model == model {
			return &c.ModelPrices[i]
		}
	}
	return nil
}

// CandidateLimit 把 max_failover_channels 转成候选上限；0 表示不限制（返回 false）。
func (c *AppConfig) CandidateLimit() (int, bool) {
	if c.Failover.MaxFailoverChannels == 0 {
		return 0, false
	}
	return int(c.Failover.MaxFailoverChannels), true
}

// ExpandEnvVars 展开 ${VAR} 与 ${VAR:-default}。
func ExpandEnvVars(content string) string {
	var b strings.Builder
	b.Grow(len(content))
	runes := []rune(content)
	for i := 0; i < len(runes); i++ {
		if runes[i] == '$' && i+1 < len(runes) && runes[i+1] == '{' {
			end := i + 2
			for end < len(runes) && runes[end] != '}' {
				end++
			}
			if end < len(runes) {
				expr := string(runes[i+2 : end])
				varName, def := expr, ""
				if pos := strings.Index(expr, ":-"); pos >= 0 {
					varName, def = expr[:pos], expr[pos+2:]
				}
				if val, ok := os.LookupEnv(varName); ok {
					b.WriteString(val)
				} else {
					b.WriteString(def)
				}
				i = end
				continue
			}
		}
		b.WriteRune(runes[i])
	}
	return b.String()
}
