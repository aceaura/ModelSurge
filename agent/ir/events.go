package ir

import "encoding/json"

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
	Type         EventType
	Index        int
	Block        *Block     // EvBlockStart
	Text         string     // 各 delta
	StopReason   StopReason // EvMessageDelta
	StopSequence string     // EvMessageDelta，StopSequence 档命中的那条序列原文
	// StopDetails EvMessageDelta，拒绝档的结构化分类（仅 anthropic 有槽位）。
	StopDetails *StopDetails
	Usage       *Usage // EvMessageStart / EvMessageDelta
	MessageID   string // EvMessageStart
	Model       string // EvMessageStart
	// Created 上游给的创建时间（Unix 秒，EvMessageStart 携带）。零值表示上游
	// 没给，编码器回退本地钟（与非流式 Response.Created 同一口径）。
	Created int64
	// ServiceTier 上游回显的实际服务档位（EvMessageStart 携带；chat chunk
	// 可能到得比首帧晚，EvMessageDelta 上也收）。保留原值不规整：跨族映射
	// 在出站编码按目标协议回显值集进行（proto.MapServiceTierEcho）。
	ServiceTier string
	// SystemFingerprint Chat 后端配置指纹（EvMessageStart 携带）。仅 chat 族
	// 有槽位，跨族出站不投影。
	SystemFingerprint string
	// Metadata OpenAI 两系响应的 metadata 回显原文（responses 流式
	// response.created 的 response 对象携带，EvMessageStart 上收）。仅
	// OpenAI 两系有槽位，跨族出站不投影。
	Metadata json.RawMessage `json:",omitempty"`
	// Container 代码执行容器回显（EvMessageStart 首帧携带；anthropic 的
	// message_delta 也可能晚到，EvMessageDelta 上也收，后值覆盖）。
	Container *Container
	// ContextMgmt anthropic message_delta 事件顶层的服务端上下文清理回执原文
	// （官方 BetaRawMessageDeltaEvent.context_management，与 delta 平级）。
	// 仅 anthropic 有槽位，跨族出站不投影；聚合落 Response.AnthropicContextMgmt。
	ContextMgmt json.RawMessage `json:",omitempty"`
	// Audio 非流式完整响应转事件流时随 EvMessageStart 携带。所有当前流式
	// 客户端协议均无官方完整音频槽位，只用于编码器记账并报告丢失。
	Audio            *AudioOutput
	Err              *Error     // EvError
	Citations        []Citation // EvCitation
	TruncatedTools   []TruncatedTool
	TruncatedContent string
	// SignatureFrom EvSigDelta 的签名来源形态，语义同 Thinking.SignatureFrom。
	// 签名在流式路径上逐片到达，来源只有解码器知道；不随事件带上，聚合出的
	// Thinking 就只有签名没有来源，下一轮会被当成外族签名丢掉。
	SignatureFrom string
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
	// StopContextWindow Anthropic 的 model_context_window_exceeded：输入上下文
	// 把窗口占满、输出被挤断。与 max_tokens（输出配额耗尽）分开是因为客户端的
	// 补救动作相反——这里要压缩/截短输入，抬 max_tokens 没有用。塌成 end_turn
	// 会把截断回答伪装成自然说完。
	StopContextWindow StopReason = "context_window_exceeded"
	// StopMaxMessages 消息数上限截断（Responses incomplete_details.reason
	// 的 "max_messages"）。与 max_tokens 的输出长度上限是两回事：客户端照
	// max_tokens 提示加大输出预算重试仍会被同一上限拦住。只有 responses
	// 一族有此档，外族出站归 length（输出确实不完整）。
	StopMaxMessages StopReason = "max_messages"
	// StopSteered 用户中途转向（steer）导致的安全边界截断（Responses
	// incomplete_details.reason 的 "steered"）。与 max_tokens 分开：那是输出
	// 配额耗尽、要加大预算重试；steered 是用户在安全边界处打断了生成、通常已有
	// 自动后继，加大预算毫无意义。塌成 max_tokens 会让客户端误判补救动作。
	// 只有 responses 一族有此档，外族出站归「输出不完整」的最近档。
	StopSteered StopReason = "steered"
)

// Incomplete 报告该停止原因是否意味着输出不完整——客户端不应把正文当成
// 最终答案。供编码器与诊断统一判据，避免各处各写一份枚举清单。
func (s StopReason) Incomplete() bool {
	switch s {
	case StopMaxTokens, StopPauseTurn, StopAborted, StopContextWindow, StopSteered:
		return true
	}
	return false
}
