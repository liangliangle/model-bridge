// Package app 持有运行期共享状态，对应 Rust 侧的 `proxy::server::AppState`。
package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"modelbridge/internal/audit"
	"modelbridge/internal/channel"
	"modelbridge/internal/config"
)

// State 是进程内共享的运行期状态。
//
// 配置采用「写时复制」：任何修改都克隆一份、改克隆、落盘成功后再整体替换指针。
// 因此读者通过 Config() 拿到的快照在整段请求处理期间都是自洽的，
// 不必为每次字段读取加锁（对应 Rust `Arc<RwLock<AppConfig>>` + `read().clone()`）。
type State struct {
	mu         sync.RWMutex
	cfg        *config.AppConfig
	configPath string

	Health *channel.HealthMap
	Audit  *audit.DB
	// HTTP 不做整体超时：超时由每个请求的 context 控制（流式请求可达 30 分钟）。
	HTTP *http.Client
}

// New 创建运行期状态。
func New(cfg *config.AppConfig, configPath string, db *audit.DB) *State {
	ids := make([]string, 0, len(cfg.Channels))
	for i := range cfg.Channels {
		ids = append(ids, cfg.Channels[i].ID)
	}
	return &State{
		cfg:        cfg,
		configPath: configPath,
		Health:     channel.NewHealthMap(ids),
		Audit:      db,
		HTTP:       NewHTTPClient(),
	}
}

// NewHTTPClient 构造共享的 HTTP 客户端。
func NewHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   16,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: time.Second,
			ForceAttemptHTTP2:     true,
		},
	}
}

// Config 返回当前配置快照。调用方必须视为只读，不得修改。
func (s *State) Config() *config.AppConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

// Snapshot 是 Config 的别名，用于满足 mcp.ConfigStore 接口的只读快照语义。
func (s *State) Snapshot() *config.AppConfig { return s.Config() }

// ConfigPath 返回配置文件路径。
func (s *State) ConfigPath() string { return s.configPath }

// Update 在写锁内克隆配置、执行修改、落盘，成功后替换快照。
// 修改函数返回错误时不会落盘也不会替换，配置保持原样。
func (s *State) Update(fn func(*config.AppConfig) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	next, err := cloneConfig(s.cfg)
	if err != nil {
		return fmt.Errorf("clone config: %w", err)
	}
	if err := fn(next); err != nil {
		return err
	}
	if err := next.SaveToFile(s.configPath); err != nil {
		return err
	}
	s.cfg = next
	return nil
}

// Replace 直接替换配置并落盘（用于导入/重置场景）。
func (s *State) Replace(cfg *config.AppConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := cfg.SaveToFile(s.configPath); err != nil {
		return err
	}
	s.cfg = cfg
	return nil
}

// SyncHealth 让健康表与当前渠道集合对齐：新渠道补建、已删除渠道移除。
func (s *State) SyncHealth() {
	cfg := s.Config()
	for i := range cfg.Channels {
		s.Health.Ensure(cfg.Channels[i].ID)
	}
}

// cloneConfig 通过 JSON 往返克隆配置。
//
// 之所以不用手写 Clone：配置字段全部带 json tag（`EndpointConfig` 还实现了
// `UnmarshalJSON` 兼容旧写法），往返能覆盖嵌套结构与指针字段，且配置改动只在
// 管理 API 路径上发生，频率极低，开销可以接受。
func cloneConfig(cfg *config.AppConfig) (*config.AppConfig, error) {
	raw, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	var out config.AppConfig
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Close 释放运行期资源。
func (s *State) Close() {
	if s.Audit != nil {
		_ = s.Audit.Close()
	}
}
