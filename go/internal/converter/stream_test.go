package converter

import (
	"bytes"
	"errors"
	"io"
	"regexp"
	"strings"
	"testing"

	"modelbridge/internal/sse"
)

// 本文件覆盖流式方向：SSE 分帧（internal/sse）、chat → messages、chat → responses、
// 以及三个 replay*（完整响应对象重放为 SSE）。
//
// 断言方式与 Rust 的集成测试一致：喂原始 SSE 字节 → 收集下游字节 → 解析事件断言
// 事件顺序、增量拼接结果与 usage（usage 必须是上游尾部真实值，不能是写死的 0）。

// ==================== 测试辅助 ====================

// chunkedReader 按 sizes 指定的分片大小依次返回 data（循环使用），
// 用来把同一段上游字节流按任意 chunk 边界切开喂给 sse.Scan / ConvertStreamResponse。
type chunkedReader struct {
	data  []byte
	sizes []int
	pos   int
	idx   int
}

func (r *chunkedReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	size := r.sizes[r.idx%len(r.sizes)]
	r.idx++
	if size <= 0 {
		size = 1
	}
	if size > len(p) {
		size = len(p)
	}
	if remain := len(r.data) - r.pos; size > remain {
		size = remain
	}
	n := copy(p, r.data[r.pos:r.pos+size])
	r.pos += n
	return n, nil
}

// dataEvent 把一段 JSON 载荷包成一条 data 事件。
func dataEvent(payload string) string {
	return "data: " + payload + "\n\n"
}

// sseEventBlock 把一段 JSON 载荷包成带事件名的 SSE 事件。
func sseEventBlock(event, payload string) string {
	return "event: " + event + "\ndata: " + payload + "\n\n"
}

// parsedEvent 是解析后的下游事件。
type parsedEvent struct {
	Event string
	Data  map[string]any
	Raw   string
}

// parseSSE 用 sse.Scan 解析下游字节流（同时验证输出确实是合法 SSE 帧）。
func parseSSE(t *testing.T, raw []byte) []parsedEvent {
	t.Helper()
	var events []parsedEvent
	err := sse.Scan(bytes.NewReader(raw), func(ev sse.Event) error {
		parsed := parsedEvent{Event: ev.Event, Raw: ev.Data}
		if obj, err := decodeObject([]byte(ev.Data)); err == nil {
			parsed.Data = obj
		}
		events = append(events, parsed)
		return nil
	})
	if err != nil {
		t.Fatalf("sse.Scan(%s): %v", raw, err)
	}
	return events
}

// eventNames 给出事件序列（事件名，缺省时用 data 里的 type 兜底）。
func eventNames(events []parsedEvent) []string {
	names := make([]string, 0, len(events))
	for _, ev := range events {
		name := ev.Event
		if name == "" && ev.Data != nil {
			name, _ = asString(ev.Data["type"])
		}
		names = append(names, name)
	}
	return names
}

// eventsOfType 取出指定类型的事件。
func eventsOfType(events []parsedEvent, want string) []map[string]any {
	var out []map[string]any
	for _, ev := range events {
		if ev.Data == nil {
			continue
		}
		if got, _ := asString(ev.Data["type"]); got == want {
			out = append(out, ev.Data)
		}
	}
	return out
}

// equalStrings 比较两个字符串切片。
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// generatedIDMatcher 匹配 genID 生成的 `<prefix>_<24 位十六进制>` id。
var generatedIDMatcher = regexp.MustCompile(`[a-z]+_[0-9a-f]{24}`)

// normalizeIDs 把每次转换都会重新生成的 id 归一化，便于跨次运行逐字节比较。
func normalizeIDs(raw []byte) []byte {
	return generatedIDMatcher.ReplaceAll(raw, []byte("<id>"))
}

// joinChunks 拼接字节块。
func joinChunks(chunks [][]byte) []byte {
	var out []byte
	for _, chunk := range chunks {
		out = append(out, chunk...)
	}
	return out
}

// runStream 通过门面跑一遍 chat 上游 → out 格式的流式转换。
func runStream(t *testing.T, session *convSession, out ApiFormat, model string, payload string, sizes []int) ([]byte, Usage) {
	t.Helper()
	var buf bytes.Buffer
	reader := &chunkedReader{data: []byte(payload), sizes: sizes}
	usage, err := session.convertStreamResponse(FormatOpenAIChat, out, reader, &buf, model, nil)
	if err != nil {
		t.Fatalf("ConvertStreamResponse: %v", err)
	}
	return buf.Bytes(), usage
}

// ==================== SSE 分帧 ====================

func TestScanSSEFraming(t *testing.T) {
	payload := "event: response.created\ndata: {\"a\":1}\n\n" +
		"data: {\"b\":2}\n" +
		"data: {\"c\":3}\n\n" +
		"event: ping\n\n" + // 只有 event 行：不构成事件
		"data: {\"d\":4}\r\n\r\n" + // CRLF 分帧
		": comment\n\n" + // 注释行：忽略
		"data:   {\"e\":5}   \n\n" + // 载荷两侧空白被裁掉
		"data: [DONE]\n\n" +
		"data: {\"late\":true}\n\n" // [DONE] 之后不再交付

	want := []sse.Event{
		{Event: "response.created", Data: `{"a":1}`},
		{Event: "", Data: "{\"b\":2}\n{\"c\":3}"},
		{Event: "", Data: `{"d":4}`},
		{Event: "", Data: `{"e":5}`},
	}

	collect := func(r io.Reader) ([]sse.Event, error) {
		var got []sse.Event
		err := sse.Scan(r, func(ev sse.Event) error {
			got = append(got, ev)
			return nil
		})
		return got, err
	}

	// 基线：整段一次读完。
	got, err := collect(strings.NewReader(payload))
	if err != nil {
		t.Fatalf("sse.Scan: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("事件数 = %d (%#v), want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("事件[%d] = %#v, want %#v", i, got[i], want[i])
		}
	}

	// 每一个字节偏移处切开：结果必须逐字节等价。
	for offset := 0; offset <= len(payload); offset++ {
		sizes := []int{offset, len(payload) - offset}
		if offset == 0 {
			sizes = []int{len(payload)}
		}
		split, err := collect(&chunkedReader{data: []byte(payload), sizes: sizes})
		if err != nil {
			t.Fatalf("offset %d: sse.Scan: %v", offset, err)
		}
		if len(split) != len(want) {
			t.Fatalf("offset %d: 事件数 = %d (%#v), want %d", offset, len(split), split, len(want))
		}
		for i := range want {
			if split[i] != want[i] {
				t.Fatalf("offset %d: 事件[%d] = %#v, want %#v", offset, i, split[i], want[i])
			}
		}
	}

	// 逐字节喂入同样等价。
	byteWise, err := collect(&chunkedReader{data: []byte(payload), sizes: []int{1}})
	if err != nil {
		t.Fatalf("逐字节: sse.Scan: %v", err)
	}
	if len(byteWise) != len(want) {
		t.Fatalf("逐字节: 事件数 = %d, want %d", len(byteWise), len(want))
	}
	for i := range want {
		if byteWise[i] != want[i] {
			t.Fatalf("逐字节: 事件[%d] = %#v, want %#v", i, byteWise[i], want[i])
		}
	}

	// 无分隔符结尾的残段也要在 EOF 时交付。
	tail, err := collect(strings.NewReader("data: {\"f\":6}"))
	if err != nil {
		t.Fatalf("残段: sse.Scan: %v", err)
	}
	if len(tail) != 1 || tail[0].Data != `{"f":6}` {
		t.Fatalf("残段事件 = %#v", tail)
	}
}

func TestScanSSEHandleErrorAborts(t *testing.T) {
	wantErr := errors.New("boom")
	calls := 0
	payload := dataEvent(`{"n":1}`) + dataEvent(`{"n":2}`) + dataEvent(`{"n":3}`)
	err := sse.Scan(strings.NewReader(payload), func(ev sse.Event) error {
		calls++
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
	if calls != 1 {
		t.Fatalf("handle 调用次数 = %d, want 1（出错后立即中止）", calls)
	}
}

// ==================== chat → messages（流式）====================

var chatTextStream = dataEvent(`{"id":"c1","model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`) +
	dataEvent(`{"id":"c1","model":"gpt-4o","choices":[{"index":0,"delta":{"content":"he"},"finish_reason":null}]}`) +
	dataEvent(`{"id":"c1","model":"gpt-4o","choices":[{"index":0,"delta":{"content":"llo"},"finish_reason":null}]}`) +
	dataEvent(`{"id":"c1","model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`) +
	dataEvent(`{"id":"c1","model":"gpt-4o","choices":[],"usage":{"prompt_tokens":123,"completion_tokens":45,"total_tokens":168}}`) +
	dataEvent(`[DONE]`)

func TestChatToAnthropicStreamEventOrderAndUsage(t *testing.T) {
	session := newConvSession()
	raw, usage := runStream(t, session, FormatAnthropic, "claude-x", chatTextStream, []int{len(chatTextStream)})

	events := parseSSE(t, raw)
	want := []string{
		"message_start",
		"content_block_start",
		"content_block_delta",
		"content_block_delta",
		"content_block_stop",
		"message_delta",
		"message_stop",
	}
	if got := eventNames(events); !equalStrings(got, want) {
		t.Fatalf("事件顺序 = %v, want %v", got, want)
	}

	// message_start 带模型名、空 content、usage 初始为零。
	// 门面传入的模型名优先；未传时才回退到上游 chunk 里的 model。
	start := eventsOfType(events, "message_start")[0]
	message, _ := asMap(start["message"])
	if got, _ := asString(message["model"]); got != "claude-x" {
		t.Fatalf("message_start.model = %q, want claude-x", got)
	}
	fallback := newChatToAnthropicStream("")
	fallbackOut := joinChunks(fallback.processEvent(sse.Event{Data: `{"id":"c1","model":"upstream-model","choices":[{"index":0,"delta":{"content":"x"},"finish_reason":null}]}`}))
	fallbackMessage := mustMapField(t, eventsOfType(parseSSE(t, fallbackOut), "message_start")[0], "message")
	if got, _ := asString(fallbackMessage["model"]); got != "upstream-model" {
		t.Fatalf("回退模型名 = %q, want upstream-model", got)
	}
	if got, _ := asString(message["role"]); got != "assistant" {
		t.Fatalf("message_start.role = %q", got)
	}
	if got, _ := asString(message["type"]); got != "message" {
		t.Fatalf("message_start.message.type = %q", got)
	}

	// content_block_start 是 text 块，下标 0。
	blockStart := eventsOfType(events, "content_block_start")[0]
	if got := intField(blockStart, "index"); got != 0 {
		t.Fatalf("content_block_start.index = %d", got)
	}
	block, _ := asMap(blockStart["content_block"])
	if got, _ := asString(block["type"]); got != "text" {
		t.Fatalf("content_block.type = %q", got)
	}

	// 拼接的 text_delta 必须等于上游文本。
	var text strings.Builder
	for _, delta := range eventsOfType(events, "content_block_delta") {
		inner, _ := asMap(delta["delta"])
		if got, _ := asString(inner["type"]); got != "text_delta" {
			t.Fatalf("delta.type = %q", got)
		}
		part, _ := asString(inner["text"])
		text.WriteString(part)
	}
	if text.String() != "hello" {
		t.Fatalf("拼接文本 = %q, want hello", text.String())
	}

	// message_delta 必须携带**真实** usage 与 stop_reason（不能是写死的 0）。
	delta := eventsOfType(events, "message_delta")
	if len(delta) != 1 {
		t.Fatalf("message_delta 数量 = %d, want 1", len(delta))
	}
	stop, _ := asMap(delta[0]["delta"])
	if got, _ := asString(stop["stop_reason"]); got != "end_turn" {
		t.Fatalf("stop_reason = %q, want end_turn", got)
	}
	deltaUsage, _ := asMap(delta[0]["usage"])
	if got := intField(deltaUsage, "input_tokens"); got != 123 {
		t.Fatalf("usage.input_tokens = %d, want 123（上游尾部 usage chunk）", got)
	}
	if got := intField(deltaUsage, "output_tokens"); got != 45 {
		t.Fatalf("usage.output_tokens = %d, want 45", got)
	}

	if usage != (Usage{InputTokens: 123, OutputTokens: 45}) {
		t.Fatalf("返回 Usage = %+v", usage)
	}

	// message_stop 必须是最后一个事件。
	if name := eventNames(events)[len(events)-1]; name != "message_stop" {
		t.Fatalf("最后一个事件 = %q, want message_stop", name)
	}
}

func TestChatToAnthropicStreamCacheTokensUseAnthropicConvention(t *testing.T) {
	payload := dataEvent(`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"content":"x"},"finish_reason":null}]}`) +
		dataEvent(`{"id":"c1","model":"m","choices":[],"usage":{"prompt_tokens":100,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":40}}}`) +
		dataEvent(`[DONE]`)

	session := newConvSession()
	raw, usage := runStream(t, session, FormatAnthropic, "m", payload, []int{len(payload)})
	delta := eventsOfType(parseSSE(t, raw), "message_delta")[0]
	deltaUsage, _ := asMap(delta["usage"])
	if got := intField(deltaUsage, "input_tokens"); got != 60 {
		t.Fatalf("input_tokens = %d, want 60（扣除缓存命中）", got)
	}
	if got := intField(deltaUsage, "cache_read_input_tokens"); got != 40 {
		t.Fatalf("cache_read_input_tokens = %d, want 40", got)
	}
	if usage != (Usage{InputTokens: 60, OutputTokens: 5, CacheReadTokens: 40}) {
		t.Fatalf("返回 Usage = %+v", usage)
	}
}

func TestChatToAnthropicStreamToolUse(t *testing.T) {
	payload := dataEvent(`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"loc"}}]},"finish_reason":null}]}`) +
		dataEvent(`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"ation\":\"SF\"}"}}]},"finish_reason":null}]}`) +
		dataEvent(`{"id":"c1","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`) +
		dataEvent(`{"id":"c1","model":"m","choices":[],"usage":{"prompt_tokens":11,"completion_tokens":3}}`) +
		dataEvent(`[DONE]`)

	session := newConvSession()
	raw, usage := runStream(t, session, FormatAnthropic, "m", payload, []int{len(payload)})
	events := parseSSE(t, raw)

	want := []string{
		"message_start",
		"content_block_start",
		"content_block_delta",
		"content_block_delta",
		"content_block_stop",
		"message_delta",
		"message_stop",
	}
	if got := eventNames(events); !equalStrings(got, want) {
		t.Fatalf("事件顺序 = %v, want %v", got, want)
	}

	blockStart := eventsOfType(events, "content_block_start")[0]
	block, _ := asMap(blockStart["content_block"])
	if got, _ := asString(block["type"]); got != "tool_use" {
		t.Fatalf("content_block.type = %q, want tool_use", got)
	}
	if got, _ := asString(block["id"]); got != "call_1" {
		t.Fatalf("content_block.id = %q", got)
	}
	if got, _ := asString(block["name"]); got != "get_weather" {
		t.Fatalf("content_block.name = %q", got)
	}
	if input, ok := asMap(block["input"]); !ok || len(input) != 0 {
		t.Fatalf("content_block.input = %#v, want 空对象", block["input"])
	}

	var partial strings.Builder
	for _, delta := range eventsOfType(events, "content_block_delta") {
		inner, _ := asMap(delta["delta"])
		if got, _ := asString(inner["type"]); got != "input_json_delta" {
			t.Fatalf("delta.type = %q, want input_json_delta", got)
		}
		fragment, _ := asString(inner["partial_json"])
		partial.WriteString(fragment)
	}
	if partial.String() != `{"location":"SF"}` {
		t.Fatalf("拼接 partial_json = %q", partial.String())
	}

	// 出现了工具调用，stop_reason 必须是 tool_use（即便上游给的是 stop）。
	stop, _ := asMap(eventsOfType(events, "message_delta")[0]["delta"])
	if got, _ := asString(stop["stop_reason"]); got != "tool_use" {
		t.Fatalf("stop_reason = %q, want tool_use", got)
	}
	if usage.InputTokens != 11 || usage.OutputTokens != 3 {
		t.Fatalf("返回 Usage = %+v", usage)
	}
}

func TestChatToAnthropicStreamFinalizeWithoutDone(t *testing.T) {
	// 上游不发 [DONE] 直接断流：finalize 必须补齐 message_delta / message_stop。
	payload := dataEvent(`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"content":"x"},"finish_reason":null}]}`)
	session := newConvSession()
	raw, _ := runStream(t, session, FormatAnthropic, "m", payload, []int{len(payload)})
	names := eventNames(parseSSE(t, raw))
	want := []string{"message_start", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop"}
	if !equalStrings(names, want) {
		t.Fatalf("事件顺序 = %v, want %v", names, want)
	}

	// 显式 [DONE] 事件同样触发收尾，且重复触发不会再发一次。
	conv := newChatToAnthropicStream("m")
	first := conv.processEvent(sse.Event{Data: `{"id":"c1","choices":[{"index":0,"delta":{"content":"x"},"finish_reason":"length"}],"usage":{"prompt_tokens":9,"completion_tokens":7}}`})
	done := conv.processEvent(sse.Event{Data: "[DONE]"})
	again := conv.processEvent(sse.Event{Data: "[DONE]"})
	if len(again) != 0 {
		t.Fatalf("重复 [DONE] 不应再生产事件: %v", again)
	}
	names = eventNames(parseSSE(t, joinChunks(append(first, done...))))
	want = []string{"message_start", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop"}
	if !equalStrings(names, want) {
		t.Fatalf("[DONE] 收尾事件 = %v, want %v", names, want)
	}
	delta := eventsOfType(parseSSE(t, joinChunks(append(first, done...))), "message_delta")[0]
	if got, _ := asString(mustMapField(t, delta, "delta")["stop_reason"]); got != "max_tokens" {
		t.Fatalf("stop_reason = %q, want max_tokens", got)
	}
	if usage := conv.usage(); usage != (Usage{InputTokens: 9, OutputTokens: 7}) {
		t.Fatalf("usage() = %+v", usage)
	}

	// 空流：finalize 也要给出完整骨架。
	empty := newChatToAnthropicStream("m")
	names = eventNames(parseSSE(t, joinChunks(empty.finalize())))
	want = []string{"message_start", "message_delta", "message_stop"}
	if !equalStrings(names, want) {
		t.Fatalf("空流收尾 = %v, want %v", names, want)
	}
}

// mustMapField 取 map 字段并断言类型。
func mustMapField(t *testing.T, obj map[string]any, key string) map[string]any {
	t.Helper()
	value, ok := asMap(obj[key])
	if !ok {
		t.Fatalf("%s 不是对象: %#v", key, obj[key])
	}
	return value
}

// ==================== chat → responses（流式）====================

func TestChatToResponsesStreamEventOrderAndUsage(t *testing.T) {
	session := newConvSession()
	raw, usage := runStream(t, session, FormatResponses, "gpt-4o", chatTextStream, []int{len(chatTextStream)})
	events := parseSSE(t, raw)

	want := []string{
		"response.created",
		"response.in_progress",
		"response.output_item.added",
		"response.content_part.added",
		"response.output_text.delta",
		"response.output_text.delta",
		"response.output_text.done",
		"response.content_part.done",
		"response.output_item.done",
		"response.completed",
	}
	if got := eventNames(events); !equalStrings(got, want) {
		t.Fatalf("事件顺序 = %v, want %v", got, want)
	}

	created := eventsOfType(events, "response.created")[0]
	createdResp, _ := asMap(created["response"])
	if got, _ := asString(createdResp["status"]); got != "in_progress" {
		t.Fatalf("response.created.status = %q", got)
	}
	if got, _ := asString(createdResp["model"]); got != "gpt-4o" {
		t.Fatalf("response.created.model = %q", got)
	}
	inProgress := eventsOfType(events, "response.in_progress")[0]
	inProgressResp, _ := asMap(inProgress["response"])
	if got, _ := asString(inProgressResp["object"]); got != "response" {
		t.Fatalf("response.in_progress.object = %q", got)
	}

	added := eventsOfType(events, "response.output_item.added")[0]
	addedItem, _ := asMap(added["item"])
	if got, _ := asString(addedItem["type"]); got != "message" {
		t.Fatalf("output_item.added.item.type = %q", got)
	}
	if got := intField(added, "output_index"); got != 0 {
		t.Fatalf("output_index = %d, want 0", got)
	}

	var text strings.Builder
	for _, delta := range eventsOfType(events, "response.output_text.delta") {
		fragment, _ := asString(delta["delta"])
		text.WriteString(fragment)
	}
	if text.String() != "hello" {
		t.Fatalf("拼接 delta = %q, want hello", text.String())
	}

	doneText := eventsOfType(events, "response.output_text.done")[0]
	if got, _ := asString(doneText["text"]); got != "hello" {
		t.Fatalf("output_text.done.text = %q", got)
	}

	// response.completed 必须带 usage，且是上游尾部真实值。
	completed := eventsOfType(events, "response.completed")[0]
	completedResp := mustMapField(t, completed, "response")
	if got, _ := asString(completedResp["status"]); got != "completed" {
		t.Fatalf("status = %q", got)
	}
	completedUsage := mustMapField(t, completedResp, "usage")
	if got := intField(completedUsage, "input_tokens"); got != 123 {
		t.Fatalf("usage.input_tokens = %d, want 123", got)
	}
	if got := intField(completedUsage, "output_tokens"); got != 45 {
		t.Fatalf("usage.output_tokens = %d, want 45", got)
	}
	if got := intField(completedUsage, "total_tokens"); got != 168 {
		t.Fatalf("usage.total_tokens = %d, want 168", got)
	}
	output, ok := asArray(completedResp["output"])
	if !ok || len(output) != 1 {
		t.Fatalf("output = %#v, want 1 个 message item", completedResp["output"])
	}
	item, _ := asMap(output[0])
	if got, _ := asString(item["type"]); got != "message" {
		t.Fatalf("output[0].type = %q", got)
	}
	if got, _ := asString(item["status"]); got != "completed" {
		t.Fatalf("output[0].status = %q", got)
	}
	parts, _ := asArray(item["content"])
	if len(parts) != 1 {
		t.Fatalf("output[0].content = %#v", item["content"])
	}
	part, _ := asMap(parts[0])
	if got, _ := asString(part["text"]); got != "hello" {
		t.Fatalf("output[0].content[0].text = %q", got)
	}
	if usage != (Usage{InputTokens: 123, OutputTokens: 45}) {
		t.Fatalf("返回 Usage = %+v", usage)
	}

	// sequence_number 必须自增且唯一。
	seq := -1
	for i, ev := range events {
		n, ok := asInt(ev.Data["sequence_number"])
		if !ok {
			t.Fatalf("事件[%d] 缺少 sequence_number", i)
		}
		if int(n) != seq+1 {
			t.Fatalf("事件[%d].sequence_number = %d, want %d", i, n, seq+1)
		}
		seq = int(n)
	}
}

func TestChatToResponsesStreamUsageAlwaysPresent(t *testing.T) {
	// 上游完全不给 usage：response.completed 仍必须带零值 usage 对象。
	payload := dataEvent(`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`) +
		dataEvent(`{"id":"c1","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"length"}]}`) +
		dataEvent(`[DONE]`)

	session := newConvSession()
	raw, _ := runStream(t, session, FormatResponses, "m", payload, []int{len(payload)})
	completed := eventsOfType(parseSSE(t, raw), "response.completed")[0]
	completedResp := mustMapField(t, completed, "response")
	if got, _ := asString(completedResp["status"]); got != "incomplete" {
		t.Fatalf("status = %q, want incomplete（finish_reason=length）", got)
	}
	usageRaw, present := completedResp["usage"]
	if !present {
		t.Fatalf("response.completed 必须带 usage 字段")
	}
	usage, ok := asMap(usageRaw)
	if !ok {
		t.Fatalf("usage 必须是对象，得到 %#v", usageRaw)
	}
	for _, key := range []string{"input_tokens", "output_tokens", "total_tokens"} {
		value, present := usage[key]
		if !present {
			t.Fatalf("usage 缺少 %s: %#v", key, usage)
		}
		if n, ok := asInt(value); !ok || n != 0 {
			t.Fatalf("usage[%s] = %#v, want 0", key, value)
		}
	}
}

func TestChatToResponsesStreamNamespaceAndCustomTools(t *testing.T) {
	session := newConvSession()
	flat, ok := flattenNamespaceSubtool("mcp__tools", map[string]any{
		"name":       "sub_a",
		"parameters": map[string]any{"type": "object"},
	}, session.nsReverse)
	if !ok {
		t.Fatalf("flattenNamespaceSubtool 失败")
	}
	fn, _ := asMap(flat["function"])
	flatName, _ := asString(fn["name"])
	registerCustomTool("exec", session.nsReverse)

	payload := dataEvent(`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"`+flatName+`","arguments":"{\"a\":"}}]},"finish_reason":null}]}`) +
		dataEvent(`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"hi\"}"}}]},"finish_reason":null}]}`) +
		dataEvent(`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_2","type":"function","function":{"name":"exec","arguments":"{\"content\":\"return"}}]},"finish_reason":null}]}`) +
		dataEvent(`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"arguments":" 1\"}"}}]},"finish_reason":null}]}`) +
		dataEvent(`{"id":"c1","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`) +
		dataEvent(`{"id":"c1","model":"m","choices":[],"usage":{"prompt_tokens":20,"completion_tokens":8}}`) +
		dataEvent(`[DONE]`)

	raw, usage := runStream(t, session, FormatResponses, "m", payload, []int{len(payload)})
	events := parseSSE(t, raw)

	// 两个工具项各自的 output_item.added：namespace 子工具还原成 subtool + namespace，
	// custom 工具还原成 custom_tool_call。
	var functionAddedEvent, customAddedEvent map[string]any
	var functionItem, customItem map[string]any
	for _, added := range eventsOfType(events, "response.output_item.added") {
		item := mustMapField(t, added, "item")
		switch itemType, _ := asString(item["type"]); itemType {
		case "function_call":
			functionAddedEvent, functionItem = added, item
		case "custom_tool_call":
			customAddedEvent, customItem = added, item
		}
	}
	if functionAddedEvent == nil {
		t.Fatalf("缺少 function_call 的 output_item.added: %v", eventNames(events))
	}
	if customAddedEvent == nil {
		t.Fatalf("缺少 custom_tool_call 的 output_item.added: %v", eventNames(events))
	}
	if got, _ := asString(functionItem["name"]); got != "sub_a" {
		t.Fatalf("function_call.name = %q, want sub_a", got)
	}
	if got, _ := asString(functionItem["namespace"]); got != "mcp__tools" {
		t.Fatalf("function_call.namespace = %q, want mcp__tools", got)
	}
	if got, _ := asString(functionItem["call_id"]); got != "call_1" {
		t.Fatalf("function_call.call_id = %q", got)
	}
	if got, _ := asString(customItem["name"]); got != "exec" {
		t.Fatalf("custom_tool_call.name = %q", got)
	}
	if _, present := customItem["namespace"]; present {
		t.Fatalf("custom_tool_call 不应带 namespace: %#v", customItem)
	}
	if got, _ := asString(customItem["input"]); got != "" {
		t.Fatalf("custom_tool_call.input（新增时） = %q, want 空串", got)
	}

	// 参数增量按 output_index 归属正确，拼接后与上游一致。
	argsByIndex := map[int]string{}
	for _, delta := range eventsOfType(events, "response.function_call_arguments.delta") {
		fragment, _ := asString(delta["delta"])
		index := intField(delta, "output_index")
		argsByIndex[index] += fragment
	}
	functionIndex := intField(functionAddedEvent, "output_index")
	customIndex := intField(customAddedEvent, "output_index")
	if functionIndex < 1 || customIndex < 1 || functionIndex == customIndex {
		t.Fatalf("工具项 output_index = %d / %d, want 均 >= 1 且互不相同", functionIndex, customIndex)
	}
	if got := argsByIndex[functionIndex]; got != `{"a":"hi"}` {
		t.Fatalf("function_call 参数拼接 = %q", got)
	}
	if got := argsByIndex[customIndex]; got != `{"content":"return 1"}` {
		t.Fatalf("custom_tool_call 参数拼接 = %q", got)
	}

	// 收尾事件：每个工具一条 arguments.done + output_item.done。
	argumentDone := eventsOfType(events, "response.function_call_arguments.done")
	if len(argumentDone) != 2 {
		t.Fatalf("function_call_arguments.done 数量 = %d, want 2", len(argumentDone))
	}
	var doneFunction, doneCustom map[string]any
	for _, done := range eventsOfType(events, "response.output_item.done") {
		item := mustMapField(t, done, "item")
		switch itemType, _ := asString(item["type"]); itemType {
		case "function_call":
			doneFunction = item
		case "custom_tool_call":
			doneCustom = item
		}
	}
	if doneFunction == nil || doneCustom == nil {
		t.Fatalf("缺少工具项的 output_item.done: %v", eventNames(events))
	}
	if got, _ := asString(doneFunction["arguments"]); got != `{"a":"hi"}` {
		t.Fatalf("function_call.arguments = %q", got)
	}
	if got, _ := asString(doneFunction["name"]); got != "sub_a" {
		t.Fatalf("function_call.name = %q", got)
	}
	if got, _ := asString(doneCustom["name"]); got != "exec" {
		t.Fatalf("custom_tool_call.name = %q", got)
	}
	if got, _ := asString(doneCustom["input"]); got != "return 1" {
		t.Fatalf("custom_tool_call.input = %q, want 解壳后的 return 1", got)
	}
	if _, present := doneCustom["arguments"]; present {
		t.Fatalf("custom_tool_call 不应带 arguments: %#v", doneCustom)
	}
	if got, _ := asString(doneCustom["status"]); got != "completed" {
		t.Fatalf("custom_tool_call.status = %q", got)
	}

	// response.completed 的 output 必须包含这两个工具项，且 usage 为真实值。
	completedResp := mustMapField(t, eventsOfType(events, "response.completed")[0], "response")
	output, _ := asArray(completedResp["output"])
	if len(output) != 2 {
		t.Fatalf("response.completed.output = %#v, want 2 项", completedResp["output"])
	}
	completedUsage := mustMapField(t, completedResp, "usage")
	if got := intField(completedUsage, "input_tokens"); got != 20 {
		t.Fatalf("usage.input_tokens = %d, want 20", got)
	}
	if got := intField(completedUsage, "output_tokens"); got != 8 {
		t.Fatalf("usage.output_tokens = %d, want 8", got)
	}
	if usage != (Usage{InputTokens: 20, OutputTokens: 8}) {
		t.Fatalf("返回 Usage = %+v", usage)
	}
}

func TestChatToResponsesStreamReasoningPart(t *testing.T) {
	payload := dataEvent(`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"reasoning_content":"想"},"finish_reason":null}]}`) +
		dataEvent(`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"content":"答"},"finish_reason":null}]}`) +
		dataEvent(`{"id":"c1","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`) +
		dataEvent(`[DONE]`)

	session := newConvSession()
	raw, _ := runStream(t, session, FormatResponses, "m", payload, []int{len(payload)})
	events := parseSSE(t, raw)

	// reasoning part 先开、正文 part 后开：两次 content_part.added / done。
	if got := len(eventsOfType(events, "response.content_part.added")); got != 2 {
		t.Fatalf("content_part.added 数量 = %d, want 2", got)
	}
	added := eventsOfType(events, "response.content_part.added")
	firstPart := mustMapField(t, added[0], "part")
	annotations, _ := asArray(firstPart["annotations"])
	if len(annotations) != 1 {
		t.Fatalf("reasoning part annotations = %#v", firstPart["annotations"])
	}
	if got := intField(added[0], "content_index"); got != 0 {
		t.Fatalf("reasoning content_index = %d, want 0", got)
	}
	if got := intField(added[1], "content_index"); got != 1 {
		t.Fatalf("text content_index = %d, want 1", got)
	}

	completedResp := mustMapField(t, eventsOfType(events, "response.completed")[0], "response")
	output, _ := asArray(completedResp["output"])
	if len(output) != 1 {
		t.Fatalf("output = %#v, want 1 个 message item", completedResp["output"])
	}
	item, _ := asMap(output[0])
	parts, _ := asArray(item["content"])
	if len(parts) != 2 {
		t.Fatalf("message.content = %#v, want 2 个 part", item["content"])
	}
	reasoningPart, _ := asMap(parts[0])
	if got, _ := asString(reasoningPart["text"]); got != "想" {
		t.Fatalf("parts[0].text = %q", got)
	}
	textPart, _ := asMap(parts[1])
	if got, _ := asString(textPart["text"]); got != "答" {
		t.Fatalf("parts[1].text = %q", got)
	}
}

// ==================== 完整响应对象重放为 SSE ====================

func TestReplayChat(t *testing.T) {
	resp := mustObject(t, `{
		"id": "chatcmpl-1", "object": "chat.completion", "created": 42, "model": "gpt-4o",
		"choices": [{
			"index": 0,
			"message": {
				"role": "assistant",
				"content": "hi there",
				"tool_calls": [{"id":"call_1","type":"function","function":{"name":"f","arguments":"{}"}}]
			},
			"finish_reason": "tool_calls"
		}],
		"usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}
	}`)

	raw := replayChat(resp)
	if !bytes.HasSuffix(raw, []byte("data: [DONE]\n\n")) {
		t.Fatalf("重放流必须以 data: [DONE] 结束:\n%s", raw)
	}
	events := parseSSE(t, raw)
	// role / content / tool_calls / finish / usage（usage 是本地补的尾部 chunk）。
	if len(events) != 5 {
		t.Fatalf("事件数 = %d, want 5:\n%s", len(events), raw)
	}
	roleDelta := mustMapField(t, mustMapChoice(t, events[0].Data), "delta")
	if got, _ := asString(roleDelta["role"]); got != "assistant" {
		t.Fatalf("首个 chunk delta.role = %q", got)
	}
	content := mustMapField(t, mustMapChoice(t, events[1].Data), "delta")
	if got, _ := asString(content["content"]); got != "hi there" {
		t.Fatalf("content = %q", got)
	}
	toolCalls := mustMapField(t, mustMapChoice(t, events[2].Data), "delta")
	if _, ok := asArray(toolCalls["tool_calls"]); !ok {
		t.Fatalf("tool_calls 未搬运: %#v", toolCalls)
	}
	finishChoice := mustMapChoice(t, events[3].Data)
	if got, _ := asString(finishChoice["finish_reason"]); got != "tool_calls" {
		t.Fatalf("finish_reason = %q", got)
	}
	choices, _ := asArray(events[4].Data["choices"])
	if len(choices) != 0 {
		t.Fatalf("usage chunk 的 choices 必须为空数组: %#v", events[4].Data["choices"])
	}
	usage := mustMapField(t, events[4].Data, "usage")
	if got := intField(usage, "prompt_tokens"); got != 10 {
		t.Fatalf("usage.prompt_tokens = %d", got)
	}
}

// mustMapChoice 取 Chat chunk 的 choices[0]。
func mustMapChoice(t *testing.T, chunk map[string]any) map[string]any {
	t.Helper()
	choices, ok := asArray(chunk["choices"])
	if !ok || len(choices) == 0 {
		t.Fatalf("choices 不是非空数组: %#v", chunk["choices"])
	}
	choice, ok := asMap(choices[0])
	if !ok {
		t.Fatalf("choices[0] 不是对象: %#v", choices[0])
	}
	return choice
}

func TestReplayAnthropic(t *testing.T) {
	resp := mustObject(t, `{
		"id": "msg_1", "type": "message", "role": "assistant", "model": "claude-x",
		"content": [
			{"type": "text", "text": "hello"},
			{"type": "thinking", "thinking": "hmm"},
			{"type": "tool_use", "id": "toolu_1", "name": "f", "input": {"a": 1}}
		],
		"stop_reason": "tool_use",
		"usage": {"input_tokens": 10, "output_tokens": 5}
	}`)

	raw := replayAnthropic(resp)
	events := parseSSE(t, raw)
	want := []string{
		"message_start",
		"content_block_start", "content_block_delta", "content_block_stop",
		"content_block_start", "content_block_delta", "content_block_stop",
		"content_block_start", "content_block_delta", "content_block_stop",
		"message_delta",
		"message_stop",
	}
	if got := eventNames(events); !equalStrings(got, want) {
		t.Fatalf("事件顺序 = %v, want %v", got, want)
	}
	message := mustMapField(t, events[0].Data, "message")
	startUsage := mustMapField(t, message, "usage")
	if got := intField(startUsage, "input_tokens"); got != 10 {
		t.Fatalf("message_start.usage.input_tokens = %d", got)
	}
	deltas := eventsOfType(events, "content_block_delta")
	firstDelta := mustMapField(t, deltas[0], "delta")
	if got, _ := asString(firstDelta["text"]); got != "hello" {
		t.Fatalf("text_delta.text = %q", got)
	}
	secondDelta := mustMapField(t, deltas[1], "delta")
	if got, _ := asString(secondDelta["thinking"]); got != "hmm" {
		t.Fatalf("thinking_delta.thinking = %q", got)
	}
	thirdDelta := mustMapField(t, deltas[2], "delta")
	if got, _ := asString(thirdDelta["partial_json"]); got != `{"a":1}` {
		t.Fatalf("input_json_delta.partial_json = %q", got)
	}
	stop := eventsOfType(events, "message_delta")[0]
	stopDelta := mustMapField(t, stop, "delta")
	if got, _ := asString(stopDelta["stop_reason"]); got != "tool_use" {
		t.Fatalf("stop_reason = %q", got)
	}
	stopUsage := mustMapField(t, stop, "usage")
	if got := intField(stopUsage, "output_tokens"); got != 5 {
		t.Fatalf("message_delta.usage.output_tokens = %d", got)
	}

	// 未知块型整体透传（start + stop）。
	unknown := replayAnthropic(mustObject(t, `{"id":"msg_2","content":[{"type":"redacted_thinking","data":"x"}]}`))
	names := eventNames(parseSSE(t, unknown))
	want = []string{"message_start", "content_block_start", "content_block_stop", "message_delta", "message_stop"}
	if !equalStrings(names, want) {
		t.Fatalf("未知块型事件 = %v, want %v", names, want)
	}
}

func TestReplayResponses(t *testing.T) {
	resp := mustObject(t, `{
		"id": "resp_1", "object": "response", "model": "gpt-5", "status": "completed",
		"output": [
			{
				"type": "message", "id": "msg_1", "status": "completed", "role": "assistant",
				"content": [
					{"type": "output_text", "text": "hi", "annotations": []},
					{"type": "output_text", "text": "there", "annotations": [{"type":"reasoning"}]}
				]
			},
			{"type": "function_call", "id": "fc_1", "call_id": "call_1", "name": "f", "arguments": "{}", "status": "completed"}
		],
		"usage": {"input_tokens": 14326, "output_tokens": 213, "total_tokens": 14539}
	}`)

	raw := replayResponses(resp)
	events := parseSSE(t, raw)
	want := []string{
		"response.created",
		"response.in_progress",
		"response.output_item.added",
		"response.content_part.added",
		"response.output_text.delta",
		"response.output_text.done",
		"response.content_part.done",
		"response.content_part.added",
		"response.output_text.delta",
		"response.output_text.done",
		"response.content_part.done",
		"response.output_item.done",
		"response.output_item.added",
		"response.output_item.done",
		"response.completed",
	}
	if got := eventNames(events); !equalStrings(got, want) {
		t.Fatalf("事件顺序 = %v, want %v", got, want)
	}

	// sequence_number 自增且每个 data 都带 type。
	seq := -1
	for i, ev := range events {
		n, ok := asInt(ev.Data["sequence_number"])
		if !ok || int(n) != seq+1 {
			t.Fatalf("事件[%d].sequence_number = %#v, want %d", i, ev.Data["sequence_number"], seq+1)
		}
		if got, _ := asString(ev.Data["type"]); got != want[i] {
			t.Fatalf("事件[%d].type = %q, want %q", i, got, want[i])
		}
		seq = int(n)
	}

	completed := eventsOfType(events, "response.completed")[0]
	completedResp := mustMapField(t, completed, "response")
	if got, _ := asString(completedResp["object"]); got != "response" {
		t.Fatalf("completed.response.object = %q", got)
	}
	completedUsage := mustMapField(t, completedResp, "usage")
	if got := intField(completedUsage, "input_tokens"); got != 14326 {
		t.Fatalf("usage.input_tokens = %d", got)
	}
	if got := intField(completedUsage, "output_tokens"); got != 213 {
		t.Fatalf("usage.output_tokens = %d", got)
	}
	output, _ := asArray(completedResp["output"])
	if len(output) != 2 {
		t.Fatalf("completed.response.output = %#v", completedResp["output"])
	}

	// 上游没有 usage 时也要给零值对象。
	noUsage := replayResponses(mustObject(t, `{"id":"resp_2","output":[]}`))
	tail := eventsOfType(parseSSE(t, noUsage), "response.completed")[0]
	tailUsage := mustMapField(t, mustMapField(t, tail, "response"), "usage")
	if got := intField(tailUsage, "total_tokens"); got != 0 {
		t.Fatalf("零值 usage.total_tokens = %d, want 0", got)
	}
}

// ==================== 门面：任意 chunk 边界必须产出同一结果 ====================

func TestConvertStreamResponseChunkBoundaryInvariance(t *testing.T) {
	cases := []struct {
		name    string
		out     ApiFormat
		payload string
	}{
		{name: "chat → messages", out: FormatAnthropic, payload: chatTextStream},
		{name: "chat → responses", out: FormatResponses, payload: chatTextStream},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			session := newConvSession()
			baseline, baselineUsage := runStream(t, session, tc.out, "m", tc.payload, []int{len(tc.payload)})
			for offset := 0; offset <= len(tc.payload); offset++ {
				sizes := []int{offset, len(tc.payload) - offset}
				if offset == 0 {
					sizes = []int{len(tc.payload)}
				}
				got, gotUsage := runStream(t, session, tc.out, "m", tc.payload, sizes)
				// id 是一次性生成的随机值，比较前归一化。
				if !bytes.Equal(normalizeIDs(got), normalizeIDs(baseline)) {
					t.Fatalf("offset %d: 输出与整段读取不一致\n--- want ---\n%s\n--- got ---\n%s", offset, baseline, got)
				}
				if gotUsage != baselineUsage {
					t.Fatalf("offset %d: usage = %+v, want %+v", offset, gotUsage, baselineUsage)
				}
			}
		})
	}
}

// ==================== 门面：完整响应对象重放 ====================

func TestFullResponseToSSEFacade(t *testing.T) {
	// in == out：仅重放，不做转换。
	chat := []byte(`{"id":"chatcmpl-1","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`)
	chatSession := newConvSession()
	raw, err := chatSession.fullResponseToSSE(FormatOpenAIChat, FormatOpenAIChat, chat, "m")
	if err != nil {
		t.Fatalf("FullResponseToSSE(chat→chat): %v", err)
	}
	if !bytes.Contains(raw, []byte("chat.completion.chunk")) || !bytes.HasSuffix(raw, []byte("data: [DONE]\n\n")) {
		t.Fatalf("chat 重放输出异常:\n%s", raw)
	}

	// in != out：先转换再重放为入口格式的事件流。
	responsesSession := newConvSession()
	raw, err = responsesSession.fullResponseToSSE(FormatOpenAIChat, FormatResponses, chat, "m")
	if err != nil {
		t.Fatalf("FullResponseToSSE(chat→responses): %v", err)
	}
	names := eventNames(parseSSE(t, raw))
	if len(names) == 0 || names[0] != "response.created" || names[len(names)-1] != "response.completed" {
		t.Fatalf("responses 重放事件 = %v", names)
	}

	messagesSession := newConvSession()
	raw, err = messagesSession.fullResponseToSSE(FormatOpenAIChat, FormatAnthropic, chat, "m")
	if err != nil {
		t.Fatalf("FullResponseToSSE(chat→messages): %v", err)
	}
	names = eventNames(parseSSE(t, raw))
	want := []string{"message_start", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop"}
	if !equalStrings(names, want) {
		t.Fatalf("messages 重放事件 = %v, want %v", names, want)
	}

	// 矩阵外的组合必须报错。
	if _, err := chatSession.fullResponseToSSE(FormatOpenAIChat, FormatAnthropic, []byte(`{`), "m"); err == nil {
		t.Fatalf("非法 JSON 必须报错")
	}
	if _, err := chatSession.convertStreamResponse(FormatAnthropic, FormatResponses, strings.NewReader(""), io.Discard, "m", nil); err == nil {
		t.Fatalf("messages → responses 必须返回 *UnsupportedConversion")
	} else if _, ok := err.(*UnsupportedConversion); !ok {
		t.Fatalf("错误类型 = %T, want *UnsupportedConversion", err)
	}
}
