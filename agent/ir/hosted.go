package ir

import "strings"

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
