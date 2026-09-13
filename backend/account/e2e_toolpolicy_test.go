// e2e_toolpolicy_test.go kiro 严格 tool_choice 缓冲-校验-恢复重发的端到端测试。
// 覆盖：required 违规恢复成功 / none 二次违规 502 / named 换工具恢复 /
// 非流式客户端 / none + web_search 拦截次序（拦截先于校验）/ auto 不缓冲。
package account_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"relayd/backend/account"
	"relayd/backend/ir"
	"relayd/backend/proto/kiro"
)

// kiroRespStream 任意 IR 响应 -> kiro 帧流（通用 chat 流构造）。
func kiroRespStream(t *testing.T, resp *ir.Response) []byte {
	t.Helper()
	body, err := (kiro.Codec{}).EncodeResponse(resp)
	if err != nil {
		t.Fatalf("encode kiro response: %v", err)
	}
	var out []byte
	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		out = append(out, kiroFrame([]byte(line))...)
	}
	return out
}

// toolPolicyEnv 起多响应序列 mock：第 i 次聊天请求存 bodies[i] 并回
// responses[min(i, len-1)]。返回网关、bodies 与 mock 状态。
func toolPolicyEnv(t *testing.T, kiroAcc *account.KiroAccount, responses ...*ir.Response) (*httptest.Server, *[]string) {
	t.Helper()
	var bodies []string
	seq := 0
	chat := func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		i := seq
		seq++
		if i >= len(responses) {
			i = len(responses) - 1
		}
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		w.WriteHeader(200)
		_, _ = w.Write(kiroRespStream(t, responses[i]))
	}
	gw, _ := newKiroFeatureEnv(t, kiroAcc, chat)
	return gw, &bodies
}

func textOnlyResp(t *testing.T, s string) *ir.Response {
	return &ir.Response{Model: "claude-sonnet-4-5", Content: []ir.Block{{Type: ir.BlockText, Text: s}}, StopReason: ir.StopEndTurn}
}

func toolUseResp(t *testing.T, name string) *ir.Response {
	return &ir.Response{Model: "claude-sonnet-4-5", Content: []ir.Block{
		{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: "toolu_1", Name: name, Input: json.RawMessage(`{"path":"a.go"}`)}},
	}, StopReason: ir.StopToolUse}
}

// required：首次纯文本（违规）-> 恢复指令注入后重发 -> 工具调用（通过）。
func TestE2EToolPolicyRequiredRecovery(t *testing.T) {
	gw, bodies := toolPolicyEnv(t,
		&account.KiroAccount{Source: account.SourceRefreshToken, RefreshToken: "rt-1"},
		textOnlyResp(t, "no tools here"),
		toolUseResp(t, "read_file"),
	)
	req := `{"model":"claude-sonnet-4-5","max_tokens":100,
		"tools":[{"name":"read_file","description":"d","input_schema":{"type":"object"}}],
		"tool_choice":{"type":"any"},
		"messages":[{"role":"user","content":"read the file"}]}`
	status, body := postChat(t, gw.URL, "/v1/messages", req)
	if status != 200 {
		t.Fatalf("status = %d, body = %s", status, body)
	}
	if !strings.Contains(body, `"name":"read_file"`) {
		t.Errorf("client response missing tool_use: %s", body)
	}
	if len(*bodies) != 2 {
		t.Fatalf("upstream calls = %d, want 2 (violation retry)", len(*bodies))
	}
	if !strings.Contains(unescapeJSON((*bodies)[1]), "[Tool Policy Recovery]") ||
		!strings.Contains((*bodies)[1], "tool_choice required returned no tools") {
		t.Errorf("retry payload missing recovery directive: %s", (*bodies)[1])
	}
	if strings.Contains(unescapeJSON((*bodies)[0]), "[Tool Policy Recovery]") {
		t.Errorf("first payload must not carry directive: %s", (*bodies)[0])
	}
}

// none：上游坚持回工具 -> 两次违规 -> 502 tool_choice_not_satisfied。
func TestE2EToolPolicyNoneViolation502(t *testing.T) {
	gw, bodies := toolPolicyEnv(t,
		&account.KiroAccount{Source: account.SourceRefreshToken, RefreshToken: "rt-1"},
		toolUseResp(t, "read_file"),
	)
	req := `{"model":"claude-sonnet-4-5","max_tokens":100,
		"tools":[{"name":"read_file","description":"d","input_schema":{"type":"object"}}],
		"tool_choice":{"type":"none"},
		"messages":[{"role":"user","content":"hi"}]}`
	status, body := postChat(t, gw.URL, "/v1/messages", req)
	if status != 502 {
		t.Fatalf("status = %d, body = %s", status, body)
	}
	if !strings.Contains(body, "tool_choice_not_satisfied") {
		t.Errorf("body missing tool_choice_not_satisfied: %s", body)
	}
	if n := len(*bodies); n != 2 {
		t.Fatalf("upstream calls = %d, want 2 (one retry only)", n)
	}
}

// named：上游回了别的工具 -> 恢复指令点名 -> 正确工具。
func TestE2EToolPolicyNamedSwitchTool(t *testing.T) {
	gw, bodies := toolPolicyEnv(t,
		&account.KiroAccount{Source: account.SourceRefreshToken, RefreshToken: "rt-1"},
		toolUseResp(t, "write_file"),
		toolUseResp(t, "read_file"),
	)
	req := `{"model":"claude-sonnet-4-5","max_tokens":100,
		"tools":[
			{"name":"read_file","description":"d","input_schema":{"type":"object"}},
			{"name":"write_file","description":"d","input_schema":{"type":"object"}}],
		"tool_choice":{"type":"tool","name":"read_file"},
		"messages":[{"role":"user","content":"read it"}]}`
	status, body := postChat(t, gw.URL, "/v1/messages", req)
	if status != 200 {
		t.Fatalf("status = %d, body = %s", status, body)
	}
	if !strings.Contains(body, `"name":"read_file"`) {
		t.Errorf("client response missing read_file tool_use: %s", body)
	}
	if len(*bodies) != 2 {
		t.Fatalf("upstream calls = %d, want 2", len(*bodies))
	}
	if !strings.Contains(unescapeJSON((*bodies)[1]), "tool_choice violated") &&
		!strings.Contains(unescapeJSON((*bodies)[1]), "other than required tool") {
		t.Errorf("retry payload missing violation reason: %s", (*bodies)[1])
	}
}

// 非流式客户端走同一条恢复路径。
func TestE2EToolPolicyNonStream(t *testing.T) {
	gw, bodies := toolPolicyEnv(t,
		&account.KiroAccount{Source: account.SourceRefreshToken, RefreshToken: "rt-1"},
		textOnlyResp(t, "nope"),
		toolUseResp(t, "read_file"),
	)
	req := `{"model":"claude-sonnet-4-5","max_tokens":100,
		"tools":[{"name":"read_file","description":"d","input_schema":{"type":"object"}}],
		"tool_choice":{"type":"any"},
		"messages":[{"role":"user","content":"read the file"}]}`
	// 客户端非流式（无 stream 字段）+ 上游流式：聚合后校验
	status, body := postChat(t, gw.URL, "/v1/messages", req)
	if status != 200 {
		t.Fatalf("status = %d, body = %s", status, body)
	}
	if !strings.Contains(body, `"name":"read_file"`) {
		t.Errorf("client response missing tool_use: %s", body)
	}
	if len(*bodies) != 2 {
		t.Fatalf("upstream calls = %d, want 2", len(*bodies))
	}
}

// none + web_search：拦截（改写为 server_tool_use）必须先于校验，
// 否则 hosted 调用会被误判为违规。账号开 web_search、上游回 web_search 调用。
func TestE2EToolPolicyNoneWebSearchOrder(t *testing.T) {
	webSearchResp := &ir.Response{Model: "claude-sonnet-4-5", Content: []ir.Block{
		{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{
			ID: "toolu_ws1", Name: "web_search", Input: json.RawMessage(`{"query":"go"}`),
		}},
	}, StopReason: ir.StopToolUse}
	gw, bodies := toolPolicyEnv(t,
		&account.KiroAccount{Source: account.SourceRefreshToken, RefreshToken: "rt-1", WebSearch: true},
		webSearchResp,
	)
	req := `{"model":"claude-sonnet-4-5","max_tokens":100,
		"tools":[{"name":"read_file","description":"d","input_schema":{"type":"object"}}],
		"tool_choice":{"type":"none"},
		"messages":[{"role":"user","content":"search go release"}]}`
	status, body := postChat(t, gw.URL, "/v1/messages", req)
	if status != 200 {
		t.Fatalf("status = %d, body = %s", status, body)
	}
	if !strings.Contains(body, "server_tool_use") {
		t.Errorf("expected intercepted web_search as server_tool_use: %s", body)
	}
	if n := len(*bodies); n != 1 {
		t.Fatalf("upstream calls = %d, want 1 (no violation retry)", n)
	}
}

// 流式客户端：校验通过后一次性重放事件流（缓冲期间不写任何字节）。
func TestE2EToolPolicyStreamReplay(t *testing.T) {
	gw, bodies := toolPolicyEnv(t,
		&account.KiroAccount{Source: account.SourceRefreshToken, RefreshToken: "rt-1"},
		textOnlyResp(t, "no tools here"),
		toolUseResp(t, "read_file"),
	)
	req := `{"model":"claude-sonnet-4-5","max_tokens":100,"stream":true,
		"tools":[{"name":"read_file","description":"d","input_schema":{"type":"object"}}],
		"tool_choice":{"type":"any"},
		"messages":[{"role":"user","content":"read the file"}]}`
	status, body := postChat(t, gw.URL, "/v1/messages", req)
	if status != 200 {
		t.Fatalf("status = %d, body = %s", status, body)
	}
	if !strings.Contains(body, `"name":"read_file"`) || !strings.Contains(body, "event: message_stop") &&
		!strings.Contains(body, `"type":"message_stop"`) {
		t.Errorf("stream replay missing tool_use or stop: %s", body)
	}
	if n := len(*bodies); n != 2 {
		t.Fatalf("upstream calls = %d, want 2", n)
	}
}

// auto：不缓冲不校验，一次上游调用直通。
func TestE2EToolPolicyAutoNoBuffer(t *testing.T) {
	gw, bodies := toolPolicyEnv(t,
		&account.KiroAccount{Source: account.SourceRefreshToken, RefreshToken: "rt-1"},
		toolUseResp(t, "read_file"),
	)
	req := `{"model":"claude-sonnet-4-5","max_tokens":100,
		"tools":[{"name":"read_file","description":"d","input_schema":{"type":"object"}}],
		"tool_choice":{"type":"auto"},
		"messages":[{"role":"user","content":"maybe read"}]}`
	status, body := postChat(t, gw.URL, "/v1/messages", req)
	if status != 200 {
		t.Fatalf("status = %d, body = %s", status, body)
	}
	if !strings.Contains(body, `"name":"read_file"`) {
		t.Errorf("client response missing tool_use: %s", body)
	}
	if n := len(*bodies); n != 1 {
		t.Fatalf("upstream calls = %d, want 1 (no buffering for auto)", n)
	}
}
