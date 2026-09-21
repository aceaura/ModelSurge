package ir

// EstimateTokens 粗略估算文本 token 数：ASCII 约 4 字符 1 token，
// 非 ASCII（CJK 等）约 1 字 1 token。仅用于上游不提供 usage 且用户
// 显式开启估算时的兜底，结果不代表任何上游的真实计费口径。
func EstimateTokens(s string) int {
	ascii, nonASCII := 0, 0
	for _, r := range s {
		if r < 128 {
			ascii++
		} else {
			nonASCII++
		}
	}
	return ascii/4 + nonASCII
}

// EstimateRequestTokens 估算请求的输入 token（system + 消息文本 + 工具 schema）。
// 除文本外计入每条消息约 3 token 的结构开销与结尾 priming，避免短对话估成 0。
func EstimateRequestTokens(r *Request) int {
	total := 0
	for _, b := range r.System {
		total += EstimateTokens(b.Text) + 3
	}
	for _, m := range r.Messages {
		total += 3 // 每消息结构开销
		for _, b := range m.Content {
			switch b.Type {
			// 拒绝正文同样占上下文：漏计会让含拒绝历史的请求低估窗口占用，
			// 调度层据此放行后上游直接超限。
			case BlockText, BlockRefusal:
				total += EstimateTokens(b.Text)
			case BlockThinking:
				if b.Thinking != nil {
					total += EstimateTokens(b.Thinking.Text)
				}
			case BlockToolUse:
				if b.ToolUse != nil {
					total += EstimateTokens(b.ToolUse.Name) + EstimateTokens(string(b.ToolUse.Input))
				}
			case BlockToolResult:
				if b.ToolResult != nil {
					for _, cb := range b.ToolResult.Content {
						total += EstimateTokens(cb.Text)
					}
				}
			case BlockImage:
				total += 1100 // 图片按固定粗估（对齐 Anthropic 小图量级）
			}
		}
	}
	for _, t := range r.Tools {
		total += EstimateTokens(t.Name + t.Description + string(t.InputSchema))
	}
	return total + 3 // assistant 回复 priming
}
