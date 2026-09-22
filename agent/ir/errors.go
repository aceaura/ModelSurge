package ir

// Error 统一错误模型。codec 在出口处按客户端协议渲染，
// 在入口处从上游任意错误外形解析（万能解析参考 new-api GeneralErrorResponse）。
type Error struct {
	StatusCode int    // HTTP 状态码；流内错误时为推断值
	Type       string // 规范错误类型，如 "rate_limit_error"、"invalid_request_error"
	Code       string // 协议相关错误码，原样保留
	Reason     string // 上游错误原因码（如 Kiro "INVALID_MODEL_ID"），调度决策用
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
//
// 402 与 413 单列，不落到 default 的 upstream_error：
//   - 402 是账号余额/配额耗尽，不是「上游坏了」。Upstream 侧 kiro 分类表
//     （upstream/account/errors.go ClassifyKiroError）早已把 402 与 403/429 同列
//     为 RECOVERABLE=换号可救；两个参考仓也一致（sub2api 把 401/402/403/429/5xx
//     同归 UpstreamFailoverError，new-api 的默认重试区间 401-407 含 402）。判成
//     upstream_error 会让客户端读到「服务端故障」，而按 5xx 语义自行重试同一个
//     欠费账号，永远得到同一个 402。
//   - 413 是请求体过大，换谁都会被同样拒绝。relay 自己造 413 的两处
//     （contexterr.go 的上下文超限、replayDispatchError 的 CodeContextTooLarge）
//     都写 invalid_request_error；上游真发 413 时却判成 upstream_error，同一个
//     状态码因来源不同拿到两个规范类型。
func ClassifyStatus(status int) (typ string, retryable bool) {
	switch {
	case status == 400:
		return ErrTypeInvalidReq, false
	case status == 401:
		return ErrTypeAuth, true
	case status == 402:
		return ErrTypeRateLimit, true
	case status == 403:
		return ErrTypePermission, true
	case status == 404:
		return ErrTypeNotFound, false
	case status == 413:
		return ErrTypeInvalidReq, false
	case status == 429:
		return ErrTypeRateLimit, true
	case status >= 500:
		return ErrTypeOverloaded, true
	default:
		return ErrTypeUpstream, false
	}
}

// HTTPStatus 取错误的对客户端状态码。StatusCode 为零时（流内错误一律如此，
// 见 relay 里以 EvError 形式构造的那几处）按 Type 反推，而不是一律 500：
// 内容过滤与非法请求不可重试，退化成 500 会让客户端与 SDK 把它当成瞬时故障
// 反复重试，每次都被同样拒绝。
func (e *Error) HTTPStatus() int {
	if e == nil {
		return 500
	}
	if e.StatusCode != 0 {
		return e.StatusCode
	}
	switch e.Type {
	case ErrTypeInvalidReq, ErrTypeContentFilter:
		return 400
	case ErrTypeAuth:
		return 401
	case ErrTypePermission:
		return 403
	case ErrTypeNotFound:
		return 404
	case ErrTypeRateLimit:
		return 429
	case ErrTypeOverloaded:
		return 503
	default:
		return 500
	}
}

// StreamRetryable 按规范错误类型判**流内**错误能否换上游重试。流内错误没有 HTTP
// 状态码，只能按类型判；口径与 ClassifyStatus 保持一致，否则同一个错误在流式与
// 非流式两条路径上会得出相反的重试结论。
//
// 认证/权限失败要可重试：换一个账号可能就成了。非法请求/未找到/内容过滤不可重试：
// 换谁都会被同样拒绝，判成可重试只会让调度器把账号池白烧一遍。
// 未知类型默认可重试，与各族解码器此前的行为一致。
func StreamRetryable(typ string) bool {
	switch typ {
	case ErrTypeInvalidReq, ErrTypeNotFound, ErrTypeContentFilter:
		return false
	default:
		return true
	}
}

// NewHTTPError 由状态码与消息构造统一错误。
func NewHTTPError(status int, message string) *Error {
	typ, retry := ClassifyStatus(status)
	return &Error{StatusCode: status, Type: typ, Message: message, Retryable: retry}
}
