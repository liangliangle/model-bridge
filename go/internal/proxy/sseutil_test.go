package proxy

import "testing"

// 首块判定（能不能换渠道）与错误探测的回归测试。
//
// 这两个函数只在上游首块里做判断，决定了「还没写给客户端 → 可以换渠道」还是
// 「已经写了 → 只能把错误暴露给调用方」。矩阵放开后富协议渠道会服务所有入口，
// 因此三种协议的形态都必须认得出来。

func TestDetectSSEErrorInPrefix(t *testing.T) {
	cases := []struct {
		name     string
		raw      string
		wantMsg  string
		wantIsEr bool
	}{
		{
			name:     "SSE event:error 行（非 JSON 载荷）",
			raw:      "event:error\ndata: upstream boom\n\n",
			wantMsg:  "SSE error event: upstream boom",
			wantIsEr: true,
		},
		{
			name:     "event:error 行但没有任何 data 行",
			raw:      "event: error\n\n",
			wantMsg:  "SSE error event detected",
			wantIsEr: true,
		},
		{
			name:     "Chat 顶层 error 对象",
			raw:      `data: {"error":{"message":"invalid api key","type":"invalid_request_error"}}` + "\n\n",
			wantMsg:  "invalid api key",
			wantIsEr: true,
		},
		{
			name:     "Chat error 无 message 时退化为未知错误",
			raw:      `data: {"error":{"type":"server_error"}}` + "\n\n",
			wantMsg:  "Unknown stream error",
			wantIsEr: true,
		},
		{
			name:     "Anthropic type=error（信息在 error.message）",
			raw:      `data: {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}` + "\n\n",
			wantMsg:  "Overloaded",
			wantIsEr: true,
		},
		{
			name:     "Responses type=error（信息在顶层 message）",
			raw:      `data: {"type":"error","code":"rate_limit_exceeded","message":"Rate limit reached","param":null}` + "\n\n",
			wantMsg:  "Rate limit reached",
			wantIsEr: true,
		},
		{
			name:     "Responses response.failed 带 response.error.message",
			raw:      `data: {"type":"response.failed","response":{"id":"resp_1","error":{"code":"server_error","message":"upstream exploded"}}}` + "\n\n",
			wantMsg:  "upstream exploded",
			wantIsEr: true,
		},
		{
			name:     "Responses response.failed 只有 code",
			raw:      `data: {"type":"response.failed","response":{"error":{"code":"server_error"}}}` + "\n\n",
			wantMsg:  "stream failed: server_error",
			wantIsEr: true,
		},
		{
			name:     "Responses response.failed 无 error 细节",
			raw:      `data: {"type":"response.failed","response":{"id":"resp_1"}}` + "\n\n",
			wantMsg:  "Unknown stream error",
			wantIsEr: true,
		},
		{
			name: "正常 Chat 首块不误报",
			raw:  "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"\"}}]}\n\n",
		},
		{
			name: "正常 Anthropic 首块不误报",
			raw:  "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\"}}\n\n",
		},
		{
			name: "正常 Responses 首块不误报",
			raw:  "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\"}}\n\n",
		},
		{
			name: "非 SSE 的普通 JSON 不误报",
			raw:  `{"choices":[{"message":{"content":"hi"}}]}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg, isErr := DetectSSEErrorInPrefix([]byte(tc.raw))
			if isErr != tc.wantIsEr {
				t.Fatalf("isErr = %v, want %v（msg=%q）", isErr, tc.wantIsEr, msg)
			}
			if tc.wantIsEr && msg != tc.wantMsg {
				t.Fatalf("msg = %q, want %q", msg, tc.wantMsg)
			}
		})
	}
}

func TestSSEHasContentOutput(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{
			name: "Chat：仅 role 生命周期帧 → 尚无内容",
			raw:  `data: {"choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}` + "\n\n",
			want: false,
		},
		{
			name: "Chat：正文增量",
			raw:  `data: {"choices":[{"index":0,"delta":{"content":"Hel"}}]}` + "\n\n",
			want: true,
		},
		{
			name: "Chat：空 content 不算内容",
			raw:  `data: {"choices":[{"index":0,"delta":{"content":""}}]}` + "\n\n",
			want: false,
		},
		{
			name: "Chat：推理增量算内容",
			raw:  `data: {"choices":[{"index":0,"delta":{"reasoning_content":"想"}}]}` + "\n\n",
			want: true,
		},
		{
			name: "Chat：工具调用算内容",
			raw:  `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"a\""}}]}}]}` + "\n\n",
			want: true,
		},
		{
			name: "Chat：usage-only chunk（choices 为空）不算内容",
			raw:  `data: {"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":5}}` + "\n\n",
			want: false,
		},
		{
			name: "Anthropic：message_start 不算内容",
			raw:  `data: {"type":"message_start","message":{"id":"msg_1","content":[]}}` + "\n\n",
			want: false,
		},
		{
			name: "Anthropic：content_block_start 不算内容（空块）",
			raw:  `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n",
			want: false,
		},
		{
			name: "Anthropic：content_block_delta 算内容",
			raw:  `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}` + "\n\n",
			want: true,
		},
		{
			name: "Anthropic：thinking_delta 算内容",
			raw:  `data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"嗯"}}` + "\n\n",
			want: true,
		},
		{
			name: "Responses：response.created 不算内容",
			raw:  `data: {"type":"response.created","response":{"id":"resp_1"}}` + "\n\n",
			want: false,
		},
		{
			name: "Responses：output_item.added 不算内容",
			raw:  `data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message"}}` + "\n\n",
			want: false,
		},
		{
			name: "Responses：output_text.delta 算内容",
			raw:  `data: {"type":"response.output_text.delta","item_id":"i1","output_index":0,"content_index":0,"delta":"Hel"}` + "\n\n",
			want: true,
		},
		{
			name: "Responses：空的 output_text.delta 不算内容",
			raw:  `data: {"type":"response.output_text.delta","item_id":"i1","output_index":0,"content_index":0,"delta":""}` + "\n\n",
			want: false,
		},
		{
			name: "Responses：函数参数增量算内容",
			raw:  `data: {"type":"response.function_call_arguments.delta","item_id":"i1","output_index":0,"delta":"{\"loc"}` + "\n\n",
			want: true,
		},
		{
			name: "Responses：推理摘要增量算内容",
			raw:  `data: {"type":"response.reasoning_summary_text.delta","item_id":"i1","output_index":0,"summary_index":0,"delta":"先"}` + "\n\n",
			want: true,
		},
		{
			name: "多协议混合：错误帧在前、内容帧在后仍算有内容",
			raw: "data: {\"type\":\"error\",\"message\":\"x\"}\n\n" +
				"data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n",
			want: true,
		},
		{
			name: "空输入",
			raw:  "",
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := SSEHasContentOutput([]byte(tc.raw)); got != tc.want {
				t.Fatalf("SSEHasContentOutput = %v, want %v（raw=%q）", got, tc.want, tc.raw)
			}
		})
	}
}
