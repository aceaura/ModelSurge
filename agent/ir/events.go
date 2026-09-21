package ir

// EventType 流式事件类型。事件词汇以 Anthropic Messages streaming 为超集，
// 各协议 codec 负责与本协议事件模型互转。
type EventType string

const (
	EvMessageStart  EventType = "message_start"    // 流开始，携带 MessageID/Model，可含初始 Usage
	EvBlockStart    EventType = "block_start"      // 内容块开始，Block 携带类型（text/thinking/tool_use 含 id+name）
	EvTextDelta     EventType = "text_delta"       // 文本增量，Text 为增量内容
	EvThinkingDelta EventType = "thinking_delta"   // 推理增量
	EvSigDelta      EventType = "signature_delta"  // thinking 签名增量（BlockThinking 块的 Signature）
	EvToolInput     EventType = "input_json_delta" // tool_use 参数 JSON 增量，Text 为 JSON 片段
	EvCitation      EventType = "citation_delta"   // 引用标注增量，Citations 为本次新增的来源
	EvBlockStop     EventType = "block_stop"       // 内容块结束
	EvMessageDelta  EventType = "message_delta"    // 携带 StopReason 与最终 Usage
	EvMessageStop   EventType = "message_stop"     // 流正常结束
	EvPing          EventType = "ping"             // 保活
	EvError         EventType = "error"            // 流内错误，Err 非空
)

// Event 一个流式事件。Index 为内容块序号（block 级事件有效）。
type Event struct {
	Type             EventType
	Index            int
	Block            *Block     // EvBlockStart
	Text             string     // 各 delta
	StopReason       StopReason // EvMessageDelta
	StopSequence     string     // EvMessageDelta，StopSequence 档命中的那条序列原文
	Usage            *Usage     // EvMessageStart / EvMessageDelta
	MessageID        string     // EvMessageStart
	Model            string     // EvMessageStart
	Err              *Error     // EvError
	Citations        []Citation // EvCitation
	TruncatedTools   []TruncatedTool
	TruncatedContent string
}

type TruncatedTool struct {
	ID     string
	Name   string
	Reason string
}

// StopReason 规范停止原因；codec 在边界处映射协议方言
// （stop/end_turn、length/max_tokens、tool_calls/tool_use、content_filter/refusal）。
type StopReason string

const (
	StopEndTurn   StopReason = "end_turn"
	StopMaxTokens StopReason = "max_tokens"
	StopToolUse   StopReason = "tool_use"
	StopRefusal   StopReason = "refusal"
	// StopStopSequence 命中了客户端给的 stop_sequences。与 end_turn 分开是因为
	// 客户端的后续动作不同：命中停止串通常意味着它要按自己的分隔符切割输出，
	// 塌成 end_turn 会让它以为模型自然说完了。命中的序列原文在 Event/Response
	// 的 StopSequence 字段。
	StopStopSequence StopReason = "stop_sequence"
	// StopPauseTurn Anthropic 的长任务暂停：一轮没做完，客户端应把当前对话
	// 原样回传以继续。塌成 end_turn 会让客户端把半截结果当成最终答案。
	StopPauseTurn StopReason = "pause_turn"
	// StopAborted 流在上游给出任何完成信号之前就断了（连接被切、读出错、
	// 上游直接关流）。没有任何协议有原生的「中断」档，但也不能塌成 end_turn：
	// 客户端看到干净收尾就会把半截输出当成最终答案提交，而正确动作是重试。
	// 各协议按「输出不完整」的最近档表达（max_tokens / length / MAX_TOKENS /
	// incomplete），语义偏差由诊断说明。
	StopAborted StopReason = "aborted"
)

// Incomplete 报告该停止原因是否意味着输出不完整——客户端不应把正文当成
// 最终答案。供编码器与诊断统一判据，避免各处各写一份枚举清单。
func (s StopReason) Incomplete() bool {
	switch s {
	case StopMaxTokens, StopPauseTurn, StopAborted:
		return true
	}
	return false
}
