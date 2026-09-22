package converter

// direction.go —— **方向派发**：把一条「单跳边」映射到具体的实现函数。
//
// 为什么单独一层：协议方向是转换里唯一无法用类型表达的东西（两端都是 ApiFormat），
// 因此把它收敛成一张 switch 表，而不是散落在各个调用点。新增一条边只改这里；
// 出问题时也只需检查这一处（历史上真实的缺陷就是方向写反，见 hop.go 顶部）。
//
// 六条有序对（3×3 去掉恒等）都有实现，因此任意路径都能折叠出来：
//
//	请求方向：messages→chat、responses→chat（request.go）
//	          chat→messages、chat→responses（request_reverse.go）
//	响应方向：chat→messages、chat→responses（response.go）
//	          messages→chat、responses→chat（response_reverse.go）
//	流式：    chat→messages、chat→responses（stream.go）
//	          messages→chat、responses→chat（stream_reverse.go）

// mapRequest 施加一次请求方向的映射（from → to）。
func mapRequest(s *convSession, from, to ApiFormat, obj map[string]any, opts RequestOptions) (map[string]any, error) {
	switch {
	case from == to:
		return obj, nil
	case from == FormatAnthropic && to == FormatOpenAIChat:
		converted, err := convertRequest(obj, opts)
		if err != nil {
			return nil, err
		}
		if err := finalizeChatBody(converted, opts); err != nil {
			return nil, err
		}
		return converted, nil
	case from == FormatResponses && to == FormatOpenAIChat:
		converted, err := s.responsesToChat(obj, opts)
		if err != nil {
			return nil, err
		}
		if err := finalizeChatBody(converted, opts); err != nil {
			return nil, err
		}
		return converted, nil
	case from == FormatOpenAIChat && to == FormatAnthropic:
		return s.chatRequestToAnthropic(obj, opts)
	case from == FormatOpenAIChat && to == FormatResponses:
		return s.chatRequestToResponses(obj, opts)
	default:
		return nil, &UnsupportedConversion{From: from, To: to}
	}
}

// mapResponse 施加一次响应方向的映射（from → to）。
func mapResponse(s *convSession, from, to ApiFormat, obj map[string]any, model string) (map[string]any, error) {
	switch {
	case from == to:
		return obj, nil
	case from == FormatOpenAIChat && to == FormatAnthropic:
		return openAIToAnthropicResponse(obj, model), nil
	case from == FormatOpenAIChat && to == FormatResponses:
		return s.toResponsesWithNS(obj, model), nil
	case from == FormatAnthropic && to == FormatOpenAIChat:
		return s.anthropicResponseToChat(obj, model), nil
	case from == FormatResponses && to == FormatOpenAIChat:
		return s.responsesResponseToChat(obj, model), nil
	default:
		return nil, &UnsupportedConversion{From: from, To: to}
	}
}

// replayAs 把完整的响应对象按指定协议重放成该协议的 SSE 事件流。
// 用于「上游对流式请求返回了非 SSE 的 JSON 体」的降级路径（见 Hop.ReplayResponseAsSSE）。
func replayAs(obj map[string]any, format ApiFormat) []byte {
	switch format {
	case FormatResponses:
		return replayResponses(obj)
	case FormatAnthropic:
		return replayAnthropic(obj)
	default:
		return replayChat(obj)
	}
}
