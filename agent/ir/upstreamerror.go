package ir

import (
	"encoding/json"
	"strconv"
	"strings"
)

// 上游非 2xx 响应体的通用外形。字段集对齐 new-api 的
// relaykit/dto.GeneralErrorResponse：各家上游（OpenAI/Anthropic/Gemini/Azure/
// FastAPI 系代理）的错误信封差异极大，逐个协议写解析器既漏又难维护，统一按
// 「先找 error 对象，再退到各家顶层消息键」的顺序兜。
type upstreamErrorEnvelope struct {
	Error    json.RawMessage `json:"error"`
	Message  string          `json:"message"`
	Msg      string          `json:"msg"`
	Err      string          `json:"err"`
	ErrorMsg string          `json:"error_msg"`
	Detail   string          `json:"detail"`
	Header   struct {
		Message string `json:"message"`
	} `json:"header"`
	Response struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	} `json:"response"`
}

// upstreamErrorObject error 对象内部。Code 用 RawMessage 收：OpenAI 系是字符串
// （"context_length_exceeded"）、Gemini/Azure 是数字或数字串（429），后者只是把
// HTTP 状态码又说了一遍，不是错误码，必须区别对待。
type upstreamErrorObject struct {
	Message string          `json:"message"`
	Type    string          `json:"type"`
	Code    json.RawMessage `json:"code"`
	Status  string          `json:"status"`
}

// upstreamErrMessageLimit 回落成原文时的截断长度，与 relay 的 excerpt 同量级。
const upstreamErrMessageLimit = 500

// ParseUpstreamError 从上游的非 2xx 响应体解析统一错误。
//
// 此前 relay 只做 excerpt(原文) 塞进 Message：客户端拿到的 error.message 是一整段
// 转义后的 JSON（`"{\"error\":{\"message\":\"Overloaded\",...}}"`），而上游自报的
// 错误码（OpenAI 的 context_length_exceeded、Gemini 的 RESOURCE_EXHAUSTED）全糊在
// 文本里，Code 恒空。同一个上游错误走 kiro 目标时反而结构完整——kiro_remote.go 解
// ModelSurge 信封后填了 Code/Reason/干净的 Message，两条路径口径相反。
//
// Type 与 Retryable 一律按状态码推（ClassifyStatus），不采信上游自报的 type：
//   - kiro 路径早就是这么做的（`typ, _ := ir.ClassifyStatus(status)`），两条路径必须
//     同一条规则，否则同一个上游 429 因错误体外形不同拿到两个规范类型；
//   - 上游的 type 是各家私有词表（OpenAI 写 "requests"、"server_error"），照抄进
//     规范类型会把客户端与 SDK 的重试判断带偏。
//
// 解析不出消息时回落成截断后的原文：HTML 错误页、代理插的空壳 JSON 仍要可见，
// 不能因为「不是认识的形状」就把上游的失败说成一句 "upstream error"。
func ParseUpstreamError(status int, body []byte) *Error {
	typ, retry := ClassifyStatus(status)
	e := &Error{StatusCode: status, Type: typ, Retryable: retry}
	msg, code := parseUpstreamErrorBody(body)
	if msg == "" {
		msg = truncateUpstreamMessage(string(body))
	}
	if msg == "" {
		msg = "upstream error"
	}
	e.Message = msg
	e.Code = code
	return e
}

// parseUpstreamErrorBody 返回消息与上游错误码；两者都可能为空。
// 非 JSON、JSON 数组、缺字段的 body 都由 json.Unmarshal 直接挡掉，回落原文，
// 不需要额外的形状预判（预判与 Unmarshal 的结论完全重合，是不可测的冗余分支）。
func parseUpstreamErrorBody(body []byte) (msg, code string) {
	var env upstreamErrorEnvelope
	if json.Unmarshal(body, &env) != nil {
		return "", ""
	}
	if len(env.Error) > 0 {
		if obj := strings.TrimSpace(string(env.Error)); obj != "" && obj[0] == '{' {
			var o upstreamErrorObject
			if json.Unmarshal(env.Error, &o) == nil {
				code = upstreamErrorCode(o)
				if o.Message != "" {
					return o.Message, code
				}
			}
		} else {
			// {"error":"upstream boom"} 这种把消息直接写成字符串的形态。
			var s string
			if json.Unmarshal(env.Error, &s) == nil && s != "" {
				return s, ""
			}
		}
	}
	for _, cand := range []string{env.Message, env.Msg, env.Err, env.ErrorMsg, env.Detail,
		env.Header.Message, env.Response.Error.Message} {
		if cand != "" {
			return cand, code
		}
	}
	return "", code
}

// upstreamErrorCode 取上游自报的错误码。数字形态（Gemini 的 error.code、Azure 的
// code:"429"）只是 HTTP 状态码的回声，收下会让客户端把 429 当成业务错误码；这类
// 形状真正有信息量的是 Gemini 的 error.status（RESOURCE_EXHAUSTED）。
func upstreamErrorCode(o upstreamErrorObject) string {
	if raw := strings.TrimSpace(string(o.Code)); raw != "" && raw[0] == '"' {
		if s, err := strconv.Unquote(raw); err == nil && s != "" && !isStatusEcho(s) {
			return s
		}
	}
	if o.Status != "" && !isStatusEcho(o.Status) {
		return o.Status
	}
	return ""
}

// isStatusEcho 形状为纯数字的「错误码」不是错误码。
func isStatusEcho(s string) bool {
	_, err := strconv.Atoi(s)
	return err == nil
}

func truncateUpstreamMessage(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > upstreamErrMessageLimit {
		s = s[:upstreamErrMessageLimit] + "..."
	}
	return s
}
