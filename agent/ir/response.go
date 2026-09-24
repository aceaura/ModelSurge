package ir

import (
	"encoding/json"
)

// StopDetails 拒绝停止的结构化分类（仅 anthropic：message_delta/响应的
// stop_details，type 恒 "refusal"）。其余协议无槽位，跨族出站不投影。
type StopDetails struct {
	// Category 触发拒绝的策略分类（cyber/bio）；上游显式 null 与缺省同归空串。
	Category string
	// Explanation 人类可读解释，官方注明文本不稳定。
	Explanation string
}

// Response 非流式完整响应（由事件流聚合而成）。
type Response struct {
	ID         string
	Model      string
	Content    []Block
	StopReason StopReason
	// StopSequence StopStopSequence 档命中的那条序列原文；其余档为空。
	StopSequence string
	// StopDetails 拒绝档的结构化分类；非拒绝或上游未给为 nil。
	StopDetails *StopDetails
	Usage       Usage
	// ServiceTier 上游回显的实际服务档位原值（anthropic standard/priority/
	// batch；OpenAI auto/default/flex/scale/priority/fast/ultrafast）。
	ServiceTier string
	// SystemFingerprint Chat 后端配置指纹回显。仅 chat 族有槽位。
	SystemFingerprint string
	// Container 实际使用的代码执行容器回显（仅 anthropic：id/expires_at/
	// 已加载技能）。nil = 上游没用容器。客户端要靠它复用容器续话。
	Container *Container
	// Audio Chat 非流式模型音频输出。流式 Chat delta 没有官方音频槽位；
	// 非 Chat 客户端也无法接收，编码边界必须丢弃并报告。
	Audio *AudioOutput
}

// Aggregator 把 IR 事件流聚合成完整 Response。
// 用于"上游永远流式、客户端要非流式"的缓冲聚合路径。
type Aggregator struct {
	resp    Response
	open    map[int]*Block // index -> 构建中的块
	rawJSON map[int]*jsonRawBuilder
	// badArgs 聚合时被挪进 RawArgsKey 的畸形工具参数数，Notes() 报出。
	badArgs int
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
		if ev.ServiceTier != "" {
			a.resp.ServiceTier = ev.ServiceTier
		}
		if ev.SystemFingerprint != "" {
			a.resp.SystemFingerprint = ev.SystemFingerprint
		}
		if ev.Container != nil {
			a.resp.Container = ev.Container
		}
		if ev.Audio != nil {
			a.resp.Audio = ev.Audio
		}
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
		// server_tool_use 的查询串同样走 EvToolInput 增量通道（anthropic 流式
		// 与 responses 合成的都是这个形态）：开块没带 input 时登记累积器，
		// 否则聚合结果里查询串整段蒸发。开块已带完整 input（非流式回放）
		// 的保持原样，不登记。
		if b.Type == BlockServerToolUse && b.ServerToolUse != nil && len(b.ServerToolUse.Input) == 0 {
			a.rawJSON[ev.Index] = &jsonRawBuilder{}
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
				if b.ToolUse.Kind == ToolCustom {
					b.ToolUse.InputText = string(rb.buf)
					b.ToolUse.Input = b.ToolUse.ObjectInput()
				} else {
					b.ToolUse.Input = a.normalizeArgs(rb.buf)
				}
			}
			// 托管调用的查询串是服务端产出的原文，不做参数规整（normalizeArgs
			// 面向客户端可执行的 function 参数；查询串原样保留才是保真）。
			if rb := a.rawJSON[ev.Index]; rb != nil && b.ServerToolUse != nil && len(rb.buf) > 0 {
				b.ServerToolUse.Input = json.RawMessage(rb.buf)
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
		if ev.StopDetails != nil {
			a.resp.StopDetails = ev.StopDetails
		}
		// chat 的 service_tier 可能到得比首帧晚（后续 chunk 才带），
		// 晚到的非空值补上；同值重复无害。
		if ev.ServiceTier != "" {
			a.resp.ServiceTier = ev.ServiceTier
		}
		// anthropic 的 container 回显也可能落在 message_delta 上。
		if ev.Container != nil {
			a.resp.Container = ev.Container
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
			if b.ToolUse.Kind == ToolCustom {
				b.ToolUse.InputText = string(rb.buf)
				b.ToolUse.Input = b.ToolUse.ObjectInput()
			} else {
				b.ToolUse.Input = a.normalizeArgs(rb.buf)
			}
		}
		a.resp.Content = append(a.resp.Content, *b)
	}
	a.open = map[int]*Block{}
	a.rawJSON = map[int]*jsonRawBuilder{}
	return &a.resp, a.err
}

// normalizeArgs 规整并记账：畸形/非对象参数挪进 RawArgsKey 时计数。
// 截断的流式参数（max_tokens 截断是最常见来源）与「对象槽位合法值」共享
// 同一规则：原文挪进 RawArgsKey，而不是凭空清空——空 {} 会让工具不带参数执行。
func (a *Aggregator) normalizeArgs(raw []byte) json.RawMessage {
	out, ok := NormalizeToolInput(raw)
	if !ok {
		a.badArgs++
	}
	return out
}

// Notes 排干聚合损耗注记。非流式客户端路径上畸形工具参数在聚合时
// 已被挪键（codec 看到的是规整后的合法对象），注记只能在这里产生。
func (a *Aggregator) Notes() []string {
	if a.badArgs == 0 {
		return nil
	}
	n := RewrapNote(a.badArgs)
	a.badArgs = 0
	return []string{n}
}
