package ir

import (
	"encoding/json"
	"strings"
)

// 托管工具（服务端执行）的规范种类。codec 在边界处做 协议私有类型名 <-> 规范种类 映射。
const (
	HostedWebSearch     = "web_search"     // Anthropic web_search_* / OpenAI web_search / Gemini google_search
	HostedCodeExecution = "code_execution" // Anthropic code_execution_* / OpenAI code_interpreter / Gemini code_execution
)

// CanonicalHosted 把协议私有的托管工具类型名归一为规范种类；
// 无法识别的原样返回（同协议往返可透传，跨协议时由能力检查丢弃）。
func CanonicalHosted(native string) string {
	switch {
	case strings.HasPrefix(native, "web_search"), native == "google_search":
		return HostedWebSearch
	case strings.HasPrefix(native, "code_execution"), native == "code_interpreter":
		return HostedCodeExecution
	default:
		return native
	}
}

// HostedTypeFamily 原生托管工具类型名属于哪个协议族。只有同族回写时原生名
// 才可信：把 gemini 的 google_search 或 responses 的 web_search_preview 写进
// anthropic 的 tools[].type 是上游必 400 的形状，外族原名必须回落各族默认名。
//
// Anthropic 的托管类型恒带 _YYYYMMDD 版本后缀；gemini 是 google_search 与
// 无版本的 code_execution（Responses 一族的对应物叫 code_interpreter）；
// Responses 一族全是无版本小写串（web_search / web_search_preview /
// code_interpreter / file_search 等），未知名按这一族处理。
func HostedTypeFamily(native string) string {
	if n := len(native); n > 9 && native[n-9] == '_' && isDigits(native[n-8:]) {
		return "anthropic"
	}
	if native == "google_search" || native == "googleSearch" || native == "code_execution" {
		return "gemini"
	}
	return "openai-responses"
}

func isDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return len(s) > 0
}

// HostedParams 服务端托管工具的声明参数（web_search 一族）。同族往返原样
// 回写；跨族只映射双方都有槽位的子集，装不下的由 relay.Diagnose 报出。
type HostedParams struct {
	// MaxUses 本回合最多调用次数（anthropic max_uses）。Responses 无槽位。
	MaxUses int
	// AllowedDomains 域名白名单（anthropic allowed_domains ⇔ responses
	// filters.allowed_domains，两族语义相同，跨族直通）。
	AllowedDomains []string
	// BlockedDomains 域名黑名单（anthropic blocked_domains，与白名单互斥）。
	// Responses 无槽位。
	BlockedDomains []string
	// UserLocation 粗粒度地理位置（两族同为 {"type":"approximate",...} 形状，
	// 原文透传，跨族直通）。
	UserLocation json.RawMessage `json:",omitempty"`
	// SearchContextSize 检索规模档位 low/medium/high（responses
	// search_context_size）。Anthropic 无槽位。
	SearchContextSize string
}

// UnsupportedOn 报告参数里哪些目标协议族没有槽位，返回参数名列表（用于
// 损耗注记）。chat 出站的托管工具整丢、gemini 无出站，两种情形由整丢注记
// 覆盖，调用方不要在这两个目标上走这里重复报。
func (p *HostedParams) UnsupportedOn(protoName string) []string {
	if p == nil {
		return nil
	}
	var out []string
	switch protoName {
	case "anthropic":
		if p.SearchContextSize != "" {
			out = append(out, "search_context_size")
		}
	default: // openai-responses / codex
		if p.MaxUses > 0 {
			out = append(out, "max_uses")
		}
		if len(p.BlockedDomains) > 0 {
			out = append(out, "blocked_domains")
		}
	}
	return out
}
