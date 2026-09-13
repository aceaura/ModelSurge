// effort.go reasoning effort -> Kiro 原生 additionalModelRequestFields
// （KiroaaS effort_schema.py 的 Go 翻译）。
package kiro

import (
	"log"
	"strings"
	"time"
)

// NativeEffortField Kiro 原生字段名。
const NativeEffortField = "additionalModelRequestFields"

// effortTier 模型可接受的 effort 枚举与落点字段。
type effortTier struct {
	path    string   // additionalModelRequestFields 下的子对象名
	allowed []string // 可接受枚举（有序）
}

// modelEffortSchema 各模型的 effort 通道（缺省无通道——省略不伪造）。
var modelEffortSchema = map[string]effortTier{
	"claude-opus-5":     {"output_config", []string{"low", "medium", "high", "xhigh", "max"}},
	"claude-sonnet-5":   {"output_config", []string{"low", "medium", "high", "xhigh", "max"}},
	"claude-opus-4.8":   {"output_config", []string{"low", "medium", "high", "xhigh", "max"}},
	"claude-sonnet-4.6": {"output_config", []string{"low", "medium", "high", "max"}},
	"gpt-5.5":           {"reasoning", []string{"none", "low", "medium", "high", "xhigh", "max"}},
	"gpt-5.6-sol":       {"reasoning", []string{"none", "low", "medium", "high", "xhigh", "max"}},
	"gpt-5.6-terra":     {"reasoning", []string{"none", "low", "medium", "high", "xhigh", "max"}},
	"gpt-5.6-luna":      {"reasoning", []string{"none", "low", "medium", "high", "xhigh", "max"}},
}

// effortOrder 全序（低 -> 高）。
var effortOrder = []string{"low", "medium", "high", "xhigh", "max"}

// effortFallback 所有带通道模型都接受的安全档位。
const effortFallback = "medium"

// clampEffort 把请求档位夹进模型枚举。
// "none" 且模型无 none 档 -> 空（省略）；未知值 -> fallback；
// 否则取 <= 请求档位的最高档（无则取最低档）。
func clampEffort(requested string, allowed []string) string {
	for _, a := range allowed {
		if a == requested {
			return requested
		}
	}
	if requested == "none" {
		for _, a := range allowed {
			if a == "none" {
				return "none"
			}
		}
		return ""
	}
	rank := effortRank(requested)
	if rank < 0 {
		return effortFallback // 未知请求值
	}
	best, bestRank := "", -1
	for _, a := range allowed {
		if r := effortRank(a); r >= 0 && r <= rank && r > bestRank {
			best, bestRank = a, r
		}
	}
	if best != "" {
		return best
	}
	// 无更低档：取最低可用档
	lowest, lowestRank := "", 1<<30
	for _, a := range allowed {
		if r := effortRank(a); r >= 0 && r < lowestRank {
			lowest, lowestRank = a, r
		}
	}
	if lowest != "" {
		return lowest
	}
	return effortFallback
}

func effortRank(tier string) int {
	for i, t := range effortOrder {
		if t == tier {
			return i
		}
	}
	return -1
}

// resolveEffortFragment 解析 effort 决策：
// 返回 additionalModelRequestFields 的子对象（如 {"output_config":{"effort":"high"}}），
// nil 表示省略（无请求/模型无通道/none 不可表达）。
func resolveEffortFragment(modelID, effort string) map[string]any {
	if effort == "" {
		return nil
	}
	tier, ok := modelEffortSchema[NormalizeModelName(modelID)]
	if !ok {
		return nil
	}
	adopted := clampEffort(effort, tier.allowed)
	if adopted == "" {
		return nil
	}
	if adopted != effort {
		log.Printf("kiro codec: clamped effort for %s: %q -> %q", modelID, effort, adopted)
	}
	return map[string]any{tier.path: map[string]any{"effort": adopted}}
}

// EffortFirstTokenTimeoutMultipliers effort 档位 -> 首 token 等待乘数
// （config.py EFFORT_FIRST_TOKEN_TIMEOUT_MULTIPLIERS：高档位推理会显著
// 推迟首字节，按档位缩放以免昂贵推理请求被提前判卡重试）。
var EffortFirstTokenTimeoutMultipliers = map[string]float64{
	"low": 1.5, "medium": 2.0, "high": 4.0, "xhigh": 6.0, "max": 8.0,
}

// EffortFirstTokenTimeoutCap 档位放大后的上限（0 = 不封顶）。
const EffortFirstTokenTimeoutCap = 120 * time.Second

// EffortFirstTokenTimeout 按请求 effort 档位放大首 token 超时
// （effort_schema.py resolve_first_token_timeout 的 Go 翻译）。
// 空档位/"none" 用基数；显式档位先经模型枚举夹紧（无通道模型按请求
// 档位本身，未知值按 medium）；乘数封顶 EffortFirstTokenTimeoutCap。
func EffortFirstTokenTimeout(base time.Duration, modelID, effort string) time.Duration {
	if effort == "" {
		return base
	}
	requested := strings.ToLower(strings.TrimSpace(effort))
	if requested == "" || requested == "none" {
		return base
	}
	effective := requested
	if tier, ok := modelEffortSchema[NormalizeModelName(modelID)]; ok {
		effective = clampEffort(requested, tier.allowed)
	} else if effortRank(requested) < 0 {
		effective = effortFallback
	}
	m := EffortFirstTokenTimeoutMultipliers[effective]
	if m == 0 {
		m = EffortFirstTokenTimeoutMultipliers[effortFallback]
	}
	t := time.Duration(float64(base) * m)
	if EffortFirstTokenTimeoutCap > 0 && t > EffortFirstTokenTimeoutCap {
		t = EffortFirstTokenTimeoutCap
	}
	return t
}
