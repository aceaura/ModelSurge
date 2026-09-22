// decode_response.go 非流式兜底与响应编码。
// DecodeResponse：上游忽略流式约定、一次性吐出完整 eventstream 字节时，
// 走全量解析——事件提取 -> streamDecoder -> ir.Aggregator 聚合。
// EncodeResponse：把 IR 响应编码回 Kiro 事件 JSON 序列（每行一条），
// 与 DecodeResponse 互逆，供任务组7 的 mock 上游复用。
package kiro

import (
	"bytes"
	"encoding/json"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// DecodeResponse 把完整 Kiro eventstream 响应体解析为 IR 响应。
// 响应体可能含二进制帧噪声，事件提取按文本扫描进行（与流式路径同源）。
func (Codec) DecodeResponse(body []byte) (*ir.Response, error) {
	dec := &streamDecoder{}
	var evs []ir.Event
	parser := eventStreamParser{}
	for _, raw := range parser.feed(body) {
		out, err := dec.Feed("", raw.data)
		if err != nil {
			return nil, err
		}
		evs = append(evs, out...)
	}
	evs = append(evs, dec.Finish()...)

	agg := ir.NewAggregator()
	for _, ev := range evs {
		if !agg.Feed(ev) {
			break
		}
	}
	resp, kerr := agg.Finish()
	if kerr != nil {
		return resp, &ir.Error{Type: kerr.Type, Message: kerr.Message}
	}
	return resp, nil
}

// 编码方向的事件载荷。字段顺序即 JSON 键序，必须以识别前缀开头
// （{"content": / {"name": / {"stop": / {"text": / {"signature": /
// {"usage": / {"contextUsagePercentage":）。
type (
	contentEventJSON struct {
		Content string `json:"content"`
	}
	toolStartEventJSON struct {
		Name      string          `json:"name"`
		ToolUseID string          `json:"toolUseId"`
		Input     json.RawMessage `json:"input"`
	}
	toolInputEventJSON struct {
		Input json.RawMessage `json:"input"`
	}
	toolStopEventJSON struct {
		Stop map[string]any `json:"stop"`
	}
	thinkingEventJSON struct {
		Text string `json:"text"`
	}
	signatureEventJSON struct {
		Signature string `json:"signature"`
	}
	usageEventJSON struct {
		Usage kiroUsageJSON `json:"usage"`
	}
	contextUsageEventJSON struct {
		ContextUsagePercentage float64 `json:"contextUsagePercentage"`
	}
)

// EncodeResponse 把 IR 响应编码为 Kiro 事件 JSON 序列（换行分隔）。
// StopReason 为 max_tokens 时不发 context_usage（保留截断信号），
// 其余情况以 context_usage 收尾表示流正常完成。
func (Codec) EncodeResponse(resp *ir.Response) ([]byte, error) {
	var buf bytes.Buffer
	w := func(v any) {
		b, err := json.Marshal(v)
		if err != nil {
			return
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	for _, b := range resp.Content {
		switch b.Type {
		case ir.BlockThinking:
			if b.Thinking == nil || b.Thinking.Text == "" {
				continue
			}
			w(thinkingEventJSON{Text: b.Thinking.Text})
			if b.Thinking.Signature != "" {
				w(signatureEventJSON{Signature: b.Thinking.Signature})
			}
		case ir.BlockText, ir.BlockRefusal:
			// kiro 载荷无 refusal 形态，正文并入 content 而不是丢弃。
			if b.Text == "" {
				continue
			}
			w(contentEventJSON{Content: b.Text})
		case ir.BlockToolUse:
			if b.ToolUse == nil {
				continue
			}
			input, _ := ir.NormalizeToolInput(b.ToolUse.Input)
			w(toolStartEventJSON{Name: b.ToolUse.Name, ToolUseID: b.ToolUse.ID, Input: json.RawMessage(`{}`)})
			w(toolInputEventJSON{Input: input})
			w(toolStopEventJSON{Stop: map[string]any{}})
		}
	}
	if resp.Usage.CacheReadTokens > 0 || resp.Usage.CacheCreationTokens > 0 {
		u := usageEventJSON{}
		if resp.Usage.CacheReadTokens > 0 {
			u.Usage.CacheReadInputTokens = ptrInt64(int64(resp.Usage.CacheReadTokens))
		}
		if resp.Usage.CacheCreationTokens > 0 {
			u.Usage.CacheCreationInputTokens = ptrInt64(int64(resp.Usage.CacheCreationTokens))
		}
		w(u)
	}
	if resp.StopReason != ir.StopMaxTokens {
		w(contextUsageEventJSON{ContextUsagePercentage: 0})
	}
	return buf.Bytes(), nil
}

func ptrInt64(v int64) *int64 { return &v }

// drainEvents 驱动解码器消费完整事件数据并返回全部 IR 事件。
// （测试与 DecodeResponse 共用的最小管线。）
func drainEvents(dec *streamDecoder, events []kiroRawEvent) ([]ir.Event, error) {
	var out []ir.Event
	for _, raw := range events {
		evs, err := dec.Feed("", raw.data)
		if err != nil {
			return nil, err
		}
		out = append(out, evs...)
	}
	return append(out, dec.Finish()...), nil
}
