package relay

import (
	"net/http"
	"strings"
)

// ReasonContextExceeded 上下文超限错误原因码。Agent 本地分类后经
// ResultReport.Outcome 直传 Upstream/Replay：不熔断、不重试、不污染缓存。
const ReasonContextExceeded = "context_exceeded"

// classifyContextError 判定上游错误是否为上下文超限。
// 仅认 400/413（超限类错误不会以 5xx 出现），消息小写化后按特征匹配；
// 特征集参考 sub2api isOpenAIContextWindowError 与主流上游真实报文。
func classifyContextError(status int, msg string) bool {
	if status != http.StatusBadRequest && status != http.StatusRequestEntityTooLarge {
		return false
	}
	m := strings.ToLower(msg)
	if m == "" {
		return false
	}
	hasExceeded := strings.Contains(m, "exceed") || strings.Contains(m, "too large") || strings.Contains(m, "too long")
	switch {
	case strings.Contains(m, "context_length_exceeded"),
		strings.Contains(m, "context_too_large"),
		strings.Contains(m, "maximum context length"),
		strings.Contains(m, "max context length"),
		strings.Contains(m, "prompt is too long"),
		strings.Contains(m, "conversation too long"):
		return true
	case strings.Contains(m, "context window") && hasExceeded:
		return true
	case strings.Contains(m, "context length") && strings.Contains(m, "exceed"):
		return true
	case strings.Contains(m, "token limit") && strings.Contains(m, "context") && strings.Contains(m, "exceed"):
		return true
	}
	return false
}
