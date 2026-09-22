package converter

import (
	"reflect"
	"testing"
)

// usage 换算的往返测试。
//
// 为什么需要它：矩阵全开后会出现经 Chat 的两跳（messages→chat→responses 等），
// 于是同一份 usage 会被换算两次。若某个换算不是前一个的逆，客户端看到的
// token 数会与上游真实用量不一致（账单口径不能靠"看起来差不多"）。
//
// 本文件把四组往返的**精确行为**钉住：能无损的必须无损，确实有损的（协议里
// 没有对应字段）必须逐字段写明损失点，而不是笼统地"差不多"。
//
// 口径回顾（common.go 顶部）：Chat / Responses 的 prompt/input tokens **含**缓存
// 命中，Anthropic 的 input_tokens **不含**（cache_read / cache_creation 单列）。
// 注意：审计与成本始终取**上游原始流**（audit.ExtractTokensFromStream），
// 因此这里的换算损失只影响「转换后响应体里给客户端看的 usage」，不影响计费。

func TestResponsesToChatToResponsesIsLossless(t *testing.T) {
	in := map[string]any{
		"input_tokens":          100,
		"output_tokens":         20,
		"total_tokens":          120,
		"input_tokens_details":  map[string]any{"cached_tokens": 30},
		"output_tokens_details": map[string]any{"reasoning_tokens": 8},
	}
	got := chatUsageToResponses(responsesUsageToChat(in))
	if !reflect.DeepEqual(got, in) {
		t.Fatalf("responses → chat → responses 应无损\n got=%#v\nwant=%#v", got, in)
	}
}

func TestChatToResponsesToChatIsLossyOnlyForCacheCreation(t *testing.T) {
	in := map[string]any{
		"prompt_tokens":     57,
		"completion_tokens": 5,
		"total_tokens":      62,
		"prompt_tokens_details": map[string]any{
			"cached_tokens":         40,
			"cache_creation_tokens": 7,
		},
	}
	got := responsesUsageToChat(chatUsageToResponses(in))

	// Responses 没有"缓存写入"这个概念，chatUsageToResponses 只带 cached_tokens，
	// 因此 cache_creation_tokens 在这一跳被丢弃；其余字段无损。
	want := map[string]any{
		"prompt_tokens":     57,
		"completion_tokens": 5,
		"total_tokens":      62,
		"prompt_tokens_details": map[string]any{
			"cached_tokens": 40,
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("chat → responses → chat\n got=%#v\nwant=%#v", got, want)
	}
}

func TestAnthropicToChatToAnthropicFoldsCacheCreationIntoInput(t *testing.T) {
	in := map[string]any{
		"input_tokens":                10,
		"cache_read_input_tokens":     40,
		"cache_creation_input_tokens": 7,
		"output_tokens":               5,
	}
	// anthropicUsageToChat: prompt = input + cache_read + cache_creation = 57
	mid := anthropicUsageToChat(in)
	wantMid := map[string]any{
		"prompt_tokens":     57,
		"completion_tokens": 5,
		"total_tokens":      62,
		"prompt_tokens_details": map[string]any{
			"cached_tokens":         40,
			"cache_creation_tokens": 7,
		},
	}
	if !reflect.DeepEqual(mid, wantMid) {
		t.Fatalf("anthropic → chat\n got=%#v\nwant=%#v", mid, wantMid)
	}

	// chatUsageToAnthropic 只扣 cached_tokens，因此 cache_creation 被并回 input_tokens：
	// 10（原始 input）+ 7（cache_creation）= 17。这是两跳里有损的一处，必须写明。
	got := chatUsageToAnthropic(mid)
	want := map[string]any{
		"input_tokens":            17,
		"output_tokens":           5,
		"cache_read_input_tokens": 40,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("anthropic → chat → anthropic\n got=%#v\nwant=%#v", got, want)
	}
}

func TestChatToAnthropicToChatNormalizesTotal(t *testing.T) {
	in := map[string]any{
		"prompt_tokens":     57,
		"completion_tokens": 5,
		"total_tokens":      999, // 上游给的 total 与 input+output 不一致
		"prompt_tokens_details": map[string]any{
			"cached_tokens": 40,
		},
	}
	got := anthropicUsageToChat(chatUsageToAnthropic(in))

	// Anthropic 形态没有 total_tokens 字段，回来时按 prompt+completion 重算，
	// 因此上游异常的 total 会被归一化（其余字段无损）。
	want := map[string]any{
		"prompt_tokens":     57,
		"completion_tokens": 5,
		"total_tokens":      62,
		"prompt_tokens_details": map[string]any{
			"cached_tokens": 40,
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("chat → anthropic → chat\n got=%#v\nwant=%#v", got, want)
	}
}

// TestChatUsageToAnthropicOnNilReturnsZeros 固定一个刻意的行为：
// Anthropic 的 message_start 必须带 usage 对象，因此 nil 输入要返回零值对象，
// 而不是 nil（见 stream.go 的 ensureStarted 与不变式 I12）。
func TestChatUsageToAnthropicOnNilReturnsZeros(t *testing.T) {
	got := chatUsageToAnthropic(nil)
	want := map[string]any{"input_tokens": 0, "output_tokens": 0}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("chatUsageToAnthropic(nil) = %#v, want %#v", got, want)
	}

	// 其余三个换算对 nil 一律返回 nil（调用方据此省略整个 usage 对象）。
	if got := anthropicUsageToChat(nil); got != nil {
		t.Fatalf("anthropicUsageToChat(nil) = %#v, want nil", got)
	}
	if got := chatUsageToResponses(nil); got != nil {
		t.Fatalf("chatUsageToResponses(nil) = %#v, want nil", got)
	}
	if got := responsesUsageToChat(nil); got != nil {
		t.Fatalf("responsesUsageToChat(nil) = %#v, want nil", got)
	}
}

// TestChatUsageToAnthropicClampsNegativeInput 固定零下限：上游若给出
// cached_tokens > prompt_tokens（异常数据），input_tokens 不能变成负数。
func TestChatUsageToAnthropicClampsNegativeInput(t *testing.T) {
	got := chatUsageToAnthropic(map[string]any{
		"prompt_tokens":         10,
		"completion_tokens":     2,
		"prompt_tokens_details": map[string]any{"cached_tokens": 25},
	})
	if input := intField(got, "input_tokens"); input != 0 {
		t.Fatalf("input_tokens = %d, want 0（不得为负）", input)
	}
	if read := intField(got, "cache_read_input_tokens"); read != 25 {
		t.Fatalf("cache_read_input_tokens = %d, want 25（缓存命中量照实保留）", read)
	}
}
