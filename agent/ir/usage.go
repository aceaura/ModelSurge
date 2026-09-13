package ir

// Usage token 用量，采用 Anthropic 口径作为规范：
// InputTokens 不含 cache 部分，cache 单独计列。
// 各 codec 在边界处负责口径换算（OpenAI 的 input_tokens 含 cached_tokens，
// 需减去；Gemini 的 promptTokenCount 含 cachedContentTokenCount，同理）。
type Usage struct {
	InputTokens         int
	OutputTokens        int
	CacheReadTokens     int
	CacheCreationTokens int
	Estimated           bool // 本地估算产生（上游未提供）时为 true
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
	u.Estimated = u.Estimated || o.Estimated
}
