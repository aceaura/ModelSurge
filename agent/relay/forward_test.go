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

// codex 协议：无 /v1 的 /responses 路径 + 配套身份头；账号 Headers 覆盖默认值。
func TestCodexEndpoint(t *testing.T) {
	resolve := staticEndpoint("codex", "https://chatgpt.com/backend-api/codex", "eyJtok", "gpt-5.6-sol",
		map[string]string{"chatgpt-account-id": "acc-123"})
	url, headers, err := resolve(t.Context())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if url != "https://chatgpt.com/backend-api/codex/responses" {
		t.Fatalf("url = %q", url)
	}
	if headers["Authorization"] != "Bearer eyJtok" {
		t.Fatalf("Authorization = %q", headers["Authorization"])
	}
	if headers["originator"] != "codex-tui" || headers["version"] == "" || headers["OpenAI-Beta"] != "responses=experimental" {
		t.Fatalf("codex identity headers missing: %v", headers)
	}
	if ua := headers["User-Agent"]; len(ua) < len("codex-tui/") || ua[:10] != "codex-tui/" {
		t.Fatalf("User-Agent = %q, want codex-tui/<version> ...", ua)
	}
	if headers["session_id"] == "" {
		t.Fatalf("session_id missing")
	}
	// 每次解析都生成新 session_id（每请求隔离，对齐 sub2api 语义）
	_, headers3, _ := staticEndpoint("codex", "https://chatgpt.com/backend-api/codex", "t", "m", nil)(t.Context())
	if headers3["session_id"] == headers["session_id"] {
		t.Fatalf("session_id should be per-request unique: %q", headers3["session_id"])
	}
	if headers3["chatgpt-account-id"] != "" { // 未配置时不强造
		t.Fatalf("chatgpt-account-id should be absent unless configured")
	}

	// base_url 缺省兜底官方订阅端点
	url4, _, _ := staticEndpoint("codex", "", "t", "m", nil)(t.Context())
	if url4 != "https://chatgpt.com/backend-api/codex/responses" {
		t.Fatalf("default base url = %q", url4)
	}
}
