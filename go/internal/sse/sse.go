// Package sse 提供 SSE（Server-Sent Events）的分帧与解析。
//
// 收敛的原因：此前有三份独立实现——converter 的流式扫描（scanSSE）、
// audit 的响应拼装（sseData）、proxy 的流式错误探测（sseDataLine）。
// 三者对「事件边界、data 行拼接、[DONE] 语义」的理解必须完全一致，
// 否则同一条上游流在不同环节会被解析成不同结果（例如 usage 或错误事件被漏掉），
// 而这些规则属于协议细节，只应有一处维护。
//
// 语义取自 ocgo main.go::readSSE（line 2856），并保留本仓库原有的健壮性：
// 按行缓冲而非按 token，事件被任意 chunk 边界切开都能正确组帧。
package sse

import (
	"bufio"
	"io"
	"strings"
)

// Done 是 SSE 流结束标志的载荷。
const Done = "[DONE]"

// Event 是一个已分帧的 SSE 事件。
type Event struct {
	// Event 是 `event:` 行的值，未给出时为空串。
	Event string
	// Data 是全部 `data:` 行按 "\n" 拼接后的载荷（可能为 "[DONE]"）。
	Data string
}

// IsDone 判断载荷是否为流结束标志。
func IsDone(data string) bool { return data == Done }

// Data 取出一行中的 data 载荷。
//
// 字段名必须恰好是 `data`：`database: x` 这类不算（SSE 规范里字段名以冒号结束）。
// 前缀后的空白按规范去除。
func Data(line string) (string, bool) {
	rest, ok := strings.CutPrefix(line, "data:")
	if !ok {
		return "", false
	}
	return strings.TrimSpace(rest), true
}

// Scan 按行扫描 SSE 并逐个交付完整事件。
//
// 语义（与 ocgo readSSE 一致）：
//   - 空行结束一个事件；只有 `data:` 行的事件才交付（纯 `event:` 行不成事件）；
//   - 同一事件内的多条 `data:` 行按 "\n" 拼接为载荷；
//   - 载荷为 `[DONE]` 时结束扫描，且**不**交付给 handle（收尾由调用方的 finalize 负责）；
//   - handle 返回错误时立即中止并原样返回该错误；
//   - 输入读完（EOF）时，缓冲区里未以空行结尾的事件照常交付。
func Scan(r io.Reader, handle func(Event) error) error {
	br := bufio.NewReader(r)
	event := ""
	var data []string

	// flush 交付一个完整事件；返回 (是否结束扫描, 错误)。
	flush := func() (bool, error) {
		if len(data) == 0 {
			event = ""
			return false, nil
		}
		payload := strings.Join(data, "\n")
		data = nil
		ev := Event{Event: event, Data: payload}
		event = ""
		if IsDone(payload) {
			return true, nil
		}
		if err := handle(ev); err != nil {
			return true, err
		}
		return false, nil
	}

	for {
		line, err := br.ReadString('\n')
		if line != "" {
			trimmed := strings.TrimRight(line, "\n")
			trimmed = strings.TrimRight(trimmed, "\r")
			switch {
			case trimmed == "":
				stop, herr := flush()
				if herr != nil {
					return herr
				}
				if stop {
					return nil
				}
			case strings.HasPrefix(trimmed, "event:"):
				event = strings.TrimSpace(trimmed[len("event:"):])
			case strings.HasPrefix(trimmed, "data:"):
				if payload, ok := Data(trimmed); ok {
					data = append(data, payload)
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			return err
		}
	}
	if _, herr := flush(); herr != nil {
		return herr
	}
	return nil
}

// Events 把整段 SSE 文本解析为事件切片（供不需要流式处理的调用方使用，
// 例如审计层把上游原始流拼装成完整响应）。
func Events(raw string) []Event {
	var out []Event
	_ = Scan(strings.NewReader(raw), func(ev Event) error {
		out = append(out, ev)
		return nil
	})
	return out
}
