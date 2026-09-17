package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 写回行为的回归测试：这些用例断言磁盘上的「原始字节」，而不是复述实现。
// 背景：旧实现用 yaml.Marshal 整体重写文件，用户的注释模板、手写字段会被抹掉，
// 权限还会被改成 0644（文件里有 API key / 管理令牌）。

func mustWriteFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatalf("写入测试文件失败: %v", err)
	}
}

func mustReadFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取文件失败: %v", err)
	}
	return string(raw)
}

func mustMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat 失败: %v", err)
	}
	return info.Mode().Perm()
}

// assertOrder 断言 needle 在 text 中按给定顺序出现。
func assertOrder(t *testing.T, text string, needles ...string) {
	t.Helper()
	prev, prevName := -1, ""
	for _, n := range needles {
		at := strings.Index(text, n)
		if at < 0 {
			t.Fatalf("找不到 %q\n%s", n, text)
		}
		if at < prev {
			t.Fatalf("顺序错误：%q 出现在 %q 之前\n%s", n, prevName, text)
		}
		prev, prevName = at, n
	}
}

// 带注释、未知字段、引号标量与旧别名的既有文件。
const annotatedFixture = `# Model Bridge 配置文件（用户横幅）
# 第二行横幅：请勿删除
listen_port: 8080
listen_host: "0.0.0.0"

# 鉴权配置
auth:
  # 代理令牌
  proxy_tokens: []
  # 管理令牌
  admin_token: null

# 渠道列表
channels:
  - id: "ch-1"
    name: "One"
    provider: openai
    endpoint:
      url: "https://example.com/v1/chat/completions"
    api_key: "sk-1"
    priority: 1  # 渠道优先级
    enabled: true
    fallback_model: "gpt-4o-mini"

# 故障转移
failover:
  # 候选数量上限
  retry_timeout_ms: 5000
  max_retries: 9
  # 手写字段：本版本不认识
  future_option: 42
  circuit_breaker:
    failure_threshold: 3
    recovery_interval_sec: 30
    probe_requests: 2

# 别名模型
models:
  - gpt-4o
`

func TestSaveToFilePreservesExistingDocument(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	mustWriteFile(t, path, annotatedFixture, 0o644)

	cfg, err := LoadFromFile(path)
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	if cfg.ListenPort != 8080 || cfg.ListenHost != "0.0.0.0" {
		t.Fatalf("加载结果不正确: port=%d host=%q", cfg.ListenPort, cfg.ListenHost)
	}
	if cfg.Failover.MaxFailoverChannels != 9 {
		t.Fatalf("旧字段 max_retries 未生效: %d", cfg.Failover.MaxFailoverChannels)
	}

	cfg.ListenPort = 18081
	cfg.Failover.MaxFailoverChannels = 5
	cfg.Models = []string{"gpt-4o", "claude-sonnet-4-6"}
	cfg.Channels = append(cfg.Channels, ChannelConfig{
		ID:            "ch-2",
		Name:          "Two",
		Provider:      ProviderAnthropic,
		Endpoint:      EndpointConfig{URL: "https://example.com/v1/messages"},
		APIKey:        "sk-2",
		Priority:      2,
		Enabled:       true,
		FallbackModel: "claude-sonnet-4-6",
		ModelMapping:  map[string]string{},
		CustomHeaders: map[string]string{},
		TimeoutMs:     30000,
	})
	if err := cfg.SaveToFile(path); err != nil {
		t.Fatalf("保存配置失败: %v", err)
	}

	raw := mustReadFile(t, path)

	// 1) 注释全部保留：横幅、分节注释、字段上方注释、行内注释。
	for _, want := range []string{
		"# Model Bridge 配置文件（用户横幅）",
		"# 第二行横幅：请勿删除",
		"# 鉴权配置",
		"# 代理令牌",
		"# 管理令牌",
		"# 渠道列表",
		"# 渠道优先级",
		"# 故障转移",
		"# 候选数量上限",
		"# 手写字段：本版本不认识",
		"# 别名模型",
	} {
		if !strings.Contains(raw, want) {
			t.Errorf("注释丢失 %q\n%s", want, raw)
		}
	}

	// 2) 版本不认识的键必须原样保留。
	if !strings.Contains(raw, "future_option: 42") {
		t.Errorf("未知字段 future_option 被丢弃\n%s", raw)
	}

	// 3) 真正变化的取值被就地替换。
	if !strings.Contains(raw, "listen_port: 18081") {
		t.Errorf("listen_port 未更新\n%s", raw)
	}
	if !strings.Contains(raw, "max_failover_channels: 5") {
		t.Errorf("max_failover_channels 未写入\n%s", raw)
	}

	// 4) 旧别名必须消失（不能新旧两份并存）。
	if strings.Contains(raw, "max_retries") {
		t.Errorf("旧字段名 max_retries 仍在文件中\n%s", raw)
	}

	// 5) 引号风格保留、序列按内容替换。
	if !strings.Contains(raw, `listen_host: "0.0.0.0"`) {
		t.Errorf("引号风格被改写\n%s", raw)
	}
	if !strings.Contains(raw, "- claude-sonnet-4-6") {
		t.Errorf("models 未更新\n%s", raw)
	}
	if !strings.Contains(raw, "id: ch-2") {
		t.Errorf("新增渠道未写入\n%s", raw)
	}
	if !strings.Contains(raw, `id: "ch-1"`) {
		t.Errorf("既有渠道被改写\n%s", raw)
	}

	// 6) 原有键的相对顺序不变。
	assertOrder(t, raw,
		"# Model Bridge 配置文件（用户横幅）",
		"listen_port:",
		"listen_host:",
		"# 鉴权配置",
		"auth:",
		"# 渠道列表",
		"channels:",
		"# 故障转移",
		"failover:",
		"# 别名模型",
		"models:",
	)

	// 7) 保存后的文件仍然可加载，且取值与内存一致。
	reloaded, err := LoadFromFile(path)
	if err != nil {
		t.Fatalf("重新加载失败: %v\n%s", err, raw)
	}
	if reloaded.ListenPort != 18081 || reloaded.ListenHost != "0.0.0.0" {
		t.Fatalf("重新加载取值不正确: port=%d host=%q", reloaded.ListenPort, reloaded.ListenHost)
	}
	if reloaded.Failover.MaxFailoverChannels != 5 || reloaded.Failover.RetryTimeoutMs != 5000 {
		t.Fatalf("重新加载 failover 不正确: %+v", reloaded.Failover)
	}
	if len(reloaded.Channels) != 2 || reloaded.Channels[1].ID != "ch-2" {
		t.Fatalf("重新加载渠道不正确: %+v", reloaded.Channels)
	}
	if len(reloaded.Models) != 2 || reloaded.Models[1] != "claude-sonnet-4-6" {
		t.Fatalf("重新加载 models 不正确: %v", reloaded.Models)
	}
}

// 文件里原本带引号的字符串，值变化后仍应保持引号风格。
func TestSaveToFileKeepsQuotingOfChangedScalar(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	mustWriteFile(t, path, annotatedFixture, 0o600)

	cfg, err := LoadFromFile(path)
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	cfg.ListenHost = "127.0.0.1" // 原值是 "0.0.0.0"
	if err := cfg.SaveToFile(path); err != nil {
		t.Fatalf("保存配置失败: %v", err)
	}

	raw := mustReadFile(t, path)
	if !strings.Contains(raw, `listen_host: "127.0.0.1"`) {
		t.Fatalf("变化后的字符串未沿用引号风格\n%s", raw)
	}
}

// 序列：新增渠道追加到末尾，被删渠道从文件消失，其他渠道的注释不受影响。
func TestSaveToFileMergesSequences(t *testing.T) {
	const fixture = `# 渠道列表
channels:
  # 渠道 A：保留
  - id: "ch-a"
    name: "A"
    provider: openai
    endpoint:
      url: "https://a.example.com/v1/chat/completions"
    api_key: "sk-a"
    priority: 1
    enabled: true
    fallback_model: "gpt-4o-mini"

  # 渠道 B：将被删除
  - id: "ch-b"
    name: "B"
    provider: openai
    endpoint:
      url: "https://b.example.com/v1/chat/completions"
    api_key: "sk-b"
    priority: 2
    enabled: true
    fallback_model: "gpt-4o-mini"

  # 渠道 C：保留
  - id: "ch-c"
    name: "C"
    provider: anthropic
    endpoint:
      url: "https://c.example.com/v1/messages"
    api_key: "sk-c"  # C 的 key
    priority: 3
    enabled: true
    fallback_model: "claude-sonnet-4-6"
`
	path := filepath.Join(t.TempDir(), "config.yaml")
	mustWriteFile(t, path, fixture, 0o600)

	cfg, err := LoadFromFile(path)
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	if len(cfg.Channels) != 3 {
		t.Fatalf("期望 3 个渠道，实际 %d", len(cfg.Channels))
	}

	// 删除 ch-b、新增 ch-d，并把 ch-c 的优先级改掉。
	kept := make([]ChannelConfig, 0, len(cfg.Channels))
	for _, ch := range cfg.Channels {
		if ch.ID != "ch-b" {
			kept = append(kept, ch)
		}
	}
	cfg.Channels = kept
	for i := range cfg.Channels {
		if cfg.Channels[i].ID == "ch-c" {
			cfg.Channels[i].Priority = 7
		}
	}
	cfg.Channels = append(cfg.Channels, ChannelConfig{
		ID:            "ch-d",
		Name:          "D",
		Provider:      ProviderOpenAI,
		Endpoint:      EndpointConfig{URL: "https://d.example.com/v1/chat/completions"},
		APIKey:        "sk-d",
		Priority:      9,
		Enabled:       true,
		FallbackModel: "gpt-4o-mini",
		ModelMapping:  map[string]string{},
		CustomHeaders: map[string]string{},
		TimeoutMs:     30000,
	})
	if err := cfg.SaveToFile(path); err != nil {
		t.Fatalf("保存配置失败: %v", err)
	}

	raw := mustReadFile(t, path)

	if strings.Contains(raw, "ch-b") {
		t.Fatalf("被删除的渠道仍在文件中\n%s", raw)
	}
	if !strings.Contains(raw, "# 渠道 A：保留") || !strings.Contains(raw, "# 渠道 C：保留") {
		t.Fatalf("其他渠道的注释被丢弃\n%s", raw)
	}
	if !strings.Contains(raw, "# C 的 key") {
		t.Fatalf("其他渠道的行内注释被丢弃\n%s", raw)
	}
	if !strings.Contains(raw, "# 渠道列表") {
		t.Fatalf("序列自身的注释被丢弃\n%s", raw)
	}
	if !strings.Contains(raw, "priority: 7") {
		t.Fatalf("ch-c 的改动未落盘\n%s", raw)
	}
	// 既有元素保持原顺序，新元素追加在末尾。
	assertOrder(t, raw, "# 渠道 A：保留", "id: \"ch-a\"", "# 渠道 C：保留", "id: \"ch-c\"", "id: ch-d")

	reloaded, err := LoadFromFile(path)
	if err != nil {
		t.Fatalf("重新加载失败: %v\n%s", err, raw)
	}
	if len(reloaded.Channels) != 3 {
		t.Fatalf("重新加载渠道数不正确: %d", len(reloaded.Channels))
	}
	wantIDs := []string{"ch-a", "ch-c", "ch-d"}
	for i, want := range wantIDs {
		if reloaded.Channels[i].ID != want {
			t.Fatalf("渠道顺序不正确: %v", []string{
				reloaded.Channels[0].ID, reloaded.Channels[1].ID, reloaded.Channels[2].ID,
			})
		}
	}
	if reloaded.Channels[1].Priority != 7 {
		t.Fatalf("ch-c 优先级未落盘: %d", reloaded.Channels[1].Priority)
	}
}

// 权限：覆盖 0644 的老文件后必须是 0600；新建文件同样是 0600，且不留临时文件。
func TestSaveToFilePermissions(t *testing.T) {
	dir := t.TempDir()

	existing := filepath.Join(dir, "config.yaml")
	mustWriteFile(t, existing, "# 用户横幅\nlisten_port: 8080\n", 0o644)
	cfg, err := LoadFromFile(existing)
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	if err := cfg.SaveToFile(existing); err != nil {
		t.Fatalf("保存配置失败: %v", err)
	}
	if got := mustMode(t, existing); got != 0o600 {
		t.Fatalf("覆盖 0644 文件后权限应为 0600，实际 %o", got)
	}
	if !strings.Contains(mustReadFile(t, existing), "# 用户横幅") {
		t.Fatalf("覆盖后注释丢失\n%s", mustReadFile(t, existing))
	}

	// 全新路径（目录也不存在）。
	fresh := filepath.Join(dir, "nested", "config.yaml")
	if err := DefaultConfig().SaveToFile(fresh); err != nil {
		t.Fatalf("保存新配置失败: %v", err)
	}
	if got := mustMode(t, fresh); got != 0o600 {
		t.Fatalf("新建文件权限应为 0600，实际 %o", got)
	}
	if _, err := LoadFromFile(fresh); err != nil {
		t.Fatalf("新建的配置无法加载: %v", err)
	}

	// 原子替换不应留下临时文件。
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读取目录失败: %v", err)
	}
	for _, e := range entries {
		if e.Name() != "config.yaml" && e.Name() != "nested" {
			t.Fatalf("目录里留下了临时文件 %s", e.Name())
		}
	}
}

// 用户遇到的原始回归：带注释的默认模板经过一次「加载 + 改值 + 保存」后注释必须还在。
func TestSaveToFileKeepsDefaultTemplateComments(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	mustWriteFile(t, path, DefaultConfigYAML(), 0o600)

	cfg, err := LoadFromFile(path)
	if err != nil {
		t.Fatalf("加载模板失败: %v", err)
	}
	cfg.ListenPort = 18080
	if err := cfg.SaveToFile(path); err != nil {
		t.Fatalf("保存配置失败: %v", err)
	}

	raw := mustReadFile(t, path)
	for _, want := range []string{
		"# Model Bridge 配置文件",
		"# Debug 模式：打印完整的请求/响应体到控制台（含协议转换前后的对比）",
		"# debug: true",
		"# 鉴权配置（为空则不启用鉴权）",
		"# 代理端点访问令牌（客户端需在 Authorization: Bearer <token> 中携带）",
		"# 管理后台令牌（为空则不启用管理鉴权）",
		`# admin_token: "your-admin-secret"`,
		"# OpenAI Responses API 渠道（直接透传 Responses 格式）",
		"# 单次请求最多尝试几个候选渠道（跨渠道上限）；0 = 不限制。",
		"# 注意：单渠道内的重试次数由 channels[].retry_count 控制，二者独立。",
		"# 模型定价表（按实际模型精确匹配），用于成本计算。单价单位：美元 / 百万 token。",
		"# MCP 中继：转发到上游 MCP server 并按黑名单裁剪工具",
		`#     auth_token: "${MCP_TOKEN}"`,
		`#       - "unused_tool_b"`,
		`- id: "openai-primary"`,
		`- id: "anthropic-main"`,
		"max_failover_channels: 3",
		"listen_port: 18080",
	} {
		if !strings.Contains(raw, want) {
			t.Errorf("模板内容丢失 %q\n%s", want, raw)
		}
	}
	if strings.Contains(raw, "max_retries") {
		t.Errorf("不应写入旧字段名\n%s", raw)
	}

	reloaded, err := LoadFromFile(path)
	if err != nil {
		t.Fatalf("重新加载失败: %v", err)
	}
	if reloaded.ListenPort != 18080 || len(reloaded.Channels) != 2 ||
		reloaded.Failover.MaxFailoverChannels != 3 || reloaded.Failover.CircuitBreaker.ProbeRequests != 2 {
		t.Fatalf("模板重新加载结果不正确: port=%d channels=%d fo=%+v",
			reloaded.ListenPort, len(reloaded.Channels), reloaded.Failover)
	}
}

// 旧文件不可解析时：保存必须成功、覆盖成一份可加载的新文档，而不是把文件截断。
func TestSaveToFileFallsBackWhenExistingFileIsUnparsable(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{"truncated-flow-sequence", "listen_port: [8080\n"},
		{"scalar-root", "just a string\n"},
		{"sequence-root", "- a\n- b\n"},
		{"empty-file", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			mustWriteFile(t, path, tc.content, 0o644)

			cfg := DefaultConfig()
			cfg.ListenPort = 1234
			if err := cfg.SaveToFile(path); err != nil {
				t.Fatalf("保存失败: %v", err)
			}
			out := mustReadFile(t, path)
			if !strings.Contains(out, "listen_port: 1234") {
				t.Fatalf("未写入完整配置\n%s", out)
			}
			reloaded, err := LoadFromFile(path)
			if err != nil {
				t.Fatalf("保存后的文件无法加载: %v\n%s", err, out)
			}
			if reloaded.ListenPort != 1234 {
				t.Fatalf("重新加载取值不正确: %d", reloaded.ListenPort)
			}
			if got := mustMode(t, path); got != 0o600 {
				t.Fatalf("权限应为 0600，实际 %o", got)
			}
		})
	}
}

// 版本不认识的键在任何层级都要原样保留（顶层、已知映射内部都算）。
func TestSaveToFilePreservesUnknownKeysEverywhere(t *testing.T) {
	const fixture = `# 顶层注释
listen_port: 8080
hand_written_top: "keep-me"
try_this_later:
  nested_unknown: 7
failover:
  retry_timeout_ms: 5000
  future_option: 42
  circuit_breaker:
    failure_threshold: 3
    experimental_probe: true
    recovery_interval_sec: 30
    probe_requests: 2
`
	path := filepath.Join(t.TempDir(), "config.yaml")
	mustWriteFile(t, path, fixture, 0o600)

	cfg, err := LoadFromFile(path)
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	cfg.Failover.CircuitBreaker.ProbeRequests = 4
	if err := cfg.SaveToFile(path); err != nil {
		t.Fatalf("保存配置失败: %v", err)
	}

	raw := mustReadFile(t, path)
	for _, want := range []string{
		"# 顶层注释",
		`hand_written_top: "keep-me"`,
		"nested_unknown: 7",
		"future_option: 42",
		"experimental_probe: true",
		"probe_requests: 4",
	} {
		if !strings.Contains(raw, want) {
			t.Errorf("丢失 %q\n%s", want, raw)
		}
	}
	assertOrder(t, raw, "listen_port:", "hand_written_top:", "try_this_later:", "failover:")
	assertOrder(t, raw, "retry_timeout_ms:", "future_option:", "circuit_breaker:", "probe_requests:")
}

// schema 里已知、但配置已清空的字段必须从文件中删除，否则旧值会在重启后复活
// （例如清掉 MCP 的 auth_token / oauth 后，文件里还留着旧的令牌）。
func TestSaveToFileDropsClearedKnownFields(t *testing.T) {
	const fixture = `mcp_servers:
  # M1 保留注释
  - id: "m1"
    name: "M1"
    endpoint: "https://m.example.com/mcp"
    auth_token: "tok-old"
    enabled: true
    oauth_enabled: true
    oauth:
      client_id: "old-client"
      access_token: "old-token"
`
	path := filepath.Join(t.TempDir(), "config.yaml")
	mustWriteFile(t, path, fixture, 0o600)

	cfg, err := LoadFromFile(path)
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	if cfg.McpServers[0].AuthToken == nil || cfg.McpServers[0].OAuth == nil {
		t.Fatalf("前置条件不成立: %+v", cfg.McpServers[0])
	}
	cfg.McpServers[0].AuthToken = nil
	cfg.McpServers[0].OAuth = nil
	cfg.McpServers[0].OAuthEnabled = false
	if err := cfg.SaveToFile(path); err != nil {
		t.Fatalf("保存配置失败: %v", err)
	}

	raw := mustReadFile(t, path)
	for _, gone := range []string{"tok-old", "old-client", "old-token", "auth_token", "oauth:"} {
		if strings.Contains(raw, gone) {
			t.Errorf("已清空的字段仍然留在文件里: %q\n%s", gone, raw)
		}
	}
	for _, want := range []string{"# M1 保留注释", `id: "m1"`, "oauth_enabled: false"} {
		if !strings.Contains(raw, want) {
			t.Errorf("丢失 %q\n%s", want, raw)
		}
	}

	reloaded, err := LoadFromFile(path)
	if err != nil {
		t.Fatalf("重新加载失败: %v", err)
	}
	if reloaded.McpServers[0].AuthToken != nil || reloaded.McpServers[0].OAuth != nil {
		t.Fatalf("清空的字段被复活: %+v", reloaded.McpServers[0])
	}
}

// model_prices 按 model 身份合并：改价保留元素注释，删条目从文件消失，新条目追加。
func TestSaveToFileMergesModelPricesByModel(t *testing.T) {
	const fixture = `# 定价表
model_prices:
  # gpt-4o 定价
  - model: "gpt-4o"
    input_per_mtok: 2.5
    output_per_mtok: 10
    enabled: true
  # 将被删除
  - model: "old-model"
    input_per_mtok: 1
    output_per_mtok: 1
    enabled: true
`
	path := filepath.Join(t.TempDir(), "config.yaml")
	mustWriteFile(t, path, fixture, 0o600)

	cfg, err := LoadFromFile(path)
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	if len(cfg.ModelPrices) != 2 {
		t.Fatalf("期望 2 条定价，实际 %d", len(cfg.ModelPrices))
	}
	cfg.ModelPrices = []ModelPrice{{
		Model:         "gpt-4o",
		InputPerMtok:  3.0,
		OutputPerMtok: 12,
		Enabled:       true,
	}, {
		Model:         "new-model",
		InputPerMtok:  0.5,
		OutputPerMtok: 1.5,
		Enabled:       true,
	}}
	if err := cfg.SaveToFile(path); err != nil {
		t.Fatalf("保存配置失败: %v", err)
	}

	raw := mustReadFile(t, path)
	if strings.Contains(raw, "old-model") {
		t.Fatalf("被删除的定价仍在文件中\n%s", raw)
	}
	for _, want := range []string{"# 定价表", "# gpt-4o 定价", `model: "gpt-4o"`, "input_per_mtok: 3", "new-model"} {
		if !strings.Contains(raw, want) {
			t.Errorf("丢失 %q\n%s", want, raw)
		}
	}
	assertOrder(t, raw, "# 定价表", "# gpt-4o 定价", `model: "gpt-4o"`, "new-model")

	reloaded, err := LoadFromFile(path)
	if err != nil {
		t.Fatalf("重新加载失败: %v\n%s", err, raw)
	}
	if len(reloaded.ModelPrices) != 2 || reloaded.ModelPrices[0].InputPerMtok != 3 ||
		reloaded.ModelPrices[1].Model != "new-model" {
		t.Fatalf("重新加载定价不正确: %+v", reloaded.ModelPrices)
	}
}

// 保存两次必须稳定（幂等）：第二次写回不应再改动文件内容。
func TestSaveToFileIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	mustWriteFile(t, path, DefaultConfigYAML(), 0o600)

	cfg, err := LoadFromFile(path)
	if err != nil {
		t.Fatalf("加载模板失败: %v", err)
	}
	cfg.ListenPort = 18080
	if err := cfg.SaveToFile(path); err != nil {
		t.Fatalf("首次保存失败: %v", err)
	}
	first := mustReadFile(t, path)

	again, err := LoadFromFile(path)
	if err != nil {
		t.Fatalf("重新加载失败: %v", err)
	}
	if err := again.SaveToFile(path); err != nil {
		t.Fatalf("二次保存失败: %v", err)
	}
	if second := mustReadFile(t, path); second != first {
		t.Fatalf("二次保存改动了文件内容\n--- 第一次 ---\n%s\n--- 第二次 ---\n%s", first, second)
	}
}
