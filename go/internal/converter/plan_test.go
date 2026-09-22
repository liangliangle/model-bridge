package converter

import (
	"strings"
	"testing"
)

// 路径规划（plan.go）的测试。
//
// 矩阵全开之后，「哪两个协议之间能不能转」这个问题消失了，取而代之的是
// 「走哪条路径」。这里把 9 种组合的路径与跳数逐格钉住，包括刻意经 Chat 串联的
// 两条两跳路径——它是「补 4 个单向映射即可覆盖全部 9 格」这一设计的落点。

func TestNewPlanPaths(t *testing.T) {
	cases := []struct {
		name    string
		client  ApiFormat
		channel ApiFormat
		path    string
		hops    int
		convert bool
	}{
		{"chat 透传", FormatOpenAIChat, FormatOpenAIChat, "chat", 0, false},
		{"messages 透传", FormatAnthropic, FormatAnthropic, "messages", 0, false},
		{"responses 透传", FormatResponses, FormatResponses, "responses", 0, false},

		{"messages → chat（单跳）", FormatAnthropic, FormatOpenAIChat, "messages -> chat", 1, true},
		{"responses → chat（单跳）", FormatResponses, FormatOpenAIChat, "responses -> chat", 1, true},
		{"chat → messages（单跳）", FormatOpenAIChat, FormatAnthropic, "chat -> messages", 1, true},
		{"chat → responses（单跳）", FormatOpenAIChat, FormatResponses, "chat -> responses", 1, true},

		{"messages → responses（经 chat 两跳）", FormatAnthropic, FormatResponses, "messages -> chat -> responses", 2, true},
		{"responses → messages（经 chat 两跳）", FormatResponses, FormatAnthropic, "responses -> chat -> messages", 2, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan := NewPlan(tc.client, tc.channel)
			if got := plan.String(); got != tc.path {
				t.Fatalf("路径 = %q, want %q", got, tc.path)
			}
			if got := plan.Hops(); got != tc.hops {
				t.Fatalf("跳数 = %d, want %d", got, tc.hops)
			}
			if got := plan.Converts(); got != tc.convert {
				t.Fatalf("Converts = %v, want %v", got, tc.convert)
			}
			if plan.Client() != tc.client || plan.Channel() != tc.channel {
				t.Fatalf("首尾应为 %v → %v，实际 %v → %v", tc.client, tc.channel, plan.Client(), plan.Channel())
			}
			// 路径首尾就是客户端与渠道，中间只能是 Hub。
			steps := plan.Steps()
			if len(steps) != tc.hops+1 {
				t.Fatalf("路径长度 = %d, want %d", len(steps), tc.hops+1)
			}
			for i := 1; i < len(steps)-1; i++ {
				if steps[i] != Hub {
					t.Fatalf("中间枢纽必须是 chat，实际 %v（路径 %v）", steps[i], plan)
				}
			}
		})
	}
}

// TestPlanEdges 固定两个方向的边序列：请求方向是路径正向，响应方向是它的逆序。
// 这两组边就是代理层实际施加的映射顺序，写反会让响应体变成错协议。
func TestPlanEdges(t *testing.T) {
	cases := []struct {
		client, channel ApiFormat
		request         []string
		response        []string
	}{
		{
			FormatOpenAIChat, FormatOpenAIChat,
			nil, nil,
		},
		{
			FormatAnthropic, FormatOpenAIChat,
			[]string{"messages→chat"},
			[]string{"chat→messages"},
		},
		{
			FormatOpenAIChat, FormatAnthropic,
			[]string{"chat→messages"},
			[]string{"messages→chat"},
		},
		{
			FormatAnthropic, FormatResponses,
			[]string{"messages→chat", "chat→responses"},
			[]string{"responses→chat", "chat→messages"},
		},
		{
			FormatResponses, FormatAnthropic,
			[]string{"responses→chat", "chat→messages"},
			[]string{"messages→chat", "chat→responses"},
		},
	}

	for _, tc := range cases {
		t.Run(NewPlan(tc.client, tc.channel).String(), func(t *testing.T) {
			plan := NewPlan(tc.client, tc.channel)
			if got := formatEdges(plan.RequestEdges()); got != strings.Join(tc.request, ",") {
				t.Fatalf("请求方向边 = %q, want %q", got, strings.Join(tc.request, ","))
			}
			if got := formatEdges(plan.ResponseEdges()); got != strings.Join(tc.response, ",") {
				t.Fatalf("响应方向边 = %q, want %q", got, strings.Join(tc.response, ","))
			}
		})
	}
}

// TestPlanStepsIsCopy 防止调用方通过 Steps 改到内部路径。
func TestPlanStepsIsCopy(t *testing.T) {
	plan := NewPlan(FormatAnthropic, FormatResponses)
	steps := plan.Steps()
	steps[1] = FormatAnthropic
	if got := plan.String(); got != "messages -> chat -> responses" {
		t.Fatalf("Steps 应当返回副本，实际路径被改成 %q", got)
	}
}

func formatEdges(edges []Edge) string {
	parts := make([]string, 0, len(edges))
	for _, e := range edges {
		parts = append(parts, e.From.String()+"→"+e.To.String())
	}
	return strings.Join(parts, ",")
}
