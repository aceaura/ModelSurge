package openairesponses

import (
	"strings"
	"testing"
)

// R103-5 created_at 保真：上游给过的创建时间同族往返不得被代理本地钟覆盖。
func TestResponseCreatedAtRoundTrip(t *testing.T) {
	body := []byte(`{"id":"r1","object":"response","created_at":1700000000,"model":"m","status":"completed",` +
		`"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}]}`)
	resp, err := New().DecodeResponse(body)
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if resp.Created != 1700000000 {
		t.Fatalf("上游 created_at 没落进 IR：%d", resp.Created)
	}
	out, err := New().EncodeResponse(resp)
	if err != nil {
		t.Fatalf("EncodeResponse: %v", err)
	}
	if !strings.Contains(string(out), `"created_at":1700000000`) {
		t.Errorf("出站 created_at 被本地钟覆盖：%s", out)
	}
}

// 上游没给创建时间（零值）时才回退本地钟：created_at 必须存在且接近当前时间，
// 不能写成 0。
func TestResponseCreatedAtFallback(t *testing.T) {
	resp, err := New().DecodeResponse([]byte(`{"id":"r1","model":"m","status":"completed","output":[]}`))
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	out, err := New().EncodeResponse(resp)
	if err != nil {
		t.Fatalf("EncodeResponse: %v", err)
	}
	s := string(out)
	if strings.Contains(s, `"created_at":0`) || !strings.Contains(s, `"created_at":`) {
		t.Errorf("缺省回退没写出有效 created_at：%s", s)
	}
}
