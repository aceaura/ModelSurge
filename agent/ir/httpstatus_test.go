package ir

import "testing"

// StatusCode 为零时（流内错误一律如此）必须按 Type 反推，而不是一律 500：
// 不可重试的类别退化成 500 会让客户端反复重试同一个必然失败的请求。
func TestHTTPStatusFallsBackToType(t *testing.T) {
	cases := []struct {
		typ  string
		want int
	}{
		{ErrTypeInvalidReq, 400},
		{ErrTypeContentFilter, 400},
		{ErrTypeAuth, 401},
		{ErrTypePermission, 403},
		{ErrTypeNotFound, 404},
		{ErrTypeRateLimit, 429},
		{ErrTypeOverloaded, 503},
		{ErrTypeUpstream, 500},
		{"", 500},
		{"something-nobody-defined", 500},
	}
	for _, c := range cases {
		if got := (&Error{Type: c.typ}).HTTPStatus(); got != c.want {
			t.Errorf("HTTPStatus(type=%q) = %d, want %d", c.typ, got, c.want)
		}
	}
}

// 显式状态码优先：上游给了真实码就不能被类型推断覆盖。
func TestHTTPStatusPrefersExplicitCode(t *testing.T) {
	e := &Error{StatusCode: 418, Type: ErrTypeRateLimit}
	if got := e.HTTPStatus(); got != 418 {
		t.Errorf("HTTPStatus = %d, want 418（显式码优先）", got)
	}
	if got := (*Error)(nil).HTTPStatus(); got != 500 {
		t.Errorf("nil.HTTPStatus = %d, want 500", got)
	}
}

// 与 ClassifyStatus 互为逆：每个规范类型都要能往返回自己，
// 否则「按类型反推状态码再按状态码分类」会漂到别的类型上。
func TestHTTPStatusRoundTripsClassifyStatus(t *testing.T) {
	// content_filter 与 invalid_request 同为 400，反推必然归一到后者；
	// 这是刻意的（HTTP 层只有一个 400），不参与往返。
	for _, typ := range []string{
		ErrTypeInvalidReq, ErrTypeAuth, ErrTypePermission,
		ErrTypeNotFound, ErrTypeRateLimit,
	} {
		status := (&Error{Type: typ}).HTTPStatus()
		if back, _ := ClassifyStatus(status); back != typ {
			t.Errorf("type %q -> %d -> %q，未往返", typ, status, back)
		}
	}
	if status := (&Error{Type: ErrTypeOverloaded}).HTTPStatus(); status != 503 {
		t.Errorf("overloaded -> %d, want 503", status)
	} else if back, _ := ClassifyStatus(status); back != ErrTypeOverloaded {
		t.Errorf("overloaded -> 503 -> %q，未往返", back)
	}
}

// 流内错误没有状态码，只能按类型判可重试性。口径必须与 ClassifyStatus 一致，
// 否则同一个错误在流式与非流式两条路径上会得出相反的重试结论。
func TestStreamRetryableMatchesClassifyStatus(t *testing.T) {
	for _, typ := range []string{
		ErrTypeInvalidReq, ErrTypeAuth, ErrTypePermission, ErrTypeNotFound,
		ErrTypeRateLimit, ErrTypeOverloaded, ErrTypeContentFilter, ErrTypeUpstream,
	} {
		_, want := ClassifyStatus((&Error{Type: typ}).HTTPStatus())
		if got := StreamRetryable(typ); got != want {
			t.Errorf("StreamRetryable(%q) = %v，但按状态码分类是 %v（两条路径口径漂移）", typ, got, want)
		}
	}
}

func TestStreamRetryableTable(t *testing.T) {
	cases := []struct {
		typ  string
		want bool
	}{
		// 换谁都会被同样拒绝
		{ErrTypeInvalidReq, false},
		{ErrTypeNotFound, false},
		{ErrTypeContentFilter, false},
		// 换一个账号/目标就可能成功
		{ErrTypeAuth, true},
		{ErrTypePermission, true},
		{ErrTypeRateLimit, true},
		{ErrTypeOverloaded, true},
		// 未知类型默认可重试，与各族解码器此前行为一致
		{ErrTypeUpstream, true},
		{"", true},
		{"service_unavailable_error", true},
	}
	for _, c := range cases {
		if got := StreamRetryable(c.typ); got != c.want {
			t.Errorf("StreamRetryable(%q) = %v, want %v", c.typ, got, c.want)
		}
	}
}
