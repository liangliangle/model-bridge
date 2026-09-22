package channel

import (
	"strings"
	"testing"

	"modelbridge/internal/config"
	"modelbridge/internal/converter"
)

// 回归测试：渠道候选选择（SelectRoutes）。
// 与 Rust `src-tauri/tests/test_channel_selection.rs` 逐条对应。

func testChannel(id string, provider config.ProviderType, priority uint32) config.ChannelConfig {
	return config.ChannelConfig{
		ID:            id,
		Name:          id,
		Provider:      provider,
		Endpoint:      config.EndpointConfig{URL: "http://example.invalid/v1/x"},
		Priority:      priority,
		Enabled:       true,
		FallbackModel: "fallback-model",
		ModelMapping:  map[string]string{},
		TimeoutMs:     1000,
		AutoCache:     true,
	}
}

func testConfig(channels []config.ChannelConfig, limit uint32) *config.AppConfig {
	cfg := config.DefaultConfig()
	cfg.Channels = channels
	cfg.Failover.MaxFailoverChannels = limit
	return cfg
}

func selected(t *testing.T, cfg *config.AppConfig, in converter.ApiFormat) []string {
	t.Helper()
	health := NewHealthMap(nil)
	routes, err := SelectRoutes(cfg, health, "gpt-4o", in)
	if err != nil {
		t.Fatalf("该组合应可路由，却返回错误: %v", err)
	}
	ids := make([]string, 0, len(routes))
	for _, r := range routes {
		ids = append(ids, r.Channel.ID)
	}
	return ids
}

func routeError(t *testing.T, cfg *config.AppConfig, in converter.ApiFormat) *RouteError {
	t.Helper()
	health := NewHealthMap(nil)
	_, err := SelectRoutes(cfg, health, "gpt-4o", in)
	if err == nil {
		t.Fatal("该组合不应可路由，却成功了")
	}
	var re *RouteError
	if !asRouteError(err, &re) {
		t.Fatalf("期望 *RouteError，得到 %T: %v", err, err)
	}
	return re
}

func asRouteError(err error, target **RouteError) bool {
	re, ok := err.(*RouteError)
	if ok {
		*target = re
	}
	return ok
}

func assertEqual(t *testing.T, got, want []string, label string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: 得到 %v，期望 %v", label, got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("%s: 得到 %v，期望 %v", label, got, want)
		}
	}
}

// ==================== 渠道选择：协议不参与 ====================

// TestEveryProtocolCombinationIsRoutable 固定矩阵全开后的选路语义：
// 任意入口协议都能落到任意渠道协议上（转换由 converter 负责），
// 因此候选就是全部健康渠道，顺序**只**由 priority 决定。
func TestEveryProtocolCombinationIsRoutable(t *testing.T) {
	cfg := testConfig([]config.ChannelConfig{
		testChannel("A-anthropic", config.ProviderAnthropic, 1),
		testChannel("B-responses", config.ProviderOpenAIResponses, 2),
		testChannel("C-chat", config.ProviderOpenAI, 3),
	}, 10)

	want := []string{"A-anthropic", "B-responses", "C-chat"}
	for _, in := range []converter.ApiFormat{converter.FormatOpenAIChat, converter.FormatAnthropic, converter.FormatResponses} {
		assertEqual(t, selected(t, cfg, in), want, in.String()+" 入口")
	}
}

// TestNoHealthyChannelIsTheOnlyRoutingFailure 固定失败语义：
// 矩阵全开之后，「有健康渠道却一个都用不上」不再可能，
// 唯一的路由失败原因是没有任何启用且健康的渠道。
func TestNoHealthyChannelIsTheOnlyRoutingFailure(t *testing.T) {
	// 有渠道但全部禁用 → 没有可用渠道。
	disabled := testChannel("A-anthropic", config.ProviderAnthropic, 1)
	disabled.Enabled = false
	cfg := testConfig([]config.ChannelConfig{disabled}, 10)
	re := routeError(t, cfg, converter.FormatOpenAIChat)
	if re.Kind != RouteNoAvailableChannel {
		t.Fatalf("期望 NoAvailableChannel，得到 %+v", re)
	}

	// 熔断/不健康渠道的过滤由熔断相关用例覆盖（见下方 priority 与熔断小节）：
	// 这里只固定「协议不再是失败原因」这一条语义。
}

func TestNoAvailableChannelIsReportedDistinctly(t *testing.T) {
	cfg := testConfig([]config.ChannelConfig{
		testChannel("A-chat", config.ProviderOpenAI, 1),
	}, 10)
	health := NewHealthMap(nil)
	health.Set("A-chat", ChannelHealth{State: Unhealthy})
	_, err := SelectRoutes(cfg, health, "gpt-4o", converter.FormatOpenAIChat)
	var re *RouteError
	if !asRouteError(err, &re) || re.Kind != RouteNoAvailableChannel {
		t.Fatalf("期望 NoAvailableChannel，得到 %v", err)
	}
}

// ==================== 优先级排序（在可用候选内） ====================

func TestPriorityOrdersTheSupportedCandidates(t *testing.T) {
	cfg := testConfig([]config.ChannelConfig{
		testChannel("chat-p3", config.ProviderOpenAI, 3),
		testChannel("chat-p1", config.ProviderOpenAI, 1),
		testChannel("chat-p2", config.ProviderOpenAI, 2),
	}, 10)
	assertEqual(t, selected(t, cfg, converter.FormatOpenAIChat),
		[]string{"chat-p1", "chat-p2", "chat-p3"}, "优先级排序")
}

func TestChatChannelWinsWhenPriorityIsHigher(t *testing.T) {
	cfg := testConfig([]config.ChannelConfig{
		testChannel("openai-p1", config.ProviderOpenAI, 1),
		testChannel("anthropic-p2", config.ProviderAnthropic, 2),
	}, 10)
	// 协议不参与排序，只参与准入：优先级高的 Chat 渠道排在前面。
	assertEqual(t, selected(t, cfg, converter.FormatAnthropic),
		[]string{"openai-p1", "anthropic-p2"}, "协议不参与排序")
}

func TestEqualPriorityKeepsConfigOrder(t *testing.T) {
	cfg := testConfig([]config.ChannelConfig{
		testChannel("first-p5", config.ProviderOpenAI, 5),
		testChannel("second-p5", config.ProviderOpenAI, 5),
	}, 10)
	assertEqual(t, selected(t, cfg, converter.FormatOpenAIChat),
		[]string{"first-p5", "second-p5"}, "相同优先级保持配置顺序")
}

func TestUnhealthyChannelsAreSkipped(t *testing.T) {
	cfg := testConfig([]config.ChannelConfig{
		testChannel("A-chat-p1", config.ProviderOpenAI, 1),
		testChannel("B-chat-p2", config.ProviderOpenAI, 2),
	}, 10)
	health := NewHealthMap(nil)
	health.Set("A-chat-p1", ChannelHealth{State: Unhealthy})
	routes, err := SelectRoutes(cfg, health, "gpt-4o", converter.FormatOpenAIChat)
	if err != nil {
		t.Fatalf("应可路由: %v", err)
	}
	ids := []string{}
	for _, r := range routes {
		ids = append(ids, r.Channel.ID)
	}
	assertEqual(t, ids, []string{"B-chat-p2"}, "熔断渠道被跳过")
}

// ==================== 候选数量上限 ====================

func TestDefaultLimitIsThree(t *testing.T) {
	if got := config.DefaultConfig().Failover.MaxFailoverChannels; got != 3 {
		t.Fatalf("默认候选上限应为 3，得到 %d", got)
	}
}

func TestLegacyMaxRetriesKeyStillLoads(t *testing.T) {
	cfg, err := config.Parse([]byte("failover:\n  max_retries: 7\n"))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if cfg.Failover.MaxFailoverChannels != 7 {
		t.Fatalf("旧字段名应作为别名继续生效，得到 %d", cfg.Failover.MaxFailoverChannels)
	}
}

// 核心 bug 回归：旧实现下 max_retries=0 会清空候选，导致全量 503。
func TestLegacyZeroLimitNoLongerEmptiesCandidates(t *testing.T) {
	cfg, err := config.Parse([]byte("failover:\n  max_retries: 0\n"))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if cfg.Failover.MaxFailoverChannels != 0 {
		t.Fatalf("期望 0，得到 %d", cfg.Failover.MaxFailoverChannels)
	}
	cfg.Channels = []config.ChannelConfig{
		testChannel("a", config.ProviderOpenAI, 1),
		testChannel("b", config.ProviderOpenAI, 2),
	}
	assertEqual(t, selected(t, cfg, converter.FormatOpenAIChat), []string{"a", "b"}, "0 = 不限制")
}

func TestPositiveLimitCapsCandidates(t *testing.T) {
	cfg := testConfig([]config.ChannelConfig{
		testChannel("a", config.ProviderOpenAI, 1),
		testChannel("b", config.ProviderOpenAI, 2),
		testChannel("c", config.ProviderOpenAI, 3),
	}, 2)
	assertEqual(t, selected(t, cfg, converter.FormatOpenAIChat), []string{"a", "b"}, "上限裁剪")
}

func TestLimitIsIndependentFromPerChannelRetryCount(t *testing.T) {
	a := testChannel("a", config.ProviderOpenAI, 1)
	a.RetryCount = 2
	a.RetryDelayMs = 50
	cfg := testConfig([]config.ChannelConfig{a, testChannel("b", config.ProviderOpenAI, 2)}, 1)
	assertEqual(t, selected(t, cfg, converter.FormatOpenAIChat), []string{"a"}, "上限只约束候选数")
	if cfg.Channels[0].RetryCount != 2 {
		t.Fatalf("单渠道重试次数不应被上限影响，得到 %d", cfg.Channels[0].RetryCount)
	}
}

// ==================== 终极兜底：同样不看协议 ====================

func TestUltimateFallbackIgnoresProtocol(t *testing.T) {
	// 正常候选全部不可用（禁用）时，兜底渠道仍应被选中——它的协议与入口无关。
	disabled := testChannel("A-anthropic", config.ProviderAnthropic, 1)
	disabled.Enabled = false
	cfg := testConfig([]config.ChannelConfig{disabled}, 10)
	cfg.UltimateFallback = &config.UltimateFallback{Channel: "A-anthropic"}
	assertEqual(t, selected(t, cfg, converter.FormatOpenAIChat), []string{"A-anthropic"}, "兜底生效（chat 入口 → anthropic 渠道）")
	assertEqual(t, selected(t, cfg, converter.FormatResponses), []string{"A-anthropic"}, "兜底生效（responses 入口 → anthropic 渠道）")
}

// ==================== 错误文案 ====================

func TestRouteErrorMessages(t *testing.T) {
	noChannel := (&RouteError{Kind: RouteNoAvailableChannel}).Error()
	if !strings.Contains(noChannel, "no available channels") {
		t.Fatalf("意外文案: %q", noChannel)
	}
	unsupported := (&RouteError{Kind: RouteUnsupportedProtocol, Input: converter.FormatAnthropic}).Error()
	if !strings.Contains(unsupported, "messages") {
		t.Fatalf("意外文案: %q", unsupported)
	}
}
