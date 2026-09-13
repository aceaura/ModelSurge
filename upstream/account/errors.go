// errors.go kiro 上游错误分类（KiroaaS account_errors.py 的 Go 翻译）。
// RECOVERABLE = 账号级问题（换号可救）；FATAL = 请求本身的问题
// （换号也会失败，直接透出客户端）。分类表：
//
//	402 / 403 / 429                    -> RECOVERABLE（配额/凭据/限流）
//	400 + INVALID_MODEL_ID             -> RECOVERABLE（订阅层级差异，换号）
//	400 + 其他（含 CONTENT_LENGTH_..） -> FATAL（上下文超限/畸形请求）
//	422 / 5xx / 未知                   -> FATAL
package account

import "strings"

// ErrorClass 错误处置分类。
type ErrorClass int

const (
	// ClassFatal 请求本身的问题：透出客户端，不换号。
	ClassFatal ErrorClass = iota
	// ClassRecoverable 账号级问题：换下一账号重试。
	ClassRecoverable
)

// 已知 kiro 错误 reason（kiro_errors.py KiroErrorReason 子集）。
const (
	ReasonInvalidModelID       = "INVALID_MODEL_ID"
	ReasonContentLengthExceeds = "CONTENT_LENGTH_EXCEEDS_THRESHOLD"
	ReasonMonthlyRequestCount  = "MONTHLY_REQUEST_COUNT"
)

// ClassifyKiroError kiro 上游错误分类。reason 取自错误体 JSON 的
// "reason" 字段（可能为空）。
func ClassifyKiroError(statusCode int, reason string) ErrorClass {
	switch statusCode {
	case 402, 403, 429:
		return ClassRecoverable
	case 400:
		if reason == ReasonInvalidModelID {
			return ClassRecoverable
		}
		return ClassFatal
	default:
		return ClassFatal
	}
}

// ParseKiroErrorReason 从 kiro 错误体提取 reason 与 message
// （错误体形如 {"message": "...", "reason": "..."}，字段可缺省）。
func ParseKiroErrorReason(body []byte) (reason, message string) {
	// 轻量提取，避免完整 JSON 建模：错误体上限 4KB，字段顺序不定。
	s := string(body)
	reason = extractJSONStringField(s, "reason")
	message = extractJSONStringField(s, "message")
	return reason, message
}

// extractJSONStringField 提取顶层字符串字段值（容错：非 JSON/字段缺失返回空）。
func extractJSONStringField(s, field string) string {
	// "field" : "value" 形态；引号内转义仅处理 \"（错误消息里最常见）。
	key := `"` + field + `"`
	idx := strings.Index(s, key)
	if idx < 0 {
		return ""
	}
	rest := s[idx+len(key):]
	// 跳过冒号与空白
	i := 0
	for i < len(rest) && (rest[i] == ' ' || rest[i] == ':' || rest[i] == '\t' || rest[i] == '\n' || rest[i] == '\r') {
		i++
	}
	if i >= len(rest) || rest[i] != '"' {
		return ""
	}
	i++
	var b strings.Builder
	for i < len(rest) {
		c := rest[i]
		if c == '\\' && i+1 < len(rest) {
			switch rest[i+1] {
			case '"':
				b.WriteByte('"')
			case '\\':
				b.WriteByte('\\')
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			default:
				b.WriteByte(rest[i+1])
			}
			i += 2
			continue
		}
		if c == '"' {
			break
		}
		b.WriteByte(c)
		i++
	}
	return b.String()
}
