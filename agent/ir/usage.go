package ir

import "encoding/json"

// Usage token 用量，采用 Anthropic 口径作为规范：
// InputTokens 不含 cache 部分，cache 单独计列。
// 各 codec 在边界处负责口径换算（OpenAI 的 input_tokens 含 cached_tokens，
// 需减去；Gemini 的 promptTokenCount 含 cachedContentTokenCount，同理）。
type Usage struct {
	InputTokens               int
	OutputTokens              int
	CacheReadTokens           int
	CacheCreationTokens       int
	CacheCreation5mTokens     int
	CacheCreation1hTokens     int
	CacheCreationDetailsKnown bool
	// ReasoningTokens 思考消耗，是 OutputTokens 的子集而非另一项，
	// 因此不参与任何合计——加进去会把输出算两遍。
	// 上游字段：Responses 的 output_tokens_details.reasoning_tokens、
	// Gemini 的 thoughtsTokenCount、Chat 的 completion_tokens_details、
	// Anthropic 的 output_tokens_details.thinking_tokens。
	ReasoningTokens int
	// WebSearchRequests / WebFetchRequests 服务端托管工具执行次数
	// （Anthropic usage.server_tool_use）。是次数不是 token，不进任何合计。
	WebSearchRequests int
	WebFetchRequests  int
	// PromptAudioTokens / CompletionAudioTokens Chat 音频 token 明细
	// （prompt_tokens_details / completion_tokens_details 的 audio_tokens），
	// 各自是所在总量的子集，不参与合计。
	PromptAudioTokens     int
	CompletionAudioTokens int
	// AcceptedPredictionTokens / RejectedPredictionTokens Chat 预测加速
	// （completion_tokens_details 的 accepted/rejected_prediction_tokens）。
	AcceptedPredictionTokens int
	RejectedPredictionTokens int
	// InferenceGeo Anthropic 响应侧回显的实际推理区域（usage.inference_geo）。
	// 请求侧的同名偏好字段在 Request 上；这里只是回执，不参与调度。
	InferenceGeo string
	// Speed Anthropic beta 响应侧回显的实际推理速度档（usage.speed，
	// standard/fast）。fast 是溢价计费档，客户端拿它核对上游实际按哪档
	// 执行；请求侧的声明在 Request.Speed。仅 anthropic 有槽位。
	Speed     string
	// Iterations Anthropic beta usage.iterations：按迭代阶段（message/
	// compaction/advisor）细分的用量。判别式值域仍在演进，不建模成具体结构，
	// 原文透传——同族往返逐字带回，跨族无槽位（由 UsageDropDims 报出）。
	Iterations json.RawMessage `json:",omitempty"`
	Estimated  bool            // 本地估算产生（上游未提供）时为 true
}

// TotalInput 总输入口径（含 cache），对应 OpenAI prompt_tokens 语义。
func (u Usage) TotalInput() int {
	return u.InputTokens + u.CacheReadTokens + u.CacheCreationTokens
}

// MergeNonZero 合并用量：后到的非零字段覆盖，零值不擦除已有值。
// Estimated 取两者或运算。
func (u *Usage) MergeNonZero(o Usage) {
	if o.InputTokens != 0 {
		u.InputTokens = o.InputTokens
	}
	if o.OutputTokens != 0 {
		u.OutputTokens = o.OutputTokens
	}
	if o.CacheReadTokens != 0 {
		u.CacheReadTokens = o.CacheReadTokens
	}
	if o.CacheCreationTokens != 0 {
		u.CacheCreationTokens = o.CacheCreationTokens
	}
	if o.CacheCreationDetailsKnown {
		u.CacheCreation5mTokens = o.CacheCreation5mTokens
		u.CacheCreation1hTokens = o.CacheCreation1hTokens
		u.CacheCreationDetailsKnown = true
	}
	if o.ReasoningTokens != 0 {
		u.ReasoningTokens = o.ReasoningTokens
	}
	if o.WebSearchRequests != 0 {
		u.WebSearchRequests = o.WebSearchRequests
	}
	if o.WebFetchRequests != 0 {
		u.WebFetchRequests = o.WebFetchRequests
	}
	if o.PromptAudioTokens != 0 {
		u.PromptAudioTokens = o.PromptAudioTokens
	}
	if o.CompletionAudioTokens != 0 {
		u.CompletionAudioTokens = o.CompletionAudioTokens
	}
	if o.AcceptedPredictionTokens != 0 {
		u.AcceptedPredictionTokens = o.AcceptedPredictionTokens
	}
	if o.RejectedPredictionTokens != 0 {
		u.RejectedPredictionTokens = o.RejectedPredictionTokens
	}
	if o.InferenceGeo != "" {
		u.InferenceGeo = o.InferenceGeo
	}
	if o.Speed != "" {
		u.Speed = o.Speed
	}
	if len(o.Iterations) > 0 {
		u.Iterations = o.Iterations
	}
	u.Estimated = u.Estimated || o.Estimated
}
