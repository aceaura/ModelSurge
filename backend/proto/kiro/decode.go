// decode.go Kiro 事件 JSON -> IR 事件
// （KiroaaS streaming_core.py + streaming_anthropic.py 流式路径的 Go 翻译）。
// 事件经 EventStreamToSSE 适配器以 data={json} 帧送达（event 名为空）。
// 工具调用跨事件累积（tool_start/input/stop），在流末统一去重后以
// tool_use 块发出——先全部 thinking/text，后工具块，与参考实现一致。
package kiro

import (
	"encoding/json"
	"fmt"
	"strings"

	"relayd/backend/ir"
	"relayd/backend/proto"
)

// kiroEventJSON 一条 Kiro 事件的载荷（按字段存在性判别类型，
// 判别次序与 eventstream.go 的前缀扫描一致：name/stop/input 先于其余）。
type kiroEventJSON struct {
	Content                *string         `json:"content"`
	Followup               bool            `json:"followupPrompt"`
	Name                   *string         `json:"name"`
	ToolUseID              *string         `json:"toolUseId"`
	Input                  json.RawMessage `json:"input"`
	Stop                   json.RawMessage `json:"stop"`
	Text                   *string         `json:"text"`
	Signature              *string         `json:"signature"`
	Usage                  json.RawMessage `json:"usage"` // 对象（cache 字段）或数字（credits）
	ContextUsagePercentage *float64        `json:"contextUsagePercentage"`
}

// kiroUsageJSON usage 事件的用量载荷（字段名两种拼写并存）。
type kiroUsageJSON struct {
	CacheReadInputTokens      *int64 `json:"cache_read_input_tokens"`
	CacheReadInputTokensCamel *int64 `json:"cacheReadInputTokens"`
	CacheCreationInputTokens  *int64 `json:"cache_creation_input_tokens"`
	CacheCreationInputCamel   *int64 `json:"cacheCreationInputTokens"`
}

// ---- 工具调用累积 ----

// kiroToolCall 跨 tool_start/tool_input/tool_stop 事件累积的一次调用。
type kiroToolCall struct {
	ID          string
	Name        string // Kiro 侧名（可能是别名），发出时经 OriginalForToolName 还原
	args        string // 累积的参数 JSON 片段
	invalid     bool   // 参数解析失败
	truncated   bool   // 参数形似被上游截断
	truncReason string
}

// finalize 规整参数：空白/解析失败归为 "{}"，成功则原样保留（已验证合法）。
func (tc *kiroToolCall) finalize() {
	args := strings.TrimSpace(tc.args)
	if args == "" {
		tc.args = "{}"
		return
	}
	var probe any
	if err := json.Unmarshal([]byte(args), &probe); err != nil {
		tc.truncated, tc.truncReason = diagnoseJSONTruncation(args)
		tc.invalid = true
		tc.args = "{}"
		return
	}
	tc.args = args
}

// inputToString 把事件的 input 字段（字符串片段或对象）转为可追加的文本。
// 空对象返回 ""（后续片段将补全）；非空对象取紧凑 JSON 形态。
func inputToString(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return ""
	}
	if strings.HasPrefix(s, `"`) {
		var str string
		if err := json.Unmarshal([]byte(s), &str); err != nil {
			return ""
		}
		return str
	}
	if s == "{}" {
		return ""
	}
	return s
}

// diagnoseJSONTruncation 判断残缺 JSON 是否形似被截断
// （parsers.py _diagnose_json_truncation 翻译；截断恢复见任务组9）。
func diagnoseJSONTruncation(s string) (bool, string) {
	stripped := strings.TrimSpace(s)
	if stripped == "" {
		return false, "empty string"
	}
	openB := strings.Count(stripped, "{")
	closeB := strings.Count(stripped, "}")
	openS := strings.Count(stripped, "[")
	closeS := strings.Count(stripped, "]")
	if strings.HasPrefix(stripped, "{") && !strings.HasSuffix(stripped, "}") {
		return true, fmt.Sprintf("missing %d closing brace(s)", openB-closeB)
	}
	if strings.HasPrefix(stripped, "[") && !strings.HasSuffix(stripped, "]") {
		return true, fmt.Sprintf("missing %d closing bracket(s)", openS-closeS)
	}
	if openB != closeB {
		return true, fmt.Sprintf("unbalanced braces (%d open, %d close)", openB, closeB)
	}
	if openS != closeS {
		return true, fmt.Sprintf("unbalanced brackets (%d open, %d close)", openS, closeS)
	}
	quotes := 0
	for i := 0; i < len(stripped); i++ {
		if stripped[i] == '\\' && i+1 < len(stripped) {
			i++
			continue
		}
		if stripped[i] == '"' {
			quotes++
		}
	}
	if quotes%2 != 0 {
		return true, "unclosed string literal"
	}
	return false, "malformed JSON"
}

// dedupToolCalls 去重：同 ID 保留参数更完整者；再按 名字+参数 去完全重复
// （parsers.py deduplicate_tool_calls 翻译）。
func dedupToolCalls(calls []kiroToolCall) []kiroToolCall {
	byID := map[string]int{}
	var withID []kiroToolCall
	var noID []kiroToolCall
	for _, tc := range calls {
		if tc.ID == "" {
			noID = append(noID, tc)
			continue
		}
		if idx, ok := byID[tc.ID]; ok {
			ex := &withID[idx]
			if !tc.invalid && tc.args != "{}" &&
				(ex.invalid || ex.args == "{}" || len(tc.args) > len(ex.args)) {
				*ex = tc
			} else if tc.invalid && (ex.invalid || ex.args == "{}") {
				ex.invalid = true
			}
			continue
		}
		byID[tc.ID] = len(withID)
		withID = append(withID, tc)
	}
	seen := map[string]int{}
	var unique []kiroToolCall
	for _, tc := range append(withID, noID...) {
		key := tc.Name + "-" + tc.args
		if _, ok := seen[key]; !ok {
			seen[key] = len(unique)
			unique = append(unique, tc)
		} else if tc.invalid && tc.args == "{}" {
			unique[seen[key]].invalid = true
		}
	}
	return unique
}

// ---- streamDecoder ----

// streamDecoder Kiro 事件序列 -> IR 事件。
type streamDecoder struct {
	started  bool
	finished bool
	msgID    string
	model    string // Kiro 事件不携带模型名，由 relay 注入（SetModel）

	maxInput int // context_usage 换算用输入上限；0 = DefaultMaxInputTokens

	nextIndex       int
	thinkingOpen    bool
	thinkingIndex   int
	thinkingFakeSig string // fake reasoning 伪造签名，块关闭时补发
	thinkingSigSent bool
	textOpen        bool
	textIndex       int

	tp *thinkingParser // opt.FakeReasoning 时的标签解析器

	current *kiroToolCall
	tools   []kiroToolCall

	fullContent  strings.Builder
	fullThinking strings.Builder

	cacheRead     int64
	cacheCreation int64
	contextPct    *float64
	usageSeen     bool
}

// NewStreamDecoder 实现 proto.Codec。
func (Codec) NewStreamDecoder() proto.StreamDecoder { return &streamDecoder{} }

// SetModel 注入响应模型名（Kiro 上游不回显模型，message_start 需要它）。
func (d *streamDecoder) SetModel(m string) { d.model = m }

// SetMaxInputTokens 注入模型输入上限（context_usage -> input 换算用）。
func (d *streamDecoder) SetMaxInputTokens(n int) { d.maxInput = n }

// ContextUsage 返回上游上报的上下文占用百分比（任务组7 relay 换算/记账用）。
func (d *streamDecoder) ContextUsage() (float64, bool) {
	if d.contextPct == nil {
		return 0, false
	}
	return *d.contextPct, true
}

// TruncatedTools 返回被截断的工具调用（proto.TruncationReporter 缝）。
func (d *streamDecoder) TruncatedTools() []proto.TruncatedTool {
	var out []proto.TruncatedTool
	for _, tc := range d.tools {
		if tc.truncated {
			out = append(out, proto.TruncatedTool{
				ID: tc.ID, Name: OriginalForToolName(tc.Name), Reason: tc.truncReason,
			})
		}
	}
	return out
}

// ContentTruncated 流无完成信号但已有正文（proto.TruncationReporter 缝；
// Finish 后有效——此时括号工具已解析、去重完成）。
func (d *streamDecoder) ContentTruncated() bool {
	return !d.usageSeen && d.contextPct == nil &&
		d.fullContent.Len() > 0 && len(d.tools) == 0
}

// TruncatedContent 被截断的正文全文（内容哈希关联下次请求用）。
func (d *streamDecoder) TruncatedContent() string { return d.fullContent.String() }

// Feed 消费一条 Kiro JSON 事件（event 名为空，data 为事件 JSON）。
func (d *streamDecoder) Feed(_, data string) ([]ir.Event, error) {
	if data == "" || data == "[DONE]" {
		return nil, nil
	}
	var ev kiroEventJSON
	if err := json.Unmarshal([]byte(data), &ev); err != nil {
		return nil, fmt.Errorf("kiro: decode stream event: %w", err)
	}
	var out []ir.Event
	d.ensureStarted(&out)
	switch {
	case ev.Name != nil:
		d.onToolStart(ev)
	case len(ev.Stop) != 0:
		if d.current != nil && truthyRaw(ev.Stop) {
			d.finalizeTool()
		}
	case len(ev.Input) != 0:
		if d.current != nil {
			d.current.args += inputToString(ev.Input)
		}
	case ev.Content != nil && !ev.Followup:
		d.onContent(&out, *ev.Content)
	case ev.Text != nil:
		d.onThinking(&out, *ev.Text)
	case ev.Signature != nil:
		if d.thinkingOpen && !d.thinkingSigSent && *ev.Signature != "" {
			out = append(out, ir.Event{Type: ir.EvSigDelta, Index: d.thinkingIndex, Text: *ev.Signature})
			d.thinkingSigSent = true
		}
	case len(ev.Usage) != 0:
		d.onUsage(ev.Usage)
	case ev.ContextUsagePercentage != nil:
		d.contextPct = ev.ContextUsagePercentage
	}
	return out, nil
}

func (d *streamDecoder) ensureStarted(out *[]ir.Event) {
	if d.started {
		return
	}
	d.started = true
	d.msgID = "msg_" + randomHex(24)
	if opt.FakeReasoning {
		d.tp = newThinkingParser()
	}
	*out = append(*out, ir.Event{Type: ir.EvMessageStart, MessageID: d.msgID, Model: d.model})
}

// onContent 正文事件：fake reasoning 开启时先过标签解析器，
// 解析出的思考增量走 thinking 块，剩余正文走 text 块。
func (d *streamDecoder) onContent(out *[]ir.Event, text string) {
	if d.tp != nil {
		res := d.tp.feed(text)
		if res.thinkingContent != "" {
			d.emitThinkingDelta(out, res.thinkingContent)
		}
		if res.regularContent == "" {
			return
		}
		text = res.regularContent
	}
	d.fullContent.WriteString(text)
	d.closeThinkingBlock(out)
	d.ensureTextOpen(out)
	if text != "" {
		*out = append(*out, ir.Event{Type: ir.EvTextDelta, Index: d.textIndex, Text: text})
	}
}

// onThinking 原生思考事件（{"text": 帧，绕过标签解析器）。
func (d *streamDecoder) onThinking(out *[]ir.Event, text string) {
	if text == "" {
		return
	}
	d.fullThinking.WriteString(text)
	d.emitThinkingDelta(out, text)
}

func (d *streamDecoder) emitThinkingDelta(out *[]ir.Event, text string) {
	if !d.thinkingOpen {
		d.thinkingOpen = true
		d.thinkingIndex = d.nextIndex
		d.nextIndex++
		d.thinkingSigSent = false
		d.thinkingFakeSig = "sig_" + randomHex(32)
		*out = append(*out, ir.Event{
			Type:  ir.EvBlockStart,
			Index: d.thinkingIndex,
			Block: &ir.Block{Type: ir.BlockThinking, Thinking: &ir.Thinking{}},
		})
	}
	*out = append(*out, ir.Event{Type: ir.EvThinkingDelta, Index: d.thinkingIndex, Text: text})
}

func (d *streamDecoder) closeThinkingBlock(out *[]ir.Event) {
	if !d.thinkingOpen {
		return
	}
	if !d.thinkingSigSent && d.thinkingFakeSig != "" {
		*out = append(*out, ir.Event{Type: ir.EvSigDelta, Index: d.thinkingIndex, Text: d.thinkingFakeSig})
	}
	*out = append(*out, ir.Event{Type: ir.EvBlockStop, Index: d.thinkingIndex})
	d.thinkingOpen = false
}

func (d *streamDecoder) ensureTextOpen(out *[]ir.Event) {
	if d.textOpen {
		return
	}
	d.textOpen = true
	d.textIndex = d.nextIndex
	d.nextIndex++
	*out = append(*out, ir.Event{
		Type:  ir.EvBlockStart,
		Index: d.textIndex,
		Block: &ir.Block{Type: ir.BlockText},
	})
}

func (d *streamDecoder) onToolStart(ev kiroEventJSON) {
	if d.current != nil {
		d.finalizeTool()
	}
	tc := &kiroToolCall{Name: "", args: ""}
	if ev.ToolUseID != nil && *ev.ToolUseID != "" {
		tc.ID = *ev.ToolUseID
	} else {
		tc.ID = "toolu_" + randomHex(24)
	}
	if ev.Name != nil {
		tc.Name = *ev.Name
	}
	tc.args = inputToString(ev.Input)
	d.current = tc
	if len(ev.Stop) != 0 && truthyRaw(ev.Stop) {
		d.finalizeTool()
	}
}

func (d *streamDecoder) finalizeTool() {
	if d.current == nil {
		return
	}
	d.current.finalize()
	d.tools = append(d.tools, *d.current)
	d.current = nil
}

// onUsage 提取 cache 用量字段。载荷可能是对象（cache 字段）或数字
// （credits），数字形态仅作完成信号，字段忽略（与 Python 行为一致）。
func (d *streamDecoder) onUsage(raw json.RawMessage) {
	d.usageSeen = true
	var u kiroUsageJSON
	if err := json.Unmarshal(raw, &u); err != nil {
		return
	}
	if v := u.CacheReadInputTokens; v != nil {
		d.cacheRead = *v
	}
	if v := u.CacheReadInputTokensCamel; v != nil {
		d.cacheRead = *v
	}
	if v := u.CacheCreationInputTokens; v != nil {
		d.cacheCreation = *v
	}
	if v := u.CacheCreationInputCamel; v != nil {
		d.cacheCreation = *v
	}
}

// truthyRaw 判断 stop 字段是否为真值（{"stop": true} 或非空对象）。
func truthyRaw(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s != "" && s != "false" && s != "null" && s != "0" && s != "{}"
}

// Finish 收尾：冲刷 thinking 标签解析器、关闭残余块、发出累积的工具块、
// 判定停止原因（截断 > 工具 > 正常）并补 EvMessageDelta + EvMessageStop。
// 幂等；空流也产出完整事件骨架。
func (d *streamDecoder) Finish() []ir.Event {
	var out []ir.Event
	if d.finished {
		return nil
	}
	d.finished = true
	d.ensureStarted(&out)

	if d.tp != nil {
		res := d.tp.finalize()
		if res.thinkingContent != "" {
			d.emitThinkingDelta(&out, res.thinkingContent)
		}
		if res.regularContent != "" {
			d.onContent(&out, res.regularContent)
		}
	}
	d.finalizeTool()

	bracketCalls := parseBracketToolCalls(d.fullContent.String())
	for _, bc := range bracketCalls {
		tc := kiroToolCall{ID: "toolu_" + randomHex(24), Name: bc.Name, args: string(bc.Args)}
		tc.finalize()
		d.tools = append(d.tools, tc)
	}
	d.tools = dedupToolCalls(d.tools)

	d.closeThinkingBlock(&out)
	if d.textOpen {
		out = append(out, ir.Event{Type: ir.EvBlockStop, Index: d.textIndex})
		d.textOpen = false
	}
	for _, tc := range d.tools {
		idx := d.nextIndex
		d.nextIndex++
		out = append(out, ir.Event{
			Type:  ir.EvBlockStart,
			Index: idx,
			Block: &ir.Block{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: tc.ID, Name: OriginalForToolName(tc.Name)}},
		})
		out = append(out, ir.Event{Type: ir.EvToolInput, Index: idx, Text: tc.args})
		out = append(out, ir.Event{Type: ir.EvBlockStop, Index: idx})
	}

	stop := ir.StopEndTurn
	switch {
	case d.ContentTruncated():
		stop = ir.StopMaxTokens // 无完成信号且有正文：上游截断
	case len(d.tools) > 0:
		stop = ir.StopToolUse
	}

	u := ir.Usage{
		CacheReadTokens:     int(d.cacheRead),
		CacheCreationTokens: int(d.cacheCreation),
		OutputTokens:        (d.fullContent.Len() + d.fullThinking.Len()) / 4,
		Estimated:           true,
	}
	if d.contextPct != nil && *d.contextPct > 0 {
		maxIn := d.maxInput
		if maxIn <= 0 {
			maxIn = DefaultMaxInputTokens
		}
		total := int(*d.contextPct / 100 * float64(maxIn))
		in := total - u.OutputTokens
		if in < 0 {
			in = 0
		}
		u.InputTokens = in
	}

	out = append(out, ir.Event{Type: ir.EvMessageDelta, StopReason: stop, Usage: &u})
	out = append(out, ir.Event{Type: ir.EvMessageStop})
	return out
}
