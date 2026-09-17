// Package config 负责配置的加载、默认值与模型解析。
//
// 字段与语义必须与 Rust 侧 src-tauri/src/channel/config.rs 保持一致：
// 配置文件路径、YAML 字段名、默认值、${VAR} 展开规则、以及 endpoint 的新旧两种写法。
package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"reflect"
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

// SaveToFile 将配置写回磁盘（YAML）。
//
// 文件已存在时按「文档合并」写回：原有注释、键顺序与手写的未知字段都会保留，
// 只有真正变化的取值被就地替换（新键追加在该映射末尾）。文件不存在、为空或
// 无法解析时整体序列化一份新文档（无法解析时会打日志告警，绝不把用户的文件
// 截断成读不回来的形态）。
//
// 落盘始终是 0600（配置里含 API key 与管理令牌），并通过「同目录临时文件 +
// rename」原子替换——rename 顺带避免了覆盖正在运行的二进制时的 ETXTBSY。
func (c *AppConfig) SaveToFile(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("failed to create config dir: %w", err)
	}
	out, err := c.encodeForSave(path)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(path, out, 0o600); err != nil {
		return fmt.Errorf("failed to write config %s: %w", path, err)
	}
	return nil
}

// encodeForSave 生成落盘内容：优先在既有文档上做合并写回。
func (c *AppConfig) encodeForSave(path string) ([]byte, error) {
	existing, err := os.ReadFile(path)
	switch {
	case os.IsNotExist(err):
		return marshalFresh(c)
	case err != nil:
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}
	if len(bytes.TrimSpace(existing)) == 0 {
		// 空文件没有任何注释/顺序可保留，直接写一份新文档。
		return marshalFresh(c)
	}
	out, mergeErr := mergeIntoDocument(existing, c, detectIndent(existing))
	if mergeErr != nil {
		// 不能让解析失败演变成「把用户文件截断」：整体重写一份能读回来的新文档。
		log.Printf("config: 无法合并 %s（%v），改为整体重写配置文件", path, mergeErr)
		return marshalFresh(c)
	}
	return out, nil
}

// detectIndent 推断既有文档的缩进宽度，让合并写回不会顺手把整篇重排。
//
// yaml.v3 的编码器只接受一个缩进宽度（默认 4），因此这里取「第一条有前导空格的
// 行」的空格数：文件是 2 空格就一直 2 空格，是 4 空格就一直 4 空格。取值超过 9 或
// 不是空格（制表符缩进在 YAML 里非法）时退回默认的 2——本仓库模板与手写配置都是 2。
func detectIndent(content []byte) int {
	for _, line := range strings.Split(string(content), "\n") {
		spaces := len(line) - len(strings.TrimLeft(line, " "))
		if spaces == 0 {
			continue
		}
		if spaces <= 9 {
			return spaces
		}
		break
	}
	return 2
}

// marshalFresh 整体序列化一份新文档（文件不存在或旧文件不可解析时的退路）。
func marshalFresh(c *AppConfig) ([]byte, error) {
	out, err := yaml.Marshal(c)
	if err != nil {
		return nil, fmt.Errorf("failed to serialize config: %w", err)
	}
	return out, nil
}

// writeFileAtomic 在目标目录写临时文件（0600）后 rename 覆盖目标，保证读者要么看到
// 旧文件要么看到新文件，不会读到写了一半的内容；权限由临时文件决定，因此即便原文件
// 是 0644，覆盖后也是 mode。
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir, base := filepath.Dir(path), filepath.Base(path)
	tmp, err := os.CreateTemp(dir, "."+base+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		if tmpName != "" {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	tmpName = "" // rename 成功后临时文件已不存在，不需要再清理
	return nil
}

// ---------- 合并写回：保留注释、键顺序与未知字段 ----------

// yamlStrTag 是 yaml.v3 里字符串标量的标签（沿用文件里的引号风格时用它判断类型）。
const yamlStrTag = "!!str"

// legacyAliasKeys 是需要从文件里清除的旧字段名：映射路径 → {别名: 规范字段名}。
// 只有当同一映射里已经写入了规范字段时别名才会被删除，避免出现新旧两份取值。
var legacyAliasKeys = map[string]map[string]string{
	"failover": {"max_retries": "max_failover_channels"},
}

// sequenceIdentityKeys 是需要按身份键逐元素合并的序列：映射路径 → 身份键。
// 按身份合并才能保住每个元素上的注释；未列出的序列（models、proxy_tokens、
// blocked_tools 等）整体替换元素，但保留序列自身的注释。
var sequenceIdentityKeys = map[string]string{
	"channels":     "id",
	"mcp_servers":  "id",
	"model_prices": "model",
}

// configSchema 描述配置结构在某一层允许出现的键。
//
// 由 AppConfig 的 yaml tag 反射生成（新增字段无需同步维护），用途只有一个：
// 判断「文件里有、但本次配置没写出」的键该不该删除。属于 schema 的键说明配置侧
// 已经把它清空（例如 omitempty 的 auth_token 被清掉），必须删除，否则下一次加载
// 会把旧值复活；不属于 schema 的键是用户手写或更新版本添加的字段，必须原样保留。
type configSchema struct {
	keys    map[string]*configSchema // 映射键 → 子结构
	element *configSchema            // 序列元素的结构
	isMap   bool                     // map 类型：键由用户定义，内容完全以配置为准
}

// appConfigSchema 是 AppConfig 的 YAML 结构描述（进程内只构建一次）。
var appConfigSchema = buildSchema(reflect.TypeOf(AppConfig{}), map[reflect.Type]*configSchema{})

func buildSchema(t reflect.Type, seen map[reflect.Type]*configSchema) *configSchema {
	if s, ok := seen[t]; ok {
		return s
	}
	switch t.Kind() {
	case reflect.Ptr:
		s := buildSchema(t.Elem(), seen)
		seen[t] = s
		return s
	case reflect.Map:
		s := &configSchema{isMap: true}
		seen[t] = s
		return s
	case reflect.Slice, reflect.Array:
		s := &configSchema{element: buildSchema(t.Elem(), seen)}
		seen[t] = s
		return s
	case reflect.Struct:
		s := &configSchema{keys: make(map[string]*configSchema, t.NumField())}
		seen[t] = s // 先登记，避免自引用类型无限递归
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if f.PkgPath != "" {
				continue // 未导出字段（如 maxRetriesAlias）不参与 YAML 序列化
			}
			name, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
			if name == "" || name == "-" {
				continue
			}
			s.keys[name] = buildSchema(f.Type, seen)
		}
		return s
	default:
		// 标量：没有子键。
		s := &configSchema{}
		seen[t] = s
		return s
	}
}

func (s *configSchema) child(key string) *configSchema {
	if s == nil || s.keys == nil {
		return nil
	}
	return s.keys[key]
}

// mergeIntoDocument 把配置合并进既有文档，返回合并后的 YAML 字节。
// indent 是既有文档的缩进宽度（见 detectIndent），用于保持排版不变。
func mergeIntoDocument(existing []byte, c *AppConfig, indent int) ([]byte, error) {
	dst, dstRoot, err := parseDocument(existing)
	if err != nil {
		return nil, err
	}
	_, srcRoot, err := documentOf(c)
	if err != nil {
		return nil, err
	}
	mergeMapping(dstRoot, srcRoot, appConfigSchema, "")
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(indent)
	if err := enc.Encode(dst); err != nil {
		return nil, fmt.Errorf("failed to serialize config: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("failed to serialize config: %w", err)
	}
	return buf.Bytes(), nil
}

// parseDocument 把既有文件解析成 YAML 节点树，返回「待编码的节点」与「根映射节点」。
func parseDocument(content []byte) (doc, root *yaml.Node, err error) {
	var node yaml.Node
	if uerr := yaml.Unmarshal(content, &node); uerr != nil {
		return nil, nil, fmt.Errorf("failed to parse existing config: %w", uerr)
	}
	doc = &node
	root = &node
	if node.Kind == yaml.DocumentNode {
		if len(node.Content) == 0 {
			return nil, nil, fmt.Errorf("failed to parse existing config: empty document")
		}
		root = node.Content[0]
	}
	if root.Kind != yaml.MappingNode {
		return nil, nil, fmt.Errorf("failed to parse existing config: root is not a mapping")
	}
	return doc, root, nil
}

// documentOf 把配置序列化成节点树（同样返回待编码节点与根映射节点）。
func documentOf(c *AppConfig) (doc, root *yaml.Node, err error) {
	raw, err := marshalFresh(c)
	if err != nil {
		return nil, nil, err
	}
	return parseDocument(raw)
}

// mergeMapping 就地把 src 映射的取值合并进 dst 映射：同名键递归合并（保留 dst 的键
// 节点、注释与样式），配置里新增的键追加到映射末尾，其余键的原有顺序不变。
func mergeMapping(dst, src *yaml.Node, schema *configSchema, path string) {
	if schema == nil || schema.isMap {
		// 键完全由配置决定的映射（model_mapping、custom_headers 等）：内容以配置为准，
		// 但保留映射节点自身的注释与样式。
		dst.Content = src.Content
		return
	}
	index := make(map[string]int, len(dst.Content)/2)
	for i := 0; i+1 < len(dst.Content); i += 2 {
		index[dst.Content[i].Value] = i
	}
	written := make(map[string]bool, len(src.Content)/2)
	for i := 0; i+1 < len(src.Content); i += 2 {
		key, value := src.Content[i], src.Content[i+1]
		written[key.Value] = true
		if at, ok := index[key.Value]; ok {
			mergeNode(dst.Content[at+1], value, schema.child(key.Value), joinPath(path, key.Value))
			continue
		}
		dst.Content = append(dst.Content, key, value)
	}
	kept := make([]*yaml.Node, 0, len(dst.Content))
	for i := 0; i+1 < len(dst.Content); i += 2 {
		if shouldDropKey(schema, path, dst.Content[i].Value, written) {
			continue
		}
		kept = append(kept, dst.Content[i], dst.Content[i+1])
	}
	dst.Content = kept
}

// shouldDropKey 判断「文件里有、但配置没有写出」的键是否应从文件中删除。
func shouldDropKey(schema *configSchema, path, key string, written map[string]bool) bool {
	if written[key] {
		return false
	}
	if canonical, ok := legacyAliasKeys[path][key]; ok {
		// 旧字段别名（max_retries）：规范字段已写出时删除，否则保留，不丢用户数据。
		return written[canonical]
	}
	if schema == nil || schema.keys == nil {
		return false
	}
	_, known := schema.keys[key]
	return known
}

// mergeNode 就地把 src 的取值合并进 dst，保留 dst 的注释、锚点与（可继续沿用的）样式。
func mergeNode(dst, src *yaml.Node, schema *configSchema, path string) {
	switch {
	case dst.Kind == yaml.MappingNode && src.Kind == yaml.MappingNode:
		mergeMapping(dst, src, schema, path)
	case dst.Kind == yaml.SequenceNode && src.Kind == yaml.SequenceNode:
		mergeSequence(dst, src, schema, path)
	case dst.Kind == yaml.ScalarNode && src.Kind == yaml.ScalarNode:
		mergeScalar(dst, src)
	default:
		// 形状变了（标量 ↔ 映射/序列、null ↔ 映射……）：整体采用新形状，保留注释。
		adoptNode(dst, src)
	}
}

// mergeScalar 用 src 的取值替换 dst 的取值，保留 dst 的注释；文件里原本用引号写的
// 字符串，在新值同样是字符串时沿用引号风格（例如 listen_host: "0.0.0.0"）。
func mergeScalar(dst, src *yaml.Node) {
	style := src.Style
	if src.Tag == yamlStrTag && dst.Style&(yaml.SingleQuotedStyle|yaml.DoubleQuotedStyle) != 0 {
		style = dst.Style & (yaml.SingleQuotedStyle | yaml.DoubleQuotedStyle)
	}
	dst.Tag = src.Tag
	dst.Value = src.Value
	dst.Style = style
}

// adoptNode 让 dst 采用 src 的形状（类型发生变化时使用），保留 dst 上的注释与锚点。
func adoptNode(dst, src *yaml.Node) {
	head, line, foot, anchor := dst.HeadComment, dst.LineComment, dst.FootComment, dst.Anchor
	*dst = *src
	dst.HeadComment, dst.LineComment, dst.FootComment, dst.Anchor = head, line, foot, anchor
}

// mergeSequence 合并序列。有身份键的序列（channels/mcp_servers/model_prices）按身份
// 就地合并，配置里新增的元素追加到末尾，配置里删掉的元素从文件中移除（元素自身的
// 注释跟着元素走）；其余序列整体替换元素，只保留序列自身的注释。
func mergeSequence(dst, src *yaml.Node, schema *configSchema, path string) {
	if schema == nil {
		schema = &configSchema{}
	}
	identity := sequenceIdentityKeys[path]
	if identity == "" {
		dst.Content = src.Content
		dst.Style = src.Style // 空序列保持 `[]` 这种 flow 写法
		return
	}
	index := make(map[string]int, len(dst.Content))
	for i, item := range dst.Content {
		index[identityValue(item, identity)] = i
	}
	merged := make([]bool, len(dst.Content))
	for _, item := range src.Content {
		if at, ok := index[identityValue(item, identity)]; ok {
			mergeNode(dst.Content[at], item, schema.element, joinPath(path, "[]"))
			merged[at] = true
			continue
		}
		dst.Content = append(dst.Content, item)
	}
	kept := make([]*yaml.Node, 0, len(dst.Content))
	for i, item := range dst.Content {
		if i < len(merged) && !merged[i] {
			continue // 配置里已删除的元素
		}
		kept = append(kept, item)
	}
	dst.Content = kept
}

// identityValue 取序列元素身份键的取值；元素不是映射或缺少身份键时返回空串
// （与 Parse 后该字段为零值的行为一致，保证两侧仍然能配对）。
func identityValue(item *yaml.Node, key string) string {
	if item == nil || item.Kind != yaml.MappingNode {
		return ""
	}
	for i := 0; i+1 < len(item.Content); i += 2 {
		if item.Content[i].Value == key {
			return item.Content[i+1].Value
		}
	}
	return ""
}

func joinPath(parent, key string) string {
	if parent == "" {
		return key
	}
	return parent + "." + key
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
