package converter

import (
	"io"

	"modelbridge/internal/sse"
)

// pipeline.go —— **流式转换流水线**：把路径上的每一跳串成一条链。
//
// 一条路径的响应方向有 N 条边（0/1/2），就串 N 个阶段，第 1 个阶段读上游事件流，
// 末阶段写客户端事件流。每个阶段都是现成的 streamConverter 状态机：
//
//	chat → messages / chat → responses ：stream.go（既有，逐字节未改）
//	messages → chat / responses → chat ：stream_reverse.go（新增）
//
// 为什么中间用「帧 → 事件」再解析一次（framesToEvents），而不是让阶段之间直接传对象：
// 这样两个**已验证**的既有转换器一行都不用改（它们被 1100 行流测试与 e2e 锁着），
// 新增的只有两个逆向阶段。代价是每个事件多一次 marshal + parse（微秒级，相对网络可忽略）；
// 若日后有性能诉求，再把这层换成直接传 map。
//
// 真增量：每个上游事件进来就被翻译并立即写出（flush），不做整段缓冲——这是明确要求，
// 也是「缓冲拼装再重放」那条省事路线被否决的原因。

// streamPipeline 是一条按路径组装好的流式转换链。
type streamPipeline struct {
	stages []streamConverter
	// clientChat 表示末阶段产出的是 Chat 事件流，收尾要补 `data: [DONE]`。
	// 只有 1 跳（富协议渠道 → Chat 客户端）时才会为真。
	clientChat bool
}

// newStreamPipeline 按路径的响应方向组装转换链（阶段数 = 跳数）。
func newStreamPipeline(plan Plan, model string, ns map[string]nsEntry) *streamPipeline {
	stages := make([]streamConverter, 0, plan.Hops())
	for _, edge := range plan.ResponseEdges() {
		switch {
		case edge.From == FormatOpenAIChat && edge.To == FormatAnthropic:
			stages = append(stages, newChatToAnthropicStream(model))
		case edge.From == FormatOpenAIChat && edge.To == FormatResponses:
			stages = append(stages, newChatToResponsesStream(model, ns))
		case edge.From == FormatAnthropic && edge.To == FormatOpenAIChat:
			stages = append(stages, newMessagesToChatStream(model))
		case edge.From == FormatResponses && edge.To == FormatOpenAIChat:
			stages = append(stages, newResponsesToChatStream(model))
		}
	}
	return &streamPipeline{stages: stages, clientChat: plan.Client() == FormatOpenAIChat}
}

// run 读上游事件流、逐事件翻译并写客户端；返回写出的错误（收尾照常执行）。
func (p *streamPipeline) run(r io.Reader, w io.Writer, flush func()) error {
	write := func(frames [][]byte) error {
		for _, frame := range frames {
			if _, err := w.Write(frame); err != nil {
				return err
			}
			if flush != nil {
				flush()
			}
		}
		return nil
	}

	// 路径上没有边（同协议）：本函数不该被调用，但真发生了也要把字节透传出去，
	// 而不是静默丢空响应。
	if len(p.stages) == 0 {
		_, err := io.Copy(w, r)
		if flush != nil {
			flush()
		}
		return err
	}

	if err := sse.Scan(r, func(ev sse.Event) error {
		return write(p.feed(ev))
	}); err != nil {
		return err
	}
	return write(p.finish())
}

// feed 把一个上游事件推进流水线，返回要写给客户端的字节（末阶段的输出）。
func (p *streamPipeline) feed(ev sse.Event) [][]byte {
	events := []sse.Event{ev}
	for i, stage := range p.stages {
		out := make([][]byte, 0, 4)
		for _, e := range events {
			out = append(out, stage.processEvent(e)...)
		}
		if i == len(p.stages)-1 {
			return out
		}
		events = framesToEvents(out)
	}
	return nil
}

// finish 做**收尾级联**：上游结束时先让第 1 阶段收尾，把它的终局输出喂给下一阶段，
// 再让下一阶段收尾……直到末阶段。每个阶段的 finalize 只调用一次。
//
// 不依赖中间帧里的 `data: [DONE]`：sse.Scan 本就不投递 [DONE]，收尾由这里显式驱动，
// 语义比「靠终止符传递」更明确。
func (p *streamPipeline) finish() [][]byte {
	if len(p.stages) == 0 {
		return nil
	}
	var carry []sse.Event
	var frames [][]byte
	for i, stage := range p.stages {
		out := make([][]byte, 0, 4)
		if i > 0 {
			for _, e := range carry {
				out = append(out, stage.processEvent(e)...)
			}
		}
		out = append(out, stage.finalize()...)

		if i == len(p.stages)-1 {
			frames = out
			break
		}
		carry = framesToEvents(out)
	}
	if p.clientChat {
		// Chat 客户端的流以 `data: [DONE]` 结束；富协议方向不写这一行。
		frames = append(frames, []byte("data: "+sse.Done+"\n\n"))
	}
	return frames
}

// usage 返回**上游原始**用量（Anthropic 口径）：取第 1 阶段的累加值。
// 中间阶段看到的是上一阶段合成的 usage，不能作为账单口径。
func (p *streamPipeline) usage() Usage {
	if len(p.stages) == 0 {
		return Usage{}
	}
	return p.stages[0].usage()
}

// framesToEvents 把已编码的 SSE 帧解析回事件，供下一阶段消费。
// 复用 internal/sse 的分帧实现，保证与上游解析完全同一套规则。
func framesToEvents(frames [][]byte) []sse.Event {
	out := make([]sse.Event, 0, len(frames))
	for _, frame := range frames {
		out = append(out, sse.Events(string(frame))...)
	}
	return out
}
