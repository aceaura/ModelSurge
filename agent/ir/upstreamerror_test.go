package ir

import (
	"strings"
	"testing"
)

// 上游错误体一律要解析出干净的消息与错误码。此前 relay 只做 excerpt(原文)，客户端
// 拿到的 error.message 是一整段转义 JSON，上游自报的 code 只糊在文本里、Code 恒空；
// 而同一个错误走 ModelSurge 信封时却结构完整——两条路径口径相反。
func TestParseUpstreamErrorExtractsMessageAndCode(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		wantMsg  string
		wantCode string
	}{
		{"openai", `{"error":{"message":"Rate limit reached","type":"requests","param":null,"code":"rate_limit_exceeded"}}`,
			"Rate limit reached", "rate_limit_exceeded"},
		{"anthropic", `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`,
			"Overloaded", ""},
		{"gemini", `{"error":{"code":429,"message":"You exceeded your current quota","status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.DebugInfo","detail":"throttled"}]}}`,
			"You exceeded your current quota", "RESOURCE_EXHAUSTED"},
		{"azure", `{"error":{"code":"429","message":"Requests to the deployment have exceeded the limit","target":"deployment","innererror":{"code":"429"}}}`,
			"Requests to the deployment have exceeded the limit", ""},
		{"fastapi detail", `{"detail":"Invalid API key"}`, "Invalid API key", ""},
		{"error as string", `{"error":"upstream boom"}`, "upstream boom", ""},
		{"top-level message", `{"message":"quota gone","code":"insufficient_quota"}`, "quota gone", ""},
		{"msg key", `{"msg":"quota gone"}`, "quota gone", ""},
		{"error_msg key", `{"error_msg":"quota gone"}`, "quota gone", ""},
		{"err key", `{"err":"quota gone"}`, "quota gone", ""},
		{"header message", `{"header":{"message":"quota gone"}}`, "quota gone", ""},
		{"nested response error", `{"response":{"error":{"message":"quota gone"}}}`, "quota gone", ""},
		// error 对象里只有 code 没有 message 时，消息退到顶层键，code 仍要留住
		{"code without message", `{"error":{"code":"context_length_exceeded"},"message":"too many tokens"}`,
			"too many tokens", "context_length_exceeded"},
		// status 写成数字的信封：字段类型不匹配不得连累解得好的 message 与 code。
		// 修复前整个 error 对象被丢弃，Message 回落成转义原文、Code 恒空。
		{"numeric status envelope", `{"error":{"code":"throttled","message":"slow down","retryable":true,"status":429}}`,
			"slow down", "throttled"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := ParseUpstreamError(429, []byte(c.body))
			if e.Message != c.wantMsg {
				t.Errorf("Message = %q，want %q", e.Message, c.wantMsg)
			}
			if e.Code != c.wantCode {
				t.Errorf("Code = %q，want %q", e.Code, c.wantCode)
			}
			// 解析出的消息里不得残留 JSON 结构字符：那正是修复前整段原文入 Message
			// 的症状，客户端 SDK 会把它当人话显示给用户。
			if strings.Contains(e.Message, `{"`) || strings.Contains(e.Message, `\"`) {
				t.Errorf("Message 里残留 JSON 结构：%q", e.Message)
			}
		})
	}
}

// 规范类型与可重试性一律按状态码推，不采信上游自报的 type。上游的 type 是各家私有
// 词表（OpenAI 写 "requests"/"server_error"，Anthropic 写 "overloaded_error"），照抄
// 会让同一个 429 因错误体外形不同拿到两个规范类型，客户端与 SDK 的退避判断随之分叉。
// ModelSurge 信封路径早就是这条规则（只取 ClassifyStatus 的 type、丢掉信封自报的），
// 直连路径必须一致。
func TestParseUpstreamErrorTypeFollowsStatusNotSelfReport(t *testing.T) {
	for _, tc := range []struct {
		status    int
		selfType  string
		wantType  string
		wantRetry bool
	}{
		{500, "invalid_request_error", ErrTypeOverloaded, true},
		{400, "overloaded_error", ErrTypeInvalidReq, false},
		{429, "server_error", ErrTypeRateLimit, true},
		{402, "billing_error", ErrTypeRateLimit, true},
		{413, "internal_error", ErrTypeInvalidReq, false},
	} {
		body := `{"error":{"message":"m","type":"` + tc.selfType + `"}}`
		e := ParseUpstreamError(tc.status, []byte(body))
		if e.Type != tc.wantType || e.Retryable != tc.wantRetry {
			t.Errorf("status=%d 上游自报 type=%q：得到 (%q,%v)，want (%q,%v)",
				tc.status, tc.selfType, e.Type, e.Retryable, tc.wantType, tc.wantRetry)
		}
	}
}

// 不变量：解析路径与 NewHTTPError 的分类结论必须逐码相同。两者是同一个上游错误在
// 不同调用点（forward.go 的非 2xx 分支、relay 自造的本地错误）的两条入口，口径一旦
// 分叉，同一个状态码就会拿到两个规范类型。
func TestParseUpstreamErrorAgreesWithNewHTTPErrorOnClassification(t *testing.T) {
	bodies := []string{
		`{"error":{"message":"m","type":"whatever","code":"c"}}`,
		`{"detail":"m"}`,
		`<html>boom</html>`,
		``,
	}
	for status := 400; status <= 599; status++ {
		want := NewHTTPError(status, "x")
		for _, body := range bodies {
			got := ParseUpstreamError(status, []byte(body))
			if got.Type != want.Type || got.Retryable != want.Retryable || got.StatusCode != status {
				t.Fatalf("status=%d body=%.20s：ParseUpstreamError=(%q,%v,%d) NewHTTPError=(%q,%v,%d)",
					status, body, got.Type, got.Retryable, got.StatusCode, want.Type, want.Retryable, want.StatusCode)
			}
		}
	}
}

// 认不出形状的 body 仍要把原文交给客户端：HTML 错误页、代理插的空壳 JSON 是唯一
// 的诊断线索，不能因为「不认识」就退化成一句 "upstream error"。
func TestParseUpstreamErrorFallsBackToRawBody(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"html", "  <html><body>502 Bad Gateway</body></html>  ", "<html><body>502 Bad Gateway</body></html>"},
		{"plain text", "upstream said no", "upstream said no"},
		{"json without message", `{"foo":1}`, `{"foo":1}`},
		{"empty error string", `{"error":""}`, `{"error":""}`},
		{"empty body", "", "upstream error"},
		{"whitespace body", "   \n ", "upstream error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ParseUpstreamError(502, []byte(tc.body)).Message; got != tc.want {
				t.Errorf("Message = %q，want %q", got, tc.want)
			}
		})
	}
}

// 回落成原文时同样要截断：上游可能吐一整页 HTML，不截会灌进客户端错误体与日志。
func TestParseUpstreamErrorTruncatesRawFallback(t *testing.T) {
	raw := strings.Repeat("x", upstreamErrMessageLimit+200)
	got := ParseUpstreamError(502, []byte(raw)).Message
	if !strings.HasSuffix(got, "...") {
		t.Fatalf("长原文未截断：len=%d", len(got))
	}
	if len(got) != upstreamErrMessageLimit+len("...") {
		t.Errorf("截断长度=%d，want %d", len(got), upstreamErrMessageLimit+len("..."))
	}
	// 解析成功时不截断：上游自己写的消息本来就短，截了反而丢信息。
	short := `{"error":{"message":"` + strings.Repeat("y", upstreamErrMessageLimit+50) + `"}}`
	if got := ParseUpstreamError(400, []byte(short)).Message; strings.HasSuffix(got, "...") {
		t.Error("解析出的消息不应被截断")
	}
}

// 数字形态的 code 不是错误码，只是把 HTTP 状态码又说了一遍（Gemini 的 error.code、
// Azure 的 code:"429"）。收下会让客户端把 429 当业务错误码去 switch。
func TestParseUpstreamErrorNumericCodeIsNotAnErrorCode(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"gemini numeric code", `{"error":{"code":429,"message":"m"}}`, ""},
		{"gemini numeric code with status", `{"error":{"code":429,"message":"m","status":"RESOURCE_EXHAUSTED"}}`, "RESOURCE_EXHAUSTED"},
		{"azure numeric string code", `{"error":{"code":"429","message":"m"}}`, ""},
		{"null code", `{"error":{"message":"m","code":null}}`, ""},
		{"absent code", `{"error":{"message":"m"}}`, ""},
		{"real string code", `{"error":{"message":"m","code":"context_length_exceeded"}}`, "context_length_exceeded"},
		// status 是数字（信封形态）时同样只算状态码回声，不得当错误码收下。
		{"numeric status", `{"error":{"code":429,"message":"m","status":503}}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ParseUpstreamError(429, []byte(tc.body)).Code; got != tc.want {
				t.Errorf("Code = %q，want %q", got, tc.want)
			}
		})
	}
}

// 畸形 JSON 不得 panic，也不得把半个 body 当消息：一律回落原文。
func TestParseUpstreamErrorSurvivesMalformedBody(t *testing.T) {
	for _, body := range []string{
		`{"error":`, `{"error":{"message":`, `[1,2,3]`, `{"error":[1]}`,
		`{"error":{"code":"a","message":123}}`, "\x00\x01binary", `{"message":null}`,
	} {
		e := ParseUpstreamError(500, []byte(body))
		if e == nil || e.Message == "" || e.Type != ErrTypeOverloaded || !e.Retryable {
			t.Errorf("body=%q -> %+v，want 非空消息 + overloaded/true", body, e)
		}
	}
}

// message 自身解不出来（数字型）时消息回落原文，但同一个 error 对象里解得好的
// code 不得跟着丢：归因靠的是 code，回落原文只负责让失败可见。
func TestParseUpstreamErrorKeepsCodeWhenMessageUnusable(t *testing.T) {
	e := ParseUpstreamError(400, []byte(`{"error":{"code":"context_length_exceeded","message":123}}`))
	if e.Code != "context_length_exceeded" {
		t.Errorf("Code = %q，want context_length_exceeded", e.Code)
	}
	if !strings.Contains(e.Message, "context_length_exceeded") {
		t.Errorf("Message = %q，want 回落原文", e.Message)
	}
}
