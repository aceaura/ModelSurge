package ir

import "encoding/json"

// Response 非流式完整响应（由事件流聚合而成）。
type Response struct {
	ID         string
	Model      string
	Content    []Block
	StopReason StopReason
	// StopSequence StopStopSequence 档命中的那条序列原文；其余档为空。
	StopSequence string
	Usage        Usage
}

// Aggregator 把 IR 事件流聚合成完整 Response。
// 用于"上游永远流式、客户端要非流式"的缓冲聚合路径。
type Aggregator struct {
	resp    Response
	open    map[int]*Block // index -> 构建中的块
	rawJSON map[int]*jsonRawBuilder
	started bool
	stopped bool
	err     *Error
}

type jsonRawBuilder struct{ buf []byte }

func (b *jsonRawBuilder) append(fragment string) { b.buf = append(b.buf, fragment...) }

// NewAggregator 创建聚合器。
func NewAggregator() *Aggregator {
	return &Aggregator{
		open:    make(map[int]*Block),
		rawJSON: make(map[int]*jsonRawBuilder),
	}
}

// Feed 消费一个事件。返回 false 表示流已终止（message_stop 或 error）。
func (a *Aggregator) Feed(ev Event) bool {
	switch ev.Type {
	case EvMessageStart:
		a.started = true
		a.resp.ID = ev.MessageID
		a.resp.Model = ev.Model
		if ev.Usage != nil {
			a.resp.Usage.MergeNonZero(*ev.Usage)
		}
	case EvBlockStart:
		if ev.Block == nil {
			break
		}
		b := *ev.Block
		if b.Type == BlockToolUse && b.ToolUse != nil {
			a.rawJSON[ev.Index] = &jsonRawBuilder{}
			b.ToolUse.Input = nil
		}
		a.open[ev.Index] = &b
	case EvTextDelta:
		// BlockRefusal 也用 Text 承载，同样接收 text delta；只认 BlockText
		// 会让拒绝正文在聚合路径（上游流式、客户端非流式）里静默清空。
		if b := a.open[ev.Index]; b != nil && (b.Type == BlockText || b.Type == BlockRefusal) {
			b.Text += ev.Text
		}
	case EvCitation:
		// 引用随正文之后到达（Anthropic 的 citations_delta、Gemini 的
		// groundingMetadata 都在文本之后），必须累到已开的块上——落到新块会
		// 让客户端多出一个空文本块，而标注与正文分离后偏移量全部失效。
		if b := a.open[ev.Index]; b != nil {
			b.Citations = DedupeCitations(append(b.Citations, ev.Citations...))
		}
	case EvThinkingDelta:
		if b := a.open[ev.Index]; b != nil && b.Type == BlockThinking && b.Thinking != nil {
			b.Thinking.Text += ev.Text
		}
	case EvSigDelta:
		if b := a.open[ev.Index]; b != nil && b.Type == BlockThinking && b.Thinking != nil {
			b.Thinking.Signature += ev.Text
			// 来源随首片确定，后续片是同一签名的续传，不覆盖。
			if b.Thinking.SignatureFrom == "" {
				b.Thinking.SignatureFrom = ev.SignatureFrom
			}
		}
	case EvToolInput:
		if rb := a.rawJSON[ev.Index]; rb != nil {
			rb.append(ev.Text)
		}
	case EvBlockStop:
		if b := a.open[ev.Index]; b != nil {
			if rb := a.rawJSON[ev.Index]; rb != nil && b.ToolUse != nil {
				b.ToolUse.Input = normalizeJSON(rb.buf)
			}
			a.resp.Content = append(a.resp.Content, *b)
			delete(a.open, ev.Index)
			delete(a.rawJSON, ev.Index)
		}
	case EvMessageDelta:
		a.resp.StopReason = ev.StopReason
		if ev.StopSequence != "" {
			a.resp.StopSequence = ev.StopSequence
		}
		if ev.Usage != nil {
			a.resp.Usage.MergeNonZero(*ev.Usage)
		}
	case EvMessageStop:
		a.stopped = true
		return false
	case EvError:
		a.err = ev.Err
		return false
	}
	return true
}

// Finish 冲刷未闭合的块并返回聚合结果。
// 异常断流时也应调用，保证产出尽量完整的响应。
func (a *Aggregator) Finish() (*Response, *Error) {
	// 按 index 顺序冲刷残余块（map 无序，收集后排序）
	idxs := make([]int, 0, len(a.open))
	for i := range a.open {
		idxs = append(idxs, i)
	}
	for i := 0; i < len(idxs); i++ {
		for j := i + 1; j < len(idxs); j++ {
			if idxs[j] < idxs[i] {
				idxs[i], idxs[j] = idxs[j], idxs[i]
			}
		}
	}
	for _, i := range idxs {
		b := a.open[i]
		if rb := a.rawJSON[i]; rb != nil && b.ToolUse != nil {
			b.ToolUse.Input = normalizeJSON(rb.buf)
		}
		a.resp.Content = append(a.resp.Content, *b)
	}
	a.open = map[int]*Block{}
	a.rawJSON = map[int]*jsonRawBuilder{}
	return &a.resp, a.err
}

// normalizeJSON 把累积的 JSON 片段规整为合法 JSON；解析失败时返回 {}。
func normalizeJSON(raw []byte) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage(`{}`)
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return json.RawMessage(`{}`)
	}
	out, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return out
}
