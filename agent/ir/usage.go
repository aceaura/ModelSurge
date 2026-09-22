package ir

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
	// Gemini 的 thoughtsTokenCount、Chat 的 completion_tokens_details。
	ReasoningTokens int
	Estimated       bool // 本地估算产生（上游未提供）时为 true
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
	u.Estimated = u.Estimated || o.Estimated
}
