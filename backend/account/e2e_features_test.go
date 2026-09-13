// e2e_features_test.go 账号级功能开关端到端测试（任务组 10.3 / 11.3）。
// web_search：mock 上游 /generateAssistantResponse 返回带 web_search 工具调用的
// kiro 事件流；/mcp 返回 JSON-RPC 搜索结果（二层 JSON）。
// fake_reasoning：账号开关 → 上游载荷带思考控制标签。
// count_tokens：kiro 账号命中时按 kiro tokenizer 语义本地估算返回。
package account_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"relayd/backend/account"
	"relayd/backend/config"
	"relayd/backend/ir"
	"relayd/backend/proto/kiro"
	"relayd/backend/server"
)

// kiroToolCallStream 构造含 web_search 工具调用的 kiro 响应流
// （text 前缀 + tool_use 块，stop tool_use）。
func kiroToolCallStream() []byte {
	body, err := (kiro.Codec{}).EncodeResponse(&ir.Response{
		Model: "claude-sonnet-4-5",
		Content: []ir.Block{
			{Type: ir.BlockText, Text: "let me search"},
			{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{
				ID: "toolu_ws1", Name: "web_search",
				Input: json.RawMessage(`{"query":"go release"}`),
			}},
		},
		StopReason: ir.StopToolUse,
	})
	if err != nil {
		panic(err)
	}
	var out []byte
	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		out = append(out, kiroFrame([]byte(line))...)
	}
	return out
}

// webSearchChatHandler 标准聊天 mock：返回 web_search 工具调用流，
// 并把请求体存入 gotBody 供注入断言。
func webSearchChatHandler(gotBody *string) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		*gotBody = string(b)
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		w.WriteHeader(200)
		_, _ = w.Write(kiroToolCallStream())
	}
}

// newKiroFeatureEnv 同 newKiroEnv 但 kiro 账号身份（功能开关等）可配。
func newKiroFeatureEnv(t *testing.T, kiro *account.KiroAccount, chat func(w http.ResponseWriter, r *http.Request)) (*httptest.Server, *kiroMockState) {
	t.Helper()
	return newKiroFeatureEnvCfg(t, kiro, nil, chat)
}

// newKiroFeatureEnvCfg 同上但支持全局 kiro 段配置（任务组 13）。
func newKiroFeatureEnvCfg(t *testing.T, kiro *account.KiroAccount, kiroCfg *config.Kiro, chat func(w http.ResponseWriter, r *http.Request)) (*httptest.Server, *kiroMockState) {
	t.Helper()
	st := newKiroMock(t, chat)
	store, err := account.Open(filepath.Join(t.TempDir(), "kiro.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.InsertAccount(&account.Account{
		Name: "k1-kiro", Type: account.TypeKiro, Enabled: true, Kiro: kiro,
	}); err != nil {
		t.Fatal(err)
	}
	m, err := account.NewManager(store, account.Cooldowns{}, account.ManagerDeps{})
	if err != nil {
		t.Fatal(err)
	}
	m.SetProbeRateForTest(0)
	cfg := &config.Config{Scheduler: &config.Scheduler{SameAccountRetries: 2}, Kiro: kiroCfg}
	gw := httptest.NewServer(server.New(cfg, m).Handler())
	t.Cleanup(gw.Close)
	return gw, st
}

// newWebSearchEnv WebSearch 开关可配的 kiro 网关。
func newWebSearchEnv(t *testing.T, webSearch bool, chat func(w http.ResponseWriter, r *http.Request)) (*httptest.Server, *kiroMockState) {
	t.Helper()
	return newKiroFeatureEnv(t, &account.KiroAccount{
		Source: account.SourceRefreshToken, RefreshToken: "rt-1", WebSearch: webSearch,
	}, chat)
}

// unescapeJSON 断言辅助：还原 Go json 默认的 HTML 转义与引号转义
// （网关输出 \u003cweb_search\u003e 与 <web_search> 语义等价）。
func unescapeJSON(s string) string {
	for _, r := range [][2]string{
		{`\u003c`, "<"}, {`\u003e`, ">"}, {`\u0026`, "&"}, {`\"`, `"`},
	} {
		s = strings.ReplaceAll(s, r[0], r[1])
	}
	return s
}

// 账号开关开启（Path B）：注入工具声明、拦截调用、MCP 代执行、
// 客户端收到 server_tool_use + web_search_tool_result + 摘要 + end_turn。
func TestE2EKiroWebSearchIntercept(t *testing.T) {
	for _, stream := range []bool{false, true} {
		stream := stream
		t.Run(map[bool]string{false: "nonstream", true: "stream"}[stream], func(t *testing.T) {
			var gotBody string
			gw, st := newWebSearchEnv(t, true, webSearchChatHandler(&gotBody))
			path, body := clientBody("anthropic", stream)
			status, respBody := postChat(t, gw.URL, path, body)
			if status != 200 {
				t.Fatalf("status = %d, body = %s", status, respBody)
			}
			respBody = unescapeJSON(respBody)
			for _, want := range []string{
				`"type":"server_tool_use"`,
				`"type":"web_search_tool_result"`,
				`"encrypted_content":"fast"`,
				`<web_search>`,
				`Search results for "go release"`,
				`"end_turn"`,
				`"stop_reason":"end_turn"`,
			} {
				if !strings.Contains(respBody, want) {
					t.Errorf("response missing %s: %s", want, respBody)
				}
			}
			// 原始 tool_use 块被替换，不透传给客户端
			if strings.Contains(respBody, "toolu_ws1") {
				t.Errorf("raw tool_use id leaked: %s", respBody)
			}
			// 注入：上游请求体带 web_search 工具声明
			if !strings.Contains(gotBody, `"web_search"`) {
				t.Errorf("upstream payload missing injected web_search tool: %s", gotBody)
			}
			if got := st.mcp.Load(); got != 1 {
				t.Errorf("mcp called %d times, want 1", got)
			}
			if got := st.chat.Load(); got != 1 {
				t.Errorf("kiro chat called %d times, want 1", got)
			}
		})
	}
}

// 跨协议客户端（openai-chat）：server tool 块静默跳过，
// 摘要文本送达。
func TestE2EKiroWebSearchCrossProtocol(t *testing.T) {
	var gotBody string
	gw, st := newWebSearchEnv(t, true, webSearchChatHandler(&gotBody))
	path, body := clientBody("openai-chat", false)
	status, respBody := postChat(t, gw.URL, path, body)
	if status != 200 {
		t.Fatalf("status = %d, body = %s", status, respBody)
	}
	respBody = unescapeJSON(respBody)
	if !strings.Contains(respBody, `<web_search>`) {
		t.Errorf("summary missing from openai-chat response: %s", respBody)
	}
	if strings.Contains(respBody, "server_tool_use") || strings.Contains(respBody, "web_search_tool_result") {
		t.Errorf("server tool blocks must not leak to openai-chat: %s", respBody)
	}
	if got := st.mcp.Load(); got != 1 {
		t.Errorf("mcp called %d times, want 1", got)
	}
}

// 账号开关关闭：不注入声明；上游仍返回 web_search 调用时
// 不拦截（普通 tool_use 透传 + stop tool_use）。
func TestE2EKiroWebSearchDisabled(t *testing.T) {
	var gotBody string
	gw, st := newWebSearchEnv(t, false, webSearchChatHandler(&gotBody))
	status, respBody := postChat(t, gw.URL, "/v1/messages", anthropicChat)
	if status != 200 {
		t.Fatalf("status = %d, body = %s", status, respBody)
	}
	for _, want := range []string{`"type":"tool_use"`, "toolu_ws1", `"stop_reason":"tool_use"`} {
		if !strings.Contains(respBody, want) {
			t.Errorf("passthrough response missing %s: %s", want, respBody)
		}
	}
	if strings.Contains(respBody, "server_tool_use") || strings.Contains(respBody, "<web_search>") {
		t.Errorf("no interception expected: %s", respBody)
	}
	if strings.Contains(gotBody, `"web_search"`) {
		t.Errorf("upstream payload must not contain web_search tool: %s", gotBody)
	}
	if got := st.mcp.Load(); got != 0 {
		t.Errorf("mcp called %d times, want 0", got)
	}
}

// 客户端原生声明 hosted web_search（Path A）：账号开关关闭也拦截；
// 声明本身透传到上游载荷（不重复注入）。
func TestE2EKiroWebSearchDeclared(t *testing.T) {
	var gotBody string
	gw, st := newWebSearchEnv(t, false, webSearchChatHandler(&gotBody))
	req := `{"model":"claude-sonnet-4-5","max_tokens":100,
		"tools":[{"type":"web_search_20250305","name":"web_search"}],
		"messages":[{"role":"user","content":"search go release"}]}`
	status, respBody := postChat(t, gw.URL, "/v1/messages", req)
	if status != 200 {
		t.Fatalf("status = %d, body = %s", status, respBody)
	}
	respBody = unescapeJSON(respBody)
	for _, want := range []string{
		`"type":"server_tool_use"`,
		`"type":"web_search_tool_result"`,
		`<web_search>`,
		`"stop_reason":"end_turn"`,
	} {
		if !strings.Contains(respBody, want) {
			t.Errorf("response missing %s: %s", want, respBody)
		}
	}
	// 声明透传且不重复：载荷中 web_search 工具恰好出现一次
	if n := strings.Count(gotBody, `"web_search"`); n != 1 {
		t.Errorf("upstream payload web_search count = %d, want 1: %s", n, gotBody)
	}
	if got := st.mcp.Load(); got != 1 {
		t.Errorf("mcp called %d times, want 1", got)
	}
}

// MCP 失败：降级普通 tool_use 透传（不合成 server tool 块）。
func TestE2EKiroWebSearchMCPFailure(t *testing.T) {
	var gotBody string
	gw, st := newWebSearchEnv(t, true, webSearchChatHandler(&gotBody))
	st.mcpHandler = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(503)
		_, _ = w.Write([]byte(`{"error":"mcp down"}`))
	}
	status, respBody := postChat(t, gw.URL, "/v1/messages", anthropicChat)
	if status != 200 {
		t.Fatalf("status = %d, body = %s", status, respBody)
	}
	if !strings.Contains(respBody, `"type":"tool_use"`) || !strings.Contains(respBody, "toolu_ws1") {
		t.Errorf("degraded response must pass through raw tool_use: %s", respBody)
	}
	if !strings.Contains(respBody, `"stop_reason":"tool_use"`) {
		t.Errorf("degraded stop_reason must stay tool_use: %s", respBody)
	}
	if strings.Contains(respBody, "server_tool_use") || strings.Contains(respBody, "<web_search>") {
		t.Errorf("no synthesis expected on mcp failure: %s", respBody)
	}
	if got := st.mcp.Load(); got != 1 {
		t.Errorf("mcp called %d times, want 1", got)
	}
}

// 工具声明注入去重：账号开启且客户端已具名声明 -> 不重复注入。
func TestE2EKiroWebSearchInjectNoDuplicate(t *testing.T) {
	var gotBody string
	gw, _ := newWebSearchEnv(t, true, webSearchChatHandler(&gotBody))
	req := fmt.Sprintf(`{"model":"claude-sonnet-4-5","max_tokens":100,
		"tools":[{"type":"web_search_20250305","name":"web_search"}],
		"messages":[{"role":"user","content":"hi"}]}`)
	postChat(t, gw.URL, "/v1/messages", req)
	if n := strings.Count(gotBody, `"web_search"`); n != 1 {
		t.Errorf("upstream payload web_search count = %d, want 1: %s", n, gotBody)
	}
}

// fake_reasoning 账号开关：客户端开 thinking 时上游载荷带思考控制标签；
// 账号关闭时不注入。
func TestE2EKiroFakeReasoning(t *testing.T) {
	var gotBody string
	chat := func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		w.WriteHeader(200)
		_, _ = w.Write(kiroChatStream("OK"))
	}
	gw, _ := newKiroFeatureEnv(t, &account.KiroAccount{
		Source: account.SourceRefreshToken, RefreshToken: "rt-1", FakeReasoning: true,
	}, chat)
	req := `{"model":"claude-sonnet-4-5","max_tokens":100,
		"thinking":{"type":"enabled","budget_tokens":2000},
		"messages":[{"role":"user","content":"hi"}]}`
	status, body := postChat(t, gw.URL, "/v1/messages", req)
	if status != 200 {
		t.Fatalf("status = %d, body = %s", status, body)
	}
	gotBody = unescapeJSON(gotBody)
	if !strings.Contains(gotBody, "<thinking_mode>enabled</thinking_mode>") {
		t.Errorf("upstream payload missing thinking tags: %s", gotBody)
	}
	if !strings.Contains(gotBody, "<max_thinking_length>2000</max_thinking_length>") {
		t.Errorf("upstream payload missing budget tag: %s", gotBody)
	}
	if !strings.Contains(gotBody, "Extended Thinking Mode") {
		t.Errorf("upstream payload missing system addition: %s", gotBody)
	}

	// 账号关闭：不注入
	var gotBody2 string
	gw2, _ := newKiroFeatureEnv(t, &account.KiroAccount{
		Source: account.SourceRefreshToken, RefreshToken: "rt-1",
	}, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody2 = string(b)
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		w.WriteHeader(200)
		_, _ = w.Write(kiroChatStream("OK"))
	})
	postChat(t, gw2.URL, "/v1/messages", req)
	if strings.Contains(unescapeJSON(gotBody2), "thinking_mode") {
		t.Errorf("no injection expected when account off: %s", gotBody2)
	}
}

// fake_reasoning 账号开关解码侧：上游正文携带思考控制标签时，账号开启解析为
// thinking 块下发（客户端见 thinking_delta）；账号关闭时标签原样留在正文。
func TestE2EKiroFakeReasoningDecode(t *testing.T) {
	chat := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		w.WriteHeader(200)
		_, _ = w.Write(kiroChatStream("<thinking>secret thoughts</thinking>Hello"))
	}
	req := `{"model":"claude-sonnet-4-5","max_tokens":100,
		"thinking":{"type":"enabled","budget_tokens":2000},
		"messages":[{"role":"user","content":"hi"}]}`

	gw, _ := newKiroFeatureEnv(t, &account.KiroAccount{
		Source: account.SourceRefreshToken, RefreshToken: "rt-1", FakeReasoning: true,
	}, chat)
	status, body := postChat(t, gw.URL, "/v1/messages", req)
	if status != 200 {
		t.Fatalf("status = %d, body = %s", status, body)
	}
	if !strings.Contains(body, `"type":"thinking"`) || !strings.Contains(body, "secret thoughts") {
		t.Errorf("expected thinking block with parsed content: %s", body)
	}
	if strings.Contains(unescapeJSON(body), "<thinking>") {
		t.Errorf("control tag leaked to client: %s", body)
	}

	gw2, _ := newKiroFeatureEnv(t, &account.KiroAccount{
		Source: account.SourceRefreshToken, RefreshToken: "rt-1",
	}, chat)
	status, body2 := postChat(t, gw2.URL, "/v1/messages", req)
	if status != 200 {
		t.Fatalf("status = %d, body = %s", status, body2)
	}
	if !strings.Contains(unescapeJSON(body2), "<thinking>") {
		t.Errorf("expected raw control tag passthrough when account off: %s", body2)
	}
}

// count_tokens 命中 kiro 账号：按 kiro tokenizer 语义本地估算
// （与 IR 通用估算差异明显：图片块 100 vs 1100 token）。
func TestE2EKiroCountTokens(t *testing.T) {
	gw, st := newKiroFeatureEnv(t, &account.KiroAccount{
		Source: account.SourceRefreshToken, RefreshToken: "rt-1",
	}, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		w.WriteHeader(200)
		_, _ = w.Write(kiroChatStream("OK"))
	})
	req := `{"model":"claude-sonnet-4-5",
		"messages":[{"role":"user","content":[
			{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGk="}},
			{"type":"text","text":"hi"}]}]}`
	status, body := postChat(t, gw.URL, "/v1/messages/count_tokens", req)
	if status != 200 {
		t.Fatalf("status = %d, body = %s", status, body)
	}
	// kiro 语义：4(user 服务)+1(role)+100(image)+0("hi")+3(结尾) = 108 ×1.15 = 124
	// IR 通用估算会是 3+1100+3 = 1106（断言值可区分两条路径）
	var resp struct {
		InputTokens int `json:"input_tokens"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("parse %s: %v", body, err)
	}
	if resp.InputTokens != 124 {
		t.Errorf("input_tokens = %d, want 124 (kiro tokenizer estimate)", resp.InputTokens)
	}
	if got := st.chat.Load(); got != 0 {
		t.Errorf("count_tokens must not hit chat upstream, got %d calls", got)
	}
}

// web_search 全局开关（kiro.web_search_inject）：账号未开也注入工具声明
// 并拦截执行；与账号开关取或。
func TestE2EKiroWebSearchGlobalFlag(t *testing.T) {
	var gotBody string
	gw, st := newKiroFeatureEnvCfg(t, &account.KiroAccount{
		Source: account.SourceRefreshToken, RefreshToken: "rt-1", // 账号级 WebSearch 关
	}, &config.Kiro{WebSearchInject: true}, webSearchChatHandler(&gotBody))
	status, respBody := postChat(t, gw.URL, "/v1/messages", anthropicChat)
	if status != 200 {
		t.Fatalf("status = %d, body = %s", status, respBody)
	}
	respBody = unescapeJSON(respBody)
	if !strings.Contains(respBody, `"type":"server_tool_use"`) || !strings.Contains(respBody, `<web_search>`) {
		t.Errorf("global flag should enable interception: %s", respBody)
	}
	if !strings.Contains(gotBody, `"web_search"`) {
		t.Errorf("upstream payload missing injected web_search tool: %s", gotBody)
	}
	if got := st.mcp.Load(); got != 1 {
		t.Errorf("mcp called %d times, want 1", got)
	}

	// 全局关 + 账号关：不注入（对照组，防全局默认误开）
	var gotBody2 string
	gw2, st2 := newKiroFeatureEnvCfg(t, &account.KiroAccount{
		Source: account.SourceRefreshToken, RefreshToken: "rt-1",
	}, &config.Kiro{}, webSearchChatHandler(&gotBody2))
	postChat(t, gw2.URL, "/v1/messages", anthropicChat)
	if strings.Contains(gotBody2, `"web_search"`) {
		t.Errorf("no injection expected when global and account both off: %s", gotBody2)
	}
	if got := st2.mcp.Load(); got != 0 {
		t.Errorf("mcp called %d times, want 0", got)
	}
}
