// eventstream.go Kiro 流的原始字节 -> JSON 事件提取
// （KiroaaS parsers.py AwsEventStreamParser.feed 的 Go 翻译）。
// Kiro 上游返回 AWS eventstream 二进制帧 + JSON 载荷；这里不解析帧头/CRC，
// 按文本扫描 JSON 模式 + 括号配对提取载荷——JSON 模式命中后，
// 模式之前的二进制噪声连同事件一起被丢弃（与 Python 实现语义一致）。
package kiro

import (
	"bytes"
	"encoding/json"
	"io"
	"regexp"
)

// kiroRawEvent 从流中提取的一条 Kiro 事件（kind 判别 + 原始 JSON）。
type kiroRawEvent struct {
	kind string // content / tool_start / tool_input / tool_stop / thinking / thinking_signature / usage / context_usage
	data string // 完整 JSON 字符串
}

// eventPatterns Kiro 事件 JSON 的识别前缀（扫描取最早出现者）。
var eventPatterns = []struct {
	prefix string
	kind   string
}{
	{`{"content":`, "content"},
	{`{"name":`, "tool_start"},
	{`{"input":`, "tool_input"},
	{`{"stop":`, "tool_stop"},
	{`{"followupPrompt":`, "followup"},
	{`{"usage":`, "usage"},
	{`{"contextUsagePercentage":`, "context_usage"},
	{`{"text":`, "thinking"},
	{`{"signature":`, "thinking_signature"},
}

// eventStreamParser 增量提取 Kiro 事件（有状态：跨 chunk 缓冲 + 括号配对）。
type eventStreamParser struct {
	buf         []byte
	lastContent string // content 事件去重（Kiro 会重复发送相同内容）
	hasLast     bool
}

// feed 消费一块原始字节，返回其中完整可解析的事件。
func (p *eventStreamParser) feed(chunk []byte) []kiroRawEvent {
	p.buf = append(p.buf, chunk...)
	var events []kiroRawEvent
	for {
		earliestPos, earliestKind := -1, ""
		for _, pat := range eventPatterns {
			if pos := bytes.Index(p.buf, []byte(pat.prefix)); pos != -1 && (earliestPos == -1 || pos < earliestPos) {
				earliestPos, earliestKind = pos, pat.kind
			}
		}
		if earliestPos == -1 {
			break // 无完整模式：保留缓冲等下一块（含不完整的模式前缀）
		}
		end := findMatchingBrace(p.buf, earliestPos)
		if end == -1 {
			break // JSON 未闭合，等下一块
		}
		jsonStr := string(p.buf[earliestPos : end+1])
		p.buf = p.buf[end+1:]
		if ev, ok := p.classify(jsonStr, earliestKind); ok {
			events = append(events, ev)
		}
	}
	return events
}

// classify 解析 JSON 并判别事件类型（跳过 followup 与重复 content）。
func (p *eventStreamParser) classify(jsonStr, hint string) (kiroRawEvent, bool) {
	switch hint {
	case "followup":
		return kiroRawEvent{}, false
	case "content":
		var d struct {
			Content  string `json:"content"`
			Followup bool   `json:"followupPrompt"`
		}
		if err := json.Unmarshal([]byte(jsonStr), &d); err != nil {
			return kiroRawEvent{}, false
		}
		if d.Followup {
			return kiroRawEvent{}, false
		}
		if p.hasLast && d.Content == p.lastContent {
			return kiroRawEvent{}, false // 重复 content（Kiro 怪癖）
		}
		p.lastContent, p.hasLast = d.Content, true
		return kiroRawEvent{kind: "content", data: jsonStr}, true
	default:
		return kiroRawEvent{kind: hint, data: jsonStr}, true
	}
}

// findMatchingBrace 找配对右括号（考虑嵌套与字符串转义）。
// UTF-8 多字节序列不含 ASCII 结构字符，字节级扫描安全。
func findMatchingBrace(text []byte, start int) int {
	if start >= len(text) || text[start] != '{' {
		return -1
	}
	braceCount := 0
	inString := false
	escapeNext := false
	for i := start; i < len(text); i++ {
		c := text[i]
		if escapeNext {
			escapeNext = false
			continue
		}
		if c == '\\' && inString {
			escapeNext = true
			continue
		}
		if c == '"' {
			inString = !inString
			continue
		}
		if !inString {
			switch c {
			case '{':
				braceCount++
			case '}':
				braceCount--
				if braceCount == 0 {
					return i
				}
			}
		}
	}
	return -1
}

// ---- SSE 适配（relay 层消费） ----

// EventStreamToSSE 把 Kiro eventstream 包装成 SSE 字节流
// （每条事件一帧 data: {json}\n\n）。relay 的 EventReader 逐帧消费后
// 把 data 喂给 kiro streamDecoder.Feed——relay 管线保持协议无关。
type EventStreamToSSE struct {
	src     io.Reader
	parser  eventStreamParser
	pending []byte // 待吐出的 SSE 帧
	srcErr  error
	eof     bool
}

// NewEventStreamToSSE 包装上游响应体。
func NewEventStreamToSSE(r io.Reader) *EventStreamToSSE {
	return &EventStreamToSSE{src: r}
}

// WrapResponseBody 实现 relay 的可选 codec 缝：把上游响应体适配为
// relay 管线可消费的形态。kiro 上游返回 AWS eventstream 二进制，
// 包装为 SSE 字节流；relay 据此跳过"非 SSE 完整 JSON"兜底路径。
func (Codec) WrapResponseBody(r io.Reader) io.Reader { return NewEventStreamToSSE(r) }

// Read 实现 io.Reader：原始读取 -> 事件提取 -> 组帧吐出。
func (a *EventStreamToSSE) Read(p []byte) (int, error) {
	for {
		if n := len(a.pending); n > 0 {
			if n > len(p) {
				n = len(p)
			}
			copy(p, a.pending[:n])
			a.pending = a.pending[n:]
			return n, nil
		}
		if a.eof {
			return 0, io.EOF
		}
		chunk := make([]byte, 32*1024)
		n, err := a.src.Read(chunk)
		if n > 0 {
			for _, ev := range a.parser.feed(chunk[:n]) {
				a.pending = append(a.pending, "data: "...)
				a.pending = append(a.pending, ev.data...)
				a.pending = append(a.pending, "\n\n"...)
			}
		}
		if err != nil {
			a.eof = true
			a.srcErr = err
			if len(a.pending) == 0 && err != io.EOF {
				return 0, err
			}
			if len(a.pending) == 0 {
				return 0, io.EOF
			}
			// 先吐完待发帧，下一轮 Read 再返回 EOF/错误
		}
	}
}

// ---- 文本形态工具调用（部分模型用文本而非结构化事件） ----

var bracketCallRe = regexp.MustCompile(`(?i)\[Called\s+(\w+)\s+with\s+args:\s*`)

// bracketToolCall 文本形态工具调用。
type bracketToolCall struct {
	Name string
	Args json.RawMessage
}

// parseBracketToolCalls 从文本中提取 [Called func with args: {...}] 形态的
// 工具调用（KiroaaS parsers.py parse_bracket_tool_calls 翻译）。
func parseBracketToolCalls(text string) []bracketToolCall {
	if !bytes.Contains([]byte(text), []byte("[Called")) {
		return nil
	}
	var out []bracketToolCall
	for _, loc := range bracketCallRe.FindAllStringSubmatchIndex(text, -1) {
		name := text[loc[2]:loc[3]] // (\w+) 子组
		rest := text[loc[1]:]
		jsonStart := -1
		for i := 0; i < len(rest); i++ {
			if rest[i] == '{' {
				jsonStart = i
				break
			}
			if rest[i] == '\n' {
				break // 跨行的非 JSON 参数，放弃
			}
		}
		if jsonStart == -1 {
			continue
		}
		jsonEnd := findMatchingBrace([]byte(rest), jsonStart)
		if jsonEnd == -1 {
			continue
		}
		args := rest[jsonStart : jsonEnd+1]
		var probe any
		if err := json.Unmarshal([]byte(args), &probe); err != nil {
			continue
		}
		out = append(out, bracketToolCall{Name: name, Args: json.RawMessage(args)})
	}
	return out
}
