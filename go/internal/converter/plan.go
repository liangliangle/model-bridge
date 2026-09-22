package converter

// 本文件把「协议转换矩阵」从一条布尔判定升级为一条**路径**。
//
// 背景：原来的矩阵只有两种可能——同协议（透传）或渠道是 Chat（一跳转换）。
// 现在支持任意组合互转，于是每次请求的转换形态变成一条路径：
//
//	client == channel        → [client]                     0 跳（字节透传）
//	其一为 Chat              → [client, channel]            1 跳
//	两者都是富协议且不同      → [client, Chat, channel]       2 跳（经 Chat 串联）
//
// 为什么仍然经 Chat 中转而不是直连两个富协议：Chat 是唯一被验证过的中枢，两个富协议
// 之间的直连映射会引入第三条独立实现；拆成两条已验证的单跳映射复合，行为可复用、可单测。
//
// 路径一旦确定，请求方向与响应方向都由它折叠得出，调用点不再自己拼装方向——
// 这正是 hop.go 要消除的「方向写反」隐患（见 hop.go 顶部）。

// Hub 是转换中枢：任何两个不同协议之间的转换都经它中转。
// 对应 Rust 契约里「Chat Completions 是唯一转换枢纽」。
const Hub = FormatOpenAIChat

// Edge 是一个单跳方向（源协议 → 目标协议）。
type Edge struct {
	From ApiFormat
	To   ApiFormat
}

// Plan 是一次请求的协议路径：steps[0] 是客户端（入口）协议，
// steps[len-1] 是渠道（上游）协议，中间最多一个 Hub。
type Plan struct {
	steps []ApiFormat
}

// NewPlan 规划一次请求的协议路径。任意组合都有路径，因此不会失败。
func NewPlan(client, channel ApiFormat) Plan {
	switch {
	case client == channel:
		return Plan{steps: []ApiFormat{client}}
	case client == Hub || channel == Hub:
		return Plan{steps: []ApiFormat{client, channel}}
	default:
		return Plan{steps: []ApiFormat{client, Hub, channel}}
	}
}

// Client 返回客户端（入口）协议。
func (p Plan) Client() ApiFormat { return p.steps[0] }

// Channel 返回渠道（上游）协议。
func (p Plan) Channel() ApiFormat { return p.steps[len(p.steps)-1] }

// Steps 返回完整路径（含首尾）。
func (p Plan) Steps() []ApiFormat {
	out := make([]ApiFormat, len(p.steps))
	copy(out, p.steps)
	return out
}

// Converts 报告本次请求是否需要协议转换（上下游协议是否不同）。
func (p Plan) Converts() bool { return p.Client() != p.Channel() }

// Hops 返回转换跳数：0 = 透传，1 = 单跳，2 = 经 Chat 串联。
func (p Plan) Hops() int { return len(p.steps) - 1 }

// RequestEdges 返回请求方向要依次施加的单跳（客户端 → 渠道）。
func (p Plan) RequestEdges() []Edge {
	edges := make([]Edge, 0, len(p.steps)-1)
	for i := 0; i+1 < len(p.steps); i++ {
		edges = append(edges, Edge{From: p.steps[i], To: p.steps[i+1]})
	}
	return edges
}

// ResponseEdges 返回响应方向要依次施加的单跳（渠道 → 客户端，即请求方向的逆序）。
func (p Plan) ResponseEdges() []Edge {
	edges := make([]Edge, 0, len(p.steps)-1)
	for i := len(p.steps) - 1; i > 0; i-- {
		edges = append(edges, Edge{From: p.steps[i], To: p.steps[i-1]})
	}
	return edges
}

// String 返回可读路径，用于日志与测试失败信息（例如 "messages -> chat -> responses"）。
func (p Plan) String() string {
	out := ""
	for i, step := range p.steps {
		if i > 0 {
			out += " -> "
		}
		out += step.String()
	}
	return out
}
