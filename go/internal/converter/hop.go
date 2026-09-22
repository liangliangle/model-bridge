package converter

import "io"

// 本文件是转换器的**唯一对外入口**。
//
// 背景：session 的四个转换方法原本签名是 `(in, out ApiFormat)`，而请求方向与响应
// 方向的 in/out 含义正好相反——请求是「下游 → 上游」，响应是「上游 → 下游」。
// 两个参数同型、可互换，写反不会报错，只会安静地产生错协议的响应体（曾真实发生：
// 转换函数的参数顺序被写反，导致 Messages 入口收到 chat.completion 形态的响应）。
//
// 现在的做法是把「方向」变成一个有类型、有角色的对象：代理层每次请求只构造一次
// Hop，请求与响应共用同一个对象，方向不再由调用点逐个拼装，因此不存在写反的可能。
// 角色类型 ClientFormat / ChannelFormat 让「下游协议」与「上游协议」在编译期就
// 无法互换，矩阵校验也只在构造时做一次。

// ClientFormat 是「下游协议」——请求方（入口）使用的协议。
type ClientFormat ApiFormat

// ChannelFormat 是「上游协议」——渠道（上游服务）使用的协议。
//
// 与 ClientFormat 是两个不同的类型，把二者的值写反无法通过编译。
type ChannelFormat ApiFormat

// AsClient 标注一个格式在本次转换中的角色：下游（请求方）协议。
func (f ApiFormat) AsClient() ClientFormat { return ClientFormat(f) }

// AsChannel 标注一个格式在本次转换中的角色：上游（渠道）协议。
func (f ApiFormat) AsChannel() ChannelFormat { return ChannelFormat(f) }

// Format 取回底层格式标识。
func (c ClientFormat) Format() ApiFormat { return ApiFormat(c) }

// Format 取回底层格式标识。
func (c ChannelFormat) Format() ApiFormat { return ApiFormat(c) }

// Hop 是一次「单跳」转换：把一个请求的入口协议与目标渠道协议绑定在一起，
// 并持有该请求的转换状态（session，namespace / custom 工具的反向映射）。
//
// 生命周期与一次被代理的请求一致：代理层在选定渠道后构造，请求方向与响应方向
// 都用同一个实例——这正是方向不会被写反的原因。
type Hop struct {
	plan    Plan
	session *convSession
}

// NewHop 构造一次转换。任意协议组合都能规划出路径（见 plan.go），因此不会失败。
func NewHop(client ClientFormat, channel ChannelFormat) *Hop {
	return &Hop{plan: NewPlan(client.Format(), channel.Format()), session: newConvSession()}
}

// Client 返回下游（请求方）协议。
func (h *Hop) Client() ApiFormat { return h.plan.Client() }

// Channel 返回上游（渠道）协议。
func (h *Hop) Channel() ApiFormat { return h.plan.Channel() }

// Plan 返回本次请求的协议路径（供日志与测试观察实际跳数）。
func (h *Hop) Plan() Plan { return h.plan }

// Notes 返回本次转换里「有损但可用」的处理说明（丢弃的字段、补的默认值、被规整的形态）。
//
// 用途：有损可以接受，但必须能定位到是哪一跳丢的什么——代理层在请求结束时一次性打印。
// 真正无法表达、会改变调用方语义的字段不走这里，而是直接返回 *UnsupportedFieldError。
func (h *Hop) Notes() []string {
	out := make([]string, len(h.session.notes))
	copy(out, h.session.notes)
	return out
}

// Converts 报告本次请求是否需要协议转换。
// false 表示上下游同协议，代理层只替换模型名后字节透传。
func (h *Hop) Converts() bool { return h.plan.Converts() }

// BuildUpstreamRequest 把下游请求体转换为上游请求体（方向：下游 → 上游）。
func (h *Hop) BuildUpstreamRequest(body []byte, opts RequestOptions) ([]byte, error) {
	return h.session.buildUpstreamRequest(h.plan, body, opts)
}

// NonStreamResponse 把上游非流式响应体转换为下游响应体（方向：上游 → 下游）。
func (h *Hop) NonStreamResponse(body []byte, model string) ([]byte, error) {
	return h.session.convertNonStreamResponse(h.plan, body, model)
}

// StreamResponse 读取上游 SSE（r），写出下游 SSE（w）——方向同样是上游 → 下游。
// flush 非 nil 时，每写出一个下游事件后调用一次。
// 返回累计观察到的**上游** usage（Anthropic 口径，缓存命中单列，见 Usage）。
//
// 实现是一条按路径组装的流水线（见 pipeline.go）：单跳是一个阶段，两跳是两个阶段，
// 逐事件翻译、真增量、不整段缓冲。
func (h *Hop) StreamResponse(r io.Reader, w io.Writer, model string, flush func()) (Usage, error) {
	pipeline := newStreamPipeline(h.plan, model, h.session.nsReverse)
	if err := pipeline.run(r, w, flush); err != nil {
		return pipeline.usage(), err
	}
	return pipeline.usage(), nil
}

// ReplayResponseAsSSE 把上游**完整的非 SSE JSON 响应**重放成下游 SSE 事件流。
// 代理在上游对「流式请求」返回了单个 JSON 体时使用它。
//
// 上下游同协议时不做转换，仅按入口格式重放。
func (h *Hop) ReplayResponseAsSSE(body []byte, model string) ([]byte, error) {
	return h.session.fullResponseToSSE(h.plan, body, model)
}
