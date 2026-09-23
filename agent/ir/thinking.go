package ir

import "fmt"

// thinking.go 推理风格的跨协议换算。
//
// 两种风格互不相通是本文件存在的理由：Anthropic 系用 budget_tokens（绝对
// token 预算），OpenAI 系用 reasoning_effort（枚举档位）。四个出站各自只读
// 一侧，于是「客户端只给了另一侧」时那一维被静默替换成硬编码缺省值——
// 客户端要 effort=high 拿到 4096（Anthropic 缺省），要 budget=32768 拿到
// medium。两种情形都是 HTTP 200 且无任何说明。
//
// 换算落在 IR 而非各 codec：补出来的值要让每个出站编码器和 relay 侧的
// IR 消费者（预算日志、ClampThinking 排序）看到同一份，落在 codec 里就得
// 四处重抄同一段换算。

// 档位全序（低 -> 高）。minimal 是 OpenAI Responses 的档位，
// xhigh/max 见于 Codex 扩展。
const (
	EffortNone    = "none"
	EffortMinimal = "minimal"
	EffortLow     = "low"
	EffortMedium  = "medium"
	EffortHigh    = "high"
	EffortXHigh   = "xhigh"
	EffortMax     = "max"
)

// effortPercent 档位 -> max_tokens 的百分比。
//
// 取百分比而非固定 token 数：预算的含义是「这次回答里多少份额用于思考」，
// 与客户端给的 max_tokens 成比例才保持这个含义。固定表（sub2api 的
// 1024/4096/10240/32768、cc-switch 的 2048/8192/16384/24576）在
// max_tokens=1024 时会算出大于上限的预算，在 max_tokens=200000 时又只用掉
// 一个零头。数值对齐 new-api relaykit/relayconvert/reasoning/claude.go:345-366
// 的 effortPercentage（minimal 5 / low 20 / medium 50 / high 80 / xhigh|max 95）。
var effortPercent = map[string]int{
	EffortMinimal: 5,
	EffortLow:     20,
	EffortMedium:  50,
	EffortHigh:    80,
	EffortXHigh:   95,
	EffortMax:     95,
}

// 预算换算的边界。
const (
	// MinThinkingBudget Anthropic 的协议下限：budget_tokens 必须 >= 1024，
	// 低于它上游回不可重试 400。按百分比算出的小值抬到这里。
	MinThinkingBudget = 1024

	// DefaultThinkingBudget 既无 effort 又无 max_tokens 时的兜底。
	// 与 proto/anthropic 原有的硬编码缺省同值，保持既有行为不变。
	DefaultThinkingBudget = 4096
)

// EffortFromBudget 预算 -> 档位。
//
// 阈值对齐 new-api reasoning/intent.go:585-599（0→none、<0→high、
// ≤1024→low、≤8192→medium、其余 high）。负值是 Gemini 的「动态思考」
// 语义（由上游自行决定思考多久），落到最高档而非当成没提——客户端表达的
// 是「尽量想」。
func EffortFromBudget(budget int) string {
	switch {
	case budget == 0:
		return EffortNone
	case budget < 0:
		return EffortHigh
	case budget <= 1024:
		return EffortLow
	case budget <= 8192:
		return EffortMedium
	default:
		return EffortHigh
	}
}

// BudgetFromEffort 档位 -> 预算，按 maxTokens 的百分比。
//
// maxTokens <= 0（客户端没给上限）时回落 DefaultThinkingBudget：此处刻意
// 不照 new-api 那样报错（reasoning/claude.go:230-235 缺 max_tokens 直接拒），
// 因为本服务的既定判据是「只报有损，从不拒请求」——目标协议由调度层选，
// 客户端无从预知这一跳需要 max_tokens。
//
// 未知档位返回 0，调用方据此不改动预算：照 cc-switch
// transform_codex_anthropic.rs:36-47 的理由，猜一个值可能误吞采样参数。
func BudgetFromEffort(effort string, maxTokens int) int {
	pct, ok := effortPercent[effort]
	if !ok {
		return 0
	}
	if maxTokens <= 0 {
		return DefaultThinkingBudget
	}
	budget := maxTokens * pct / 100
	if budget < MinThinkingBudget {
		budget = MinThinkingBudget
	}
	// Anthropic 约束 budget_tokens < max_tokens。95% 档在小 max_tokens 上、
	// 以及下限抬升之后都可能越界，这里统一夹紧；夹到 0 以下交由调用方判断。
	if budget >= maxTokens {
		budget = maxTokens - 1
	}
	return budget
}

// CompleteThinking 把 Thinking 的两种风格互相补全，使每个出站 codec 无论
// 读哪一侧都拿到客户端真实意图派生出的值，而不是硬编码缺省。
//
// 只补缺失的一侧，两侧都给了则一律不动——那是客户端同时表达了两种风格，
// 替它改写任一侧都是发明意图。
//
// 返回本次补全的说明（供 Diagnose 汇入 X-ModelSurge-Notes）；没补则为空。
// 措辞用 derived 而非 dropped/filled in：这一维客户端确实给过，只是给的是
// 另一种风格，读者的下一步动作与那两类都不同。
func CompleteThinking(r *Request) []string {
	if r == nil || r.Thinking == nil || !r.Thinking.Enabled {
		return nil
	}
	tc := r.Thinking
	var notes []string
	switch {
	case tc.Effort != "" && tc.BudgetTokens == 0:
		if b := BudgetFromEffort(tc.Effort, r.MaxTokens); b > 0 {
			tc.BudgetTokens = b
			notes = append(notes, fmt.Sprintf("derived thinking budget %d from reasoning effort %s", b, tc.Effort))
		}
	case tc.Effort == "" && tc.BudgetTokens != 0:
		if e := EffortFromBudget(tc.BudgetTokens); e != "" && e != EffortNone {
			tc.Effort = e
			notes = append(notes, fmt.Sprintf("derived reasoning effort %s from thinking budget %d", e, tc.BudgetTokens))
		}
	}
	return notes
}
