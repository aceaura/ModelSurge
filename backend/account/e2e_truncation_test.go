// e2e_truncation_test.go 截断恢复端到端测试（任务组 9.2）。
// mock Kiro 流返回被截断的工具参数 / 无完成信号的正文 ->
// 网关记录；客户端下次请求携带对应 tool_use_id / 回显正文 ->
// 网关注入合成提示（Kiro 载荷中可见）；开关关闭时不注入。
package account_test

import (
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"relayd/backend/config"
)

// kiroFrames 把若干 Kiro 事件 JSON 逐个包成 eventstream 帧。
func kiroFrames(events ...string) []byte {
	var out []byte
	for _, e := range events {
		out = append(out, kiroFrame([]byte(e))...)
	}
	return out
}

// truncatedToolStream 被上游截断的工具调用流：参数 JSON 残缺（缺右括号）、
// 无 usage/context 完成信号。
func truncatedToolStream() []byte {
	return kiroFrames(
		`{"name":"Write","toolUseId":"toolu_9"}`,
		`{"input":"{\"path\":\"/very/lo"}`,
		`{"input":"ng/file.txt\""`,
		`{"stop":true}`,
	)
}

// truncatedContentStream 无完成信号但有正文的流（上游截断正文）。
func truncatedContentStream(text string) []byte {
	return kiroFrames(`{"content":` + quoteJSON(text) + `}`)
}

// quoteJSON 编码字符串为 JSON 字面量。
func quoteJSON(s string) string {
	// 测试内只用 ASCII 安全字符，简化处理
	return `"` + s + `"`
}

// bodyCapture 并发安全地捕获上游收到的请求体。
type bodyCapture struct {
	mu     sync.Mutex
	bodies []string
}

func (c *bodyCapture) add(body string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.bodies = append(c.bodies, body)
}

func (c *bodyCapture) get(i int) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if i >= len(c.bodies) {
		return ""
	}
	return c.bodies[i]
}

// chatCaptureFlows 返回按调用次序切换响应的 chat 处理器，
// 并捕获每次请求体。
func chatCaptureFlows(cap *bodyCapture, responses ...[]byte) func(w http.ResponseWriter, r *http.Request) {
	var calls int
	var mu sync.Mutex
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		cap.add(string(body))
		mu.Lock()
		i := calls
		calls++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		w.WriteHeader(200)
		if i < len(responses) {
			_, _ = w.Write(responses[i])
			return
		}
		_, _ = w.Write(kiroChatStream("OK"))
	}
}

// 工具参数截断：下次请求的 tool_result 前置 "[API Limitation]" 提示。
func TestE2ETruncationToolRecovery(t *testing.T) {
	cap := &bodyCapture{}
	gw, _, st := newKiroEnvWithConfig(t, &config.Config{
		Scheduler:                 &config.Scheduler{SameAccountRetries: 2},
		TruncationRecoveryEnabled: true,
	}, chatCaptureFlows(cap, truncatedToolStream()))

	// 请求 1：收到被截断的 tool_use（stop_reason=tool_use）
	status, body := postChat(t, gw.URL, "/v1/messages", anthropicChat)
	if status != 200 || !strings.Contains(body, `"tool_use"`) || !strings.Contains(body, "toolu_9") {
		t.Fatalf("first response missing truncated tool_use: status=%d body=%s", status, body)
	}
	if got := st.chat.Load(); got != 1 {
		t.Fatalf("chat called %d times, want 1", got)
	}

	// 请求 2：客户端回传 tool_result（引用被截断的 tool_use_id）
	second := `{"model":"claude-sonnet-4-5","max_tokens":100,"messages":[` +
		`{"role":"user","content":"hi"},` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"toolu_9","name":"Write","input":{}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_9","content":"original output"}]}]}`
	status, body = postChat(t, gw.URL, "/v1/messages", second)
	if status != 200 {
		t.Fatalf("second request: status=%d body=%s", status, body)
	}

	// 注入断言：发给 Kiro 的载荷含截断提示
	upstream := cap.get(1)
	if !strings.Contains(upstream, "[API Limitation]") {
		t.Errorf("truncation notice not injected into upstream payload:\n%s", upstream)
	}
	if !strings.Contains(upstream, "original output") {
		t.Errorf("original tool result must be retained after notice:\n%s", upstream)
	}

	// 一次性：第三次请求同 tool_use_id 不再注入
	third := strings.Replace(second, `"content":"original output"`, `"content":"retry output"`, 1)
	if status, _ := postChat(t, gw.URL, "/v1/messages", third); status != 200 {
		t.Fatalf("third request: status=%d", status)
	}
	if upstream3 := cap.get(2); strings.Contains(upstream3, "[API Limitation]") {
		t.Errorf("notice must be one-shot, got injected again:\n%s", upstream3)
	}
}

// 正文截断：下次请求回显的 assistant 正文后追加合成 user 提示。
func TestE2ETruncationContentRecovery(t *testing.T) {
	cap := &bodyCapture{}
	gw, _, _ := newKiroEnvWithConfig(t, &config.Config{
		Scheduler:                 &config.Scheduler{SameAccountRetries: 2},
		TruncationRecoveryEnabled: true,
	}, chatCaptureFlows(cap, truncatedContentStream("PARTIAL_RESPONSE_TEXT")))

	// 请求 1：收到截断正文（stop_reason=max_tokens）
	status, body := postChat(t, gw.URL, "/v1/messages", anthropicChat)
	if status != 200 || !strings.Contains(body, "PARTIAL_RESPONSE_TEXT") {
		t.Fatalf("first response missing truncated text: status=%d body=%s", status, body)
	}
	if !strings.Contains(body, `"max_tokens"`) {
		t.Fatalf("truncated content should surface stop_reason=max_tokens: %s", body)
	}

	// 请求 2：客户端回显被截断的 assistant 正文
	second := `{"model":"claude-sonnet-4-5","max_tokens":100,"messages":[` +
		`{"role":"user","content":"hi"},` +
		`{"role":"assistant","content":[{"type":"text","text":"PARTIAL_RESPONSE_TEXT"}]},` +
		`{"role":"user","content":"continue"}]}`
	if status, body := postChat(t, gw.URL, "/v1/messages", second); status != 200 {
		t.Fatalf("second request: status=%d body=%s", status, body)
	}

	upstream := cap.get(1)
	if !strings.Contains(upstream, "[System Notice]") {
		t.Errorf("content truncation notice not injected:\n%s", upstream)
	}
	if !strings.Contains(upstream, "PARTIAL_RESPONSE_TEXT") {
		t.Errorf("original assistant text must be retained:\n%s", upstream)
	}
}

// 开关关闭：不记录不注入，上游载荷原样。
func TestE2ETruncationDisabled(t *testing.T) {
	cap := &bodyCapture{}
	gw, _, _ := newKiroEnvWithConfig(t, &config.Config{
		Scheduler:                 &config.Scheduler{SameAccountRetries: 2},
		TruncationRecoveryEnabled: false,
	}, chatCaptureFlows(cap, truncatedToolStream()))

	if status, _ := postChat(t, gw.URL, "/v1/messages", anthropicChat); status != 200 {
		t.Fatal("first request failed")
	}
	second := `{"model":"claude-sonnet-4-5","max_tokens":100,"messages":[` +
		`{"role":"user","content":"hi"},` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"toolu_9","name":"Write","input":{}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_9","content":"original output"}]}]}`
	if status, _ := postChat(t, gw.URL, "/v1/messages", second); status != 200 {
		t.Fatal("second request failed")
	}
	if upstream := cap.get(1); strings.Contains(upstream, "[API Limitation]") {
		t.Errorf("disabled tracker must not inject:\n%s", upstream)
	}
}
