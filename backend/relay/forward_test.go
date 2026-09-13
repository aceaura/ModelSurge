package relay

import "testing"

// 账号级自定义头须并入协议默认头，同名键覆盖协议值（如网关会话头）。
func TestStaticEndpointExtraHeaders(t *testing.T) {
	resolve := staticEndpoint("openai-chat", "https://gw.example/zen/go", "sk-test", "m1",
		map[string]string{"x-opencode-session": "sess-1", "Authorization": "Bearer override"})
	url, headers, err := resolve(t.Context())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if url != "https://gw.example/zen/go/v1/chat/completions" {
		t.Fatalf("url = %q", url)
	}
	if headers["x-opencode-session"] != "sess-1" {
		t.Fatalf("x-opencode-session = %q", headers["x-opencode-session"])
	}
	if headers["Authorization"] != "Bearer override" {
		t.Fatalf("Authorization = %q, want override", headers["Authorization"])
	}
	if headers["Content-Type"] != "" { // 协议默认头不该被 extra 清掉
		t.Fatalf("protocol headers lost: %v", headers)
	}

	// 无 extra：行为与原来一致
	url2, headers2, _ := staticEndpoint("anthropic", "https://up.example", "sk-up", "m1", nil)(t.Context())
	if url2 != "https://up.example/v1/messages" || headers2["x-api-key"] != "sk-up" {
		t.Fatalf("plain endpoint changed: %q %v", url2, headers2)
	}
}
