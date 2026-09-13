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
	EvBlockStop     EventType = "block_stop"       // 内容块结束
	EvMessageDelta  EventType = "message_delta"    // 携带 StopReason 与最终 Usage
	EvMessageStop   EventType = "message_stop"     // 流正常结束
	EvPing          EventType = "ping"             // 保活
	EvError         EventType = "error"            // 流内错误，Err 非空
)

// Event 一个流式事件。Index 为内容块序号（block 级事件有效）。
type Event struct {
	Type       EventType
	Index      int
	Block      *Block     // EvBlockStart
	Text       string     // 各 delta
	StopReason StopReason // EvMessageDelta
	Usage      *Usage     // EvMessageStart / EvMessageDelta
	MessageID  string     // EvMessageStart
	Model      string     // EvMessageStart
	Err        *Error     // EvError
}

// StopReason 规范停止原因；codec 在边界处映射协议方言
// （stop/end_turn、length/max_tokens、tool_calls/tool_use、content_filter/refusal）。
type StopReason string

const (
	StopEndTurn   StopReason = "end_turn"
	StopMaxTokens StopReason = "max_tokens"
	StopToolUse   StopReason = "tool_use"
	StopRefusal   StopReason = "refusal"
)
