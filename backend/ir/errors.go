package ir

// Error 统一错误模型。codec 在出口处按客户端协议渲染，
// 在入口处从上游任意错误外形解析（万能解析参考 new-api GeneralErrorResponse）。
type Error struct {
	StatusCode int    // HTTP 状态码；流内错误时为推断值
	Type       string // 规范错误类型，如 "rate_limit_error"、"invalid_request_error"
	Code       string // 协议相关错误码，原样保留
	Message    string // 脱敏后的用户可读信息
	Retryable  bool   // 是否可换上游重试
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

// 规范错误类型。
const (
	ErrTypeRateLimit     = "rate_limit_error"
	ErrTypeInvalidReq    = "invalid_request_error"
	ErrTypeAuth          = "authentication_error"
	ErrTypePermission    = "permission_error"
	ErrTypeNotFound      = "not_found_error"
	ErrTypeOverloaded    = "overloaded_error"
	ErrTypeUpstream      = "upstream_error"
	ErrTypeContentFilter = "content_filter_error"
)

// ClassifyStatus 按 HTTP 状态码推断规范错误类型与可重试性。
func ClassifyStatus(status int) (typ string, retryable bool) {
	switch {
	case status == 400:
		return ErrTypeInvalidReq, false
	case status == 401:
		return ErrTypeAuth, true
	case status == 403:
		return ErrTypePermission, true
	case status == 404:
		return ErrTypeNotFound, false
	case status == 429:
		return ErrTypeRateLimit, true
	case status >= 500:
		return ErrTypeOverloaded, true
	default:
		return ErrTypeUpstream, false
	}
}

// NewHTTPError 由状态码与消息构造统一错误。
func NewHTTPError(status int, message string) *Error {
	typ, retry := ClassifyStatus(status)
	return &Error{StatusCode: status, Type: typ, Message: message, Retryable: retry}
}
