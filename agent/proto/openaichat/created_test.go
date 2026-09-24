package openaichat

import (
	"strings"
	"testing"
)

// R103-5 created 保真：上游给过的创建时间同族往返不得被代理本地钟覆盖。
func TestResponseCreatedRoundTrip(t *testing.T) {
	body := []byte(`{"id":"c1","object":"chat.completion","created":1700000000,"model":"m",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`)
	resp, _, err := (codec{}).DecodeResponseWithNotes(body)
	if err != nil {
		t.Fatalf("DecodeResponseWithNotes: %v", err)
	}
	if resp.Created != 1700000000 {
		t.Fatalf("上游 created 没落进 IR：%d", resp.Created)
	}
	out, err := codec{}.EncodeResponse(resp)
	if err != nil {
		t.Fatalf("EncodeResponse: %v", err)
	}
	if !strings.Contains(string(out), `"created":1700000000`) {
		t.Errorf("出站 created 被本地钟覆盖：%s", out)
	}
}

// 上游没给创建时间（零值）时才回退本地钟：created 必须存在且非零。
func TestResponseCreatedFallback(t *testing.T) {
	resp, _, err := (codec{}).DecodeResponseWithNotes([]byte(`{"choices":[{"index":0,"message":{"role":"assistant","content":"x"},"finish_reason":"stop"}]}`))
	if err != nil {
		t.Fatalf("DecodeResponseWithNotes: %v", err)
	}
	out, err := codec{}.EncodeResponse(resp)
	if err != nil {
		t.Fatalf("EncodeResponse: %v", err)
	}
	s := string(out)
	if strings.Contains(s, `"created":0`) || !strings.Contains(s, `"created":`) {
		t.Errorf("缺省回退没写出有效 created：%s", s)
	}
}
