package converter

import "strings"

// request_reverse.go —— **请求方向的逆向映射：Chat → 富协议**。
//
//	chatRequestToAnthropic ：chat → messages
//	chatRequestToResponses ：chat → responses
//
// 它们是 request.go 里 messages→chat / responses→chat 的逆，服务于
// 「Chat 客户端 → 富协议渠道」，以及经 Chat 的两跳（如 messages 客户端 → responses 渠道）。
//
// 两条边都遵循同一套「有损处理」纪律（见 plan 的「有损清单」）：
//   - 目标协议里**没有对应概念**、丢弃会改变调用方语义的字段（n>1、logprobs）→ 直接拒绝
//     （*UnsupportedFieldError，代理层转 400 并带上字段名）；
//   - 其余没有对应概念的字段 → 丢弃或改写，并记一条带方向的 note（Hop.Notes() 会打日志）。

// ==================== 共用的入站卫生与拒绝规则 ====================

// rejectChatFieldsFor 拒绝目标协议无法表达的 Chat 字段。
//
// 这些字段被静默丢弃会改变调用方的语义（例如 n>1 时调用方会解析多条 choice），
// 因此宁可明确失败，也不给出形状不对的结果。
func rejectChatFieldsFor(chat map[string]any, target ApiFormat) error {
	if n, ok := asInt(chat["n"]); ok && n > 1 {
		return &UnsupportedFieldError{
			Field:  "n",
			Target: target,
			Reason: "the target protocol returns exactly one choice per request",
		}
	}
	for _, field := range []string{"logprobs", "top_logprobs"} {
		v, present := chat[field]
		if !present || v == nil {
			continue
		}
		if b, isBool := asBool(v); isBool && !b {
			continue // logprobs: false 是显式关闭，可以照常转换
		}
		return &UnsupportedFieldError{
			Field:  field,
			Target: target,
			Reason: "the target protocol has no token-level log probabilities",
		}
	}
	return nil
}

// hygienizeInboundChat 对**入站**的 Chat 请求体做工具消息卫生检查。
//
// 为什么在这里做：sanitizeRawChatToolMessages 原本只在「富协议 → Chat」时对**出站**
// Chat 体执行；Chat 当客户端时出站体是富协议，于是这层清洗不会跑。但入站 Chat 体里的
// 问题依旧存在，而且后果更重——Anthropic 会严格校验 tool_use/tool_result 配对，
// Chat 侧断链或缺 tool_call_id 的 tool 消息映射过去就是非法请求。
func (s *convSession) hygienizeInboundChat(chat map[string]any, target ApiFormat) {
	messages, ok := asArray(chat["messages"])
	if !ok {
		return
	}
	sanitized, changed := sanitizeRawChatMessages(messages)
	if !changed {
		return
	}
	chat["messages"] = sanitized
	s.noteEdge(FormatOpenAIChat, target,
		"入站工具消息不满足严格配对（断链或缺 tool_call_id），已按 Chat 约定修复后再转换")
}

// noteDroppedChatFields 对「存在但目标协议没有对应概念」的字段记 note。
func (s *convSession) noteDroppedChatFields(chat map[string]any, target ApiFormat, fields ...string) {
	for _, field := range fields {
		if v, present := chat[field]; present && v != nil {
			s.noteEdge(FormatOpenAIChat, target, "丢弃字段 %q：目标协议没有对应概念", field)
		}
	}
}

// chatSystemTexts 收集 Chat 里 system（以及等价于 system 的 developer）消息的文本。
// 同时把这两类消息从 messages 里排除掉——它们要落到目标协议的 system/instructions 位置。
func (s *convSession) chatSystemTexts(chat map[string]any, target ApiFormat) ([]string, []any) {
	messages, ok := asArray(chat["messages"])
	if !ok {
		return nil, nil
	}
	var systemTexts []string
	rest := make([]any, 0, len(messages))
	for _, raw := range messages {
		msg, ok := asMap(raw)
		if !ok {
			continue
		}
		role, _ := asString(msg["role"])
		if role == "system" || role == "developer" {
			if text := extractText(msg["content"]); text != "" {
				systemTexts = append(systemTexts, text)
			}
			continue
		}
		rest = append(rest, msg)
	}
	return systemTexts, rest
}

// chatToolCallsOf 取一条 Chat 消息的 tool_calls（统一成 [{id, name, arguments}] 形态）。
func chatToolCallsOf(msg map[string]any) []chatCall {
	calls, ok := asArray(msg["tool_calls"])
	if !ok {
		return nil
	}
	out := make([]chatCall, 0, len(calls))
	for _, raw := range calls {
		call, ok := asMap(raw)
		if !ok {
			continue
		}
		item := chatCall{ID: "", Name: "", Arguments: "{}"}
		item.ID, _ = asString(call["id"])
		if fn, ok := asMap(call["function"]); ok {
			item.Name, _ = asString(fn["name"])
			if args, ok := asString(fn["arguments"]); ok && args != "" {
				item.Arguments = args
			}
		}
		out = append(out, item)
	}
	return out
}

// chatCall 是 Chat 工具调用的归一化形态。
type chatCall struct {
	ID        string
	Name      string
	Arguments string
}

// ==================== Chat → Anthropic Messages ====================

// chatRequestToAnthropic 把 Chat Completions 请求转换为 Anthropic Messages 请求。
//
// 映射要点（与 convertRequest 互为逆）：
//
//	system / developer 消息 → system（文本块数组）
//	user / assistant 消息    → messages（content 一律用块数组形态）
//	assistant.tool_calls     → assistant 的 tool_use 块
//	tool 消息                → 紧随其后的 user 消息里的 tool_result 块（连续多条合并为一条）
//	tools                    → Anthropic 工具（function 嵌套展平为 input_schema）
//	tool_choice              → auto / any / tool（none 通过不下发 tools 表达）
//	reasoning_effort         → thinking.budget_tokens
//	max_tokens               → max_tokens（必填，缺失时用 defaultAnthropicMaxTokens 兜底）
//	stop                     → stop_sequences
//	user                     → metadata.user_id
func (s *convSession) chatRequestToAnthropic(chat map[string]any, opts RequestOptions) (map[string]any, error) {
	if err := rejectChatFieldsFor(chat, FormatAnthropic); err != nil {
		return nil, err
	}
	s.hygienizeInboundChat(chat, FormatAnthropic)

	systemTexts, rest := s.chatSystemTexts(chat, FormatAnthropic)

	out := map[string]any{
		"model": pickModel(chat, opts.Model),
	}

	// system：用文本块数组（可承载 cache_control，且多段 system 不会丢边界）。
	if len(systemTexts) > 0 {
		blocks := make([]any, 0, len(systemTexts))
		for _, text := range systemTexts {
			blocks = append(blocks, map[string]any{"type": "text", "text": text})
		}
		out["system"] = blocks
	}

	messages, err := s.chatMessagesToAnthropic(rest)
	if err != nil {
		return nil, err
	}
	out["messages"] = messages

	if tools, ok := s.chatToolsToAnthropic(chat); ok {
		out["tools"] = tools
	}
	if choice, ok := s.chatToolChoiceToAnthropic(chat, out); ok {
		out["tool_choice"] = choice
	}

	// max_tokens：Anthropic 必填（Rust 侧同名常量 defaultAnthropicMaxTokens）。
	// Chat 的 max_tokens 可选，很多客户端不带，因此必须有兜底，否则转换后的请求会被
	// 上游以 400 拒绝；补值时记 note，调用方能据此知道上限是被设定的。
	maxTokens, ok := asInt(chat["max_tokens"])
	if !ok || maxTokens <= 0 {
		maxTokens = defaultAnthropicMaxTokens
		s.noteEdge(FormatOpenAIChat, FormatAnthropic,
			"入站请求没有 max_tokens，已按 Anthropic 的必填要求补默认值 %d", defaultAnthropicMaxTokens)
	}

	// 推理强度 → thinking 预算。
	if effort := downstreamReasoningEffort(chat["reasoning_effort"], chat["reasoning"], chat["thinking"], chat["output_config"]); effort != "" {
		budget := effortToBudget(effort)
		if budget >= maxTokens {
			// Anthropic 要求 max_tokens > budget_tokens。
			maxTokens = budget + 1024
			s.noteEdge(FormatOpenAIChat, FormatAnthropic,
				"thinking 预算 %d 不小于 max_tokens，已把 max_tokens 提升到 %d", budget, maxTokens)
		}
		out["thinking"] = map[string]any{"type": "enabled", "budget_tokens": budget}
		// 开启 thinking 时 Anthropic 不接受 temperature / top_p。
		if _, present := chat["temperature"]; present {
			s.noteEdge(FormatOpenAIChat, FormatAnthropic, "开启 thinking 时丢弃 temperature（Anthropic 不接受）")
		}
		if _, present := chat["top_p"]; present {
			s.noteEdge(FormatOpenAIChat, FormatAnthropic, "开启 thinking 时丢弃 top_p（Anthropic 不接受）")
		}
	} else {
		copyIfPresent(out, chat, "temperature")
		copyIfPresent(out, chat, "top_p")
	}
	out["max_tokens"] = maxTokens

	// stop → stop_sequences（Chat 允许字符串或数组）。
	// 注意用 []any 而不是 []string：转换产物是 JSON 对象，全仓库统一用 []any 承载数组，
	// 混入 []string 会让按 []any 取值的地方（asArray）看不到它。
	if v, present := chat["stop"]; present && v != nil {
		if seqs := stringList(v); len(seqs) > 0 {
			items := make([]any, 0, len(seqs))
			for _, seq := range seqs {
				items = append(items, seq)
			}
			out["stop_sequences"] = items
		}
	}

	// user → metadata.user_id（Anthropic 有对应字段，不算有损）。
	if user, ok := asString(chat["user"]); ok && user != "" {
		out["metadata"] = map[string]any{"user_id": user}
	}

	out["stream"] = opts.Stream

	// stream_options 刻意不记 note：富协议恒返回 usage，客户端的意图自动被满足。
	s.noteDroppedChatFields(chat, FormatAnthropic,
		"seed", "frequency_penalty", "presence_penalty", "logit_bias", "response_format",
		"parallel_tool_calls")
	return out, nil
}

// chatMessagesToAnthropic 把（已剔除 system 的）Chat 消息序列转换为 Anthropic messages。
func (s *convSession) chatMessagesToAnthropic(messages []any) ([]any, error) {
	out := make([]any, 0, len(messages))
	var pendingToolResults []any

	// flushToolResults 落地累积的 tool_result 块。
	// Anthropic 要求 tool_result 出现在紧随 assistant 工具调用的 user 消息里，
	// 因此连续的 Chat tool 消息必须合并成同一条 user 消息。
	flushToolResults := func() {
		if len(pendingToolResults) == 0 {
			return
		}
		out = append(out, map[string]any{"role": "user", "content": pendingToolResults})
		pendingToolResults = nil
	}

	for _, raw := range messages {
		msg, ok := asMap(raw)
		if !ok {
			continue
		}
		role, _ := asString(msg["role"])

		if role == "tool" || role == "function" {
			block, ok := s.chatToolMessageToAnthropic(msg)
			if !ok {
				continue
			}
			pendingToolResults = append(pendingToolResults, block)
			continue
		}

		// 遇到普通消息前先落地工具结果，保证配对顺序。
		flushToolResults()

		anthropicRole := "user"
		if role == "assistant" {
			anthropicRole = "assistant"
		}

		blocks := s.chatContentToAnthropicBlocks(msg["content"])
		for _, call := range chatToolCallsOf(msg) {
			input := any(map[string]any{})
			if v, ok := decodeAny(call.Arguments); ok {
				input = v
			} else {
				s.noteEdge(FormatOpenAIChat, FormatAnthropic,
					"工具 %q 的 arguments 不是合法 JSON，input 退化为空对象", call.Name)
			}
			id := call.ID
			if id == "" {
				id = genID("toolu_")
			}
			blocks = append(blocks, map[string]any{
				"type":  "tool_use",
				"id":    id,
				"name":  call.Name,
				"input": input,
			})
		}

		// 推理内容：Chat 只有 reasoning_content 字符串，而 Anthropic 会校验 thinking 块的
		// signature。签名无法伪造（伪造会被上游拒绝），因此这里**丢弃**并记录。
		if _, ok := asString(msg["reasoning_content"]); ok {
			s.noteEdge(FormatOpenAIChat, FormatAnthropic,
				"丢弃 assistant 历史里的 reasoning_content：Anthropic 的 thinking 块需要有效签名，无法伪造")
		}

		if len(blocks) == 0 {
			// Anthropic 不接受空 content，跳过并说明（否则整条请求会被上游拒绝）。
			s.noteEdge(FormatOpenAIChat, FormatAnthropic, "跳过内容为空的 %s 消息（Anthropic 不接受空 content）", anthropicRole)
			continue
		}
		out = append(out, map[string]any{"role": anthropicRole, "content": blocks})
	}

	flushToolResults()

	// Anthropic 要求 messages 非空；入站只有 system 时补一条占位 user 消息。
	if len(out) == 0 {
		out = append(out, map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": ""},
		}})
	}
	return out, nil
}

// chatToolMessageToAnthropic 把一条 Chat tool 消息转换为 Anthropic tool_result 块。
func (s *convSession) chatToolMessageToAnthropic(msg map[string]any) (any, bool) {
	callID, _ := asString(msg["tool_call_id"])
	if callID == "" {
		// 没有 tool_call_id 的 tool 消息在 Anthropic 侧无法配对，只能丢弃。
		s.noteEdge(FormatOpenAIChat, FormatAnthropic,
			"丢弃缺少 tool_call_id 的 tool 消息（Anthropic 的 tool_result 必须与 tool_use 配对）")
		return nil, false
	}
	return map[string]any{
		"type":        "tool_result",
		"tool_use_id": callID,
		"content":     extractText(msg["content"]),
	}, true
}

// chatContentToAnthropicBlocks 把 Chat 的 content（字符串或 part 数组）转成 Anthropic 内容块。
func (s *convSession) chatContentToAnthropicBlocks(content any) []any {
	blocks := []any{}
	if text, ok := asString(content); ok {
		if text != "" {
			blocks = append(blocks, map[string]any{"type": "text", "text": text})
		}
		return blocks
	}
	parts, ok := asArray(content)
	if !ok {
		return blocks
	}
	for _, raw := range parts {
		if text, ok := asString(raw); ok {
			if text != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": text})
			}
			continue
		}
		part, ok := asMap(raw)
		if !ok {
			continue
		}
		switch ptype, _ := asString(part["type"]); ptype {
		case "text":
			if text, ok := asString(part["text"]); ok && text != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": text})
			}
		case "image_url":
			blocks = append(blocks, openaiImageToAnthropic(part["image_url"]))
		default:
			s.noteEdge(FormatOpenAIChat, FormatAnthropic, "丢弃不支持的内容 part 类型 %q", ptype)
		}
	}
	return blocks
}

// chatToolsToAnthropic 把 Chat 的 tools 转换为 Anthropic 工具列表。
// 返回 ok=false 表示不需要下发 tools 键。
func (s *convSession) chatToolsToAnthropic(chat map[string]any) ([]any, bool) {
	tools, ok := asArray(chat["tools"])
	if !ok || len(tools) == 0 {
		return nil, false
	}
	out := make([]any, 0, len(tools))
	for _, raw := range tools {
		tool, ok := asMap(raw)
		if !ok {
			continue
		}
		if ttype, _ := asString(tool["type"]); ttype != "function" {
			// 例如 Chat 的 web_search / mcp：Anthropic 侧没有同名类型，丢弃并说明。
			s.noteEdge(FormatOpenAIChat, FormatAnthropic, "丢弃非 function 工具类型 %q", ttype)
			continue
		}
		fn, ok := asMap(tool["function"])
		if !ok {
			continue
		}
		name, _ := asString(fn["name"])
		if name == "" {
			continue
		}
		item := map[string]any{
			"name":         name,
			"input_schema": toolParametersOrDefault(fn["parameters"]),
		}
		if desc, ok := asString(fn["description"]); ok && desc != "" {
			item["description"] = desc
		}
		out = append(out, item)
	}
	if len(out) == 0 {
		return nil, false
	}
	return out, true
}

// chatToolChoiceToAnthropic 把 Chat 的 tool_choice 转换为 Anthropic 形态。
//
// Anthropic 没有 "none"：要禁止工具只能不下发 tools，因此 "none" 会就地删掉 out["tools"]。
func (s *convSession) chatToolChoiceToAnthropic(chat map[string]any, out map[string]any) (any, bool) {
	raw, present := chat["tool_choice"]
	if !present || raw == nil {
		return nil, false
	}
	if choice, ok := asString(raw); ok {
		switch choice {
		case "auto":
			return map[string]any{"type": "auto"}, true
		case "required", "any":
			return map[string]any{"type": "any"}, true
		case "none":
			delete(out, "tools")
			s.noteEdge(FormatOpenAIChat, FormatAnthropic,
				"tool_choice=none 通过不下发 tools 表达（Anthropic 没有 none 取值）")
			return nil, false
		default:
			return map[string]any{"type": "auto"}, true
		}
	}
	if m, ok := asMap(raw); ok {
		if name, ok := nestedString2(m, "function", "name"); ok {
			return map[string]any{"type": "tool", "name": name}, true
		}
	}
	return nil, false
}

// ==================== Chat → OpenAI Responses ====================

// chatRequestToResponses 把 Chat Completions 请求转换为 OpenAI Responses 请求。
//
// 映射要点（与 responsesToChat 互为逆）：
//
//	system / developer 消息 → instructions（字符串）
//	user 消息                → input 的 message 项（input_text / input_image part）
//	assistant 消息           → input 的 message 项（output_text part）
//	assistant.tool_calls     → input 的 function_call 项（call_id 配对）
//	tool 消息                → input 的 function_call_output 项
//	tools                    → Responses 扁平工具（name/description/parameters 直接在顶层）
//	tool_choice              → auto / none / required / {"type":"function","name":...}
//	max_tokens               → max_output_tokens
//	response_format          → text.format
//	reasoning_effort         → reasoning.effort
func (s *convSession) chatRequestToResponses(chat map[string]any, opts RequestOptions) (map[string]any, error) {
	if err := rejectChatFieldsFor(chat, FormatResponses); err != nil {
		return nil, err
	}
	s.hygienizeInboundChat(chat, FormatResponses)

	systemTexts, rest := s.chatSystemTexts(chat, FormatResponses)

	out := map[string]any{
		"model": pickModel(chat, opts.Model),
		"input": s.chatMessagesToResponsesInput(rest),
	}
	if len(systemTexts) > 0 {
		out["instructions"] = strings.Join(systemTexts, "\n\n")
	}

	if tools, ok := s.chatToolsToResponses(chat); ok {
		out["tools"] = tools
	}
	if choice, ok := chatToolChoiceToResponses(chat); ok {
		out["tool_choice"] = choice
	}

	if maxTokens, ok := asInt(chat["max_tokens"]); ok && maxTokens > 0 {
		out["max_output_tokens"] = maxTokens
	}
	if effort := downstreamReasoningEffort(chat["reasoning_effort"], chat["reasoning"], chat["thinking"], chat["output_config"]); effort != "" {
		out["reasoning"] = map[string]any{"effort": effort}
	}
	if format := chatResponseFormatToTextFormat(chat["response_format"]); format != nil {
		out["text"] = map[string]any{"format": format}
	}

	copyIfPresent(out, chat, "temperature")
	copyIfPresent(out, chat, "top_p")

	out["stream"] = opts.Stream

	// Responses 没有 stop sequences，也没有 Chat 的采样惩罚项与 logit_bias。
	// stream_options 刻意不记 note（同上）。
	s.noteDroppedChatFields(chat, FormatResponses,
		"stop", "seed", "frequency_penalty", "presence_penalty", "logit_bias",
		"parallel_tool_calls", "user")
	return out, nil
}

// chatMessagesToResponsesInput 把（已剔除 system 的）Chat 消息序列转换为 Responses input 项。
func (s *convSession) chatMessagesToResponsesInput(messages []any) []any {
	out := make([]any, 0, len(messages))
	for _, raw := range messages {
		msg, ok := asMap(raw)
		if !ok {
			continue
		}
		role, _ := asString(msg["role"])

		switch role {
		case "tool", "function":
			callID, _ := asString(msg["tool_call_id"])
			if callID == "" {
				s.noteEdge(FormatOpenAIChat, FormatResponses,
					"丢弃缺少 tool_call_id 的 tool 消息（function_call_output 必须与 function_call 配对）")
				continue
			}
			out = append(out, map[string]any{
				"type":    "function_call_output",
				"call_id": callID,
				"output":  extractText(msg["content"]),
			})
			continue
		}

		responsesRole := "user"
		if role == "assistant" {
			responsesRole = "assistant"
		}

		if parts := s.chatContentToResponsesParts(msg["content"], responsesRole); len(parts) > 0 {
			out = append(out, map[string]any{
				"type":    "message",
				"role":    responsesRole,
				"content": parts,
			})
		}

		for _, call := range chatToolCallsOf(msg) {
			item := map[string]any{
				"type":      "function_call",
				"call_id":   call.ID,
				"name":      call.Name,
				"arguments": call.Arguments,
			}
			if call.ID == "" {
				item["call_id"] = genID("call_")
			}
			out = append(out, item)
		}

		// 推理内容：Responses 的 reasoning 项必须是模型真正产出过的项（带 id / encrypted_content
		// 才能续接），用一段字符串伪造出来的项可能被上游拒绝。因此丢弃并记录，
		// 而不是赌上游接受。
		if _, ok := asString(msg["reasoning_content"]); ok {
			s.noteEdge(FormatOpenAIChat, FormatResponses,
				"丢弃 assistant 历史里的 reasoning_content：Responses 的 reasoning 项无法用纯文本伪造")
		}
	}
	return out
}

// chatContentToResponsesParts 把 Chat 的 content 转成 Responses 的输入 part 数组。
func (s *convSession) chatContentToResponsesParts(content any, role string) []any {
	// assistant 的历史正文在 Responses 侧用 output_text，user 侧用 input_text。
	textType := "input_text"
	if role == "assistant" {
		textType = "output_text"
	}
	parts := []any{}
	if text, ok := asString(content); ok {
		if text != "" {
			parts = append(parts, map[string]any{"type": textType, "text": text})
		}
		return parts
	}
	raw, ok := asArray(content)
	if !ok {
		return parts
	}
	for _, item := range raw {
		if text, ok := asString(item); ok {
			if text != "" {
				parts = append(parts, map[string]any{"type": textType, "text": text})
			}
			continue
		}
		part, ok := asMap(item)
		if !ok {
			continue
		}
		switch ptype, _ := asString(part["type"]); ptype {
		case "text":
			if text, ok := asString(part["text"]); ok && text != "" {
				parts = append(parts, map[string]any{"type": textType, "text": text})
			}
		case "image_url":
			if url := responsesImageURL(part); url != "" {
				parts = append(parts, map[string]any{"type": "input_image", "image_url": url})
			}
		default:
			s.noteEdge(FormatOpenAIChat, FormatResponses, "丢弃不支持的内容 part 类型 %q", ptype)
		}
	}
	return parts
}

// chatToolsToResponses 把 Chat 的 tools 转换为 Responses 的扁平工具列表。
func (s *convSession) chatToolsToResponses(chat map[string]any) ([]any, bool) {
	tools, ok := asArray(chat["tools"])
	if !ok || len(tools) == 0 {
		return nil, false
	}
	out := make([]any, 0, len(tools))
	for _, raw := range tools {
		tool, ok := asMap(raw)
		if !ok {
			continue
		}
		if ttype, _ := asString(tool["type"]); ttype != "function" {
			s.noteEdge(FormatOpenAIChat, FormatResponses, "丢弃非 function 工具类型 %q", ttype)
			continue
		}
		fn, ok := asMap(tool["function"])
		if !ok {
			continue
		}
		name, _ := asString(fn["name"])
		if name == "" {
			continue
		}
		item := map[string]any{
			"type":       "function",
			"name":       name,
			"parameters": toolParametersOrDefault(fn["parameters"]),
		}
		if desc, ok := asString(fn["description"]); ok && desc != "" {
			item["description"] = desc
		}
		out = append(out, item)
	}
	if len(out) == 0 {
		return nil, false
	}
	return out, true
}

// chatToolChoiceToResponses 把 Chat 的 tool_choice 转换为 Responses 形态。
// 两者的字符串取值（auto / none / required）一致，只有指定函数时的形状不同。
func chatToolChoiceToResponses(chat map[string]any) (any, bool) {
	raw, present := chat["tool_choice"]
	if !present || raw == nil {
		return nil, false
	}
	if choice, ok := asString(raw); ok {
		switch choice {
		case "auto", "none", "required":
			return choice, true
		default:
			return "auto", true
		}
	}
	if m, ok := asMap(raw); ok {
		if name, ok := nestedString2(m, "function", "name"); ok {
			return map[string]any{"type": "function", "name": name}, true
		}
	}
	return nil, false
}

// chatResponseFormatToTextFormat 是 textFormatToResponseFormat 的逆：
// Chat 的 response_format → Responses 的 text.format。
func chatResponseFormatToTextFormat(format any) map[string]any {
	m, ok := asMap(format)
	if !ok {
		return nil
	}
	switch t, _ := asString(m["type"]); t {
	case "json_object":
		return map[string]any{"type": "json_object"}
	case "json_schema":
		schema, ok := asMap(m["json_schema"])
		if !ok {
			return nil
		}
		name, _ := asString(schema["name"])
		if name == "" {
			name = "response_schema"
		}
		out := map[string]any{
			"type":   "json_schema",
			"name":   name,
			"schema": schema["schema"],
		}
		if strict, ok := asBool(schema["strict"]); ok {
			out["strict"] = strict
		}
		return out
	default:
		// 例如 "text"：Responses 的默认形态就是纯文本，无需下发。
		return nil
	}
}

// ==================== 小工具 ====================

// copyIfPresent 把 src 里存在且非 nil 的字段原样复制到 dst。
func copyIfPresent(dst, src map[string]any, key string) {
	if v, present := src[key]; present && v != nil {
		dst[key] = v
	}
}

// stringList 把「字符串或字符串数组」统一成字符串切片。
func stringList(v any) []string {
	if s, ok := asString(v); ok {
		if s == "" {
			return nil
		}
		return []string{s}
	}
	items, ok := asArray(v)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		if s, ok := asString(item); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

// nestedString2 下钻两层取字符串（keys 为空或类型不符时返回 false）。
func nestedString2(obj map[string]any, keys ...string) (string, bool) {
	var cur any = obj
	for _, key := range keys {
		m, ok := asMap(cur)
		if !ok {
			return "", false
		}
		cur, ok = m[key]
		if !ok {
			return "", false
		}
	}
	s, ok := asString(cur)
	return s, ok && s != ""
}
