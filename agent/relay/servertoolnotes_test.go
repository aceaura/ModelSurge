package relay

import (
	"encoding/json"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
	"github.com/aceaura/ModelSurge/agent/proto"
	_ "github.com/aceaura/ModelSurge/agent/proto/anthropic"
	_ "github.com/aceaura/ModelSurge/agent/proto/gemini"
	_ "github.com/aceaura/ModelSurge/agent/proto/openaichat"
	_ "github.com/aceaura/ModelSurge/agent/proto/openairesponses"
)

// R95：历史里的托管工具块（Anthropic 的 server_tool_use / web_search_tool_result）
// 在外族出站请求里整块消失。两种块型成对出现，一起丢不会撕毁
// tool_use/tool_result 配平，上游不会拒——但模型看不到自己上一轮让网关搜了
// 什么、搜回了哪些页面，只能重新搜一遍。这条损耗必须进诊断注记。

// foreignOutbound 出站三外族（gemini 只入站，anthropic 是原生形态）。
// 按纪律硬编码，不动态取协议清单。
var foreignOutbound = []string{"codex", "openai-chat", "openai-responses"}

// r95HistoryReq 历史里 2 个托管调用 + 1 个结果块。计数刻意不对称：对称夹具
// 看不出「两个计数接反」，那条变异会存活。
func r95HistoryReq() *ir.Request {
	return &ir.Request{
		Model: "m", MaxTokens: 64,
		Messages: []ir.Message{{Role: ir.RoleAssistant, Content: []ir.Block{
			{Type: ir.BlockText, Text: "lead"},
			{Type: ir.BlockServerToolUse, ServerToolUse: &ir.ServerToolUse{
				ID: "srvtoolu_1", Name: "web_search", Input: json.RawMessage(`{"query":"weather in Paris"}`),
			}},
			{Type: ir.BlockServerToolUse, ServerToolUse: &ir.ServerToolUse{
				ID: "srvtoolu_2", Name: "web_search", Input: json.RawMessage(`{"query":"weather in Lyon"}`),
			}},
			{Type: ir.BlockWebSearchToolResult, WebSearchToolResult: &ir.WebSearchToolResult{
				ToolUseID: "srvtoolu_1",
				Results:   []ir.WebSearchResult{{Title: "Paris weather", URL: "https://example.com/p", Snippet: "sunny"}},
			}},
		}}},
	}
}

func TestDiagnoseReportsServerToolHistory(t *testing.T) {
	if proto.ServerToolDropNote(2, 1) == proto.ServerToolDropNote(1, 2) {
		t.Fatal("注记措辞对两个计数对称，测不出计数接反")
	}
	want := proto.ServerToolDropNote(2, 1)
	for _, name := range foreignOutbound {
		t.Run(name, func(t *testing.T) {
			o := proto.MustOutbound(name)
			notes := Diagnose(r95HistoryReq(), name, o.Caps())
			if !slices.Contains(notes, want) {
				t.Fatalf("请求侧未报托管工具块损耗：notes=%q", notes)
			}
			for _, n := range notes {
				for _, leak := range []string{"example.com", "Paris weather", "sunny", "weather in Paris", "weather in Lyon"} {
					if strings.Contains(n, leak) {
						t.Errorf("注记带出了会话内容 %q：%s", leak, n)
					}
				}
			}
			// 报损耗的前提是真的丢了：请求体里不该再出现这两个块的任何痕迹。
			body, err := o.EncodeRequest(r95HistoryReq())
			if err != nil {
				t.Fatalf("EncodeRequest: %v", err)
			}
			for _, gone := range []string{"srvtoolu_1", "srvtoolu_2", "example.com", "weather in Paris", "server_tool_use"} {
				if strings.Contains(string(body), gone) {
					t.Errorf("谎报损耗：请求体里仍有 %q：%s", gone, body)
				}
			}
		})
	}
}

func TestDiagnoseSilentOnServerToolForAnthropic(t *testing.T) {
	o := proto.MustOutbound("anthropic")
	notes := Diagnose(r95HistoryReq(), "anthropic", o.Caps())
	for _, n := range notes {
		if strings.Contains(n, "server-side tool") || strings.Contains(n, "web search result") {
			t.Errorf("anthropic 谎报托管工具块损耗：%s", n)
		}
	}
	body, err := o.EncodeRequest(r95HistoryReq())
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	for _, keep := range []string{"server_tool_use", "web_search_tool_result", "srvtoolu_1"} {
		if !strings.Contains(string(body), keep) {
			t.Errorf("anthropic 出站丢了原生块 %q：%s", keep, body)
		}
	}
}

// 投递通道：非流式响应侧注记要落进 X-ModelSurge-Notes 头，并与请求侧注记合并。
func TestServerToolNotesReachResponseHeader(t *testing.T) {
	resp := &ir.Response{ID: "m1", Model: "m", StopReason: ir.StopEndTurn, Content: []ir.Block{
		{Type: ir.BlockText, Text: "lead"},
		{Type: ir.BlockServerToolUse, ServerToolUse: &ir.ServerToolUse{ID: "srvtoolu_1", Name: "web_search"}},
		{Type: ir.BlockWebSearchToolResult, WebSearchToolResult: &ir.WebSearchToolResult{ToolUseID: "srvtoolu_1"}},
	}}
	w := httptest.NewRecorder()
	w.Header().Set("X-ModelSurge-Notes", "request-side note")
	writeResponse(w, proto.MustInbound("openai-responses"), nil, resp, false, nil)
	h := w.Header().Get("X-ModelSurge-Notes")
	if !strings.Contains(h, "request-side note") {
		t.Errorf("请求侧注记被覆盖：%q", h)
	}
	if !strings.Contains(h, proto.ServerToolDropNote(1, 1)) {
		t.Errorf("托管工具块损耗未进响应头：%q", h)
	}
}

// 流式投递：头已发出，注记走 SSE 注释帧。
func TestServerToolNotesReachSSEFrames(t *testing.T) {
	frames := proto.SSENoteFrames([]string{proto.ServerToolDropNote(1, 1)})
	if len(frames) != 1 {
		t.Fatalf("注记帧数应为 1，实得 %d", len(frames))
	}
	s := string(frames[0])
	if !strings.HasPrefix(s, ": modelsurge-note: ") {
		t.Errorf("不是 SSE 注释帧：%q", s)
	}
	if !strings.Contains(s, "server-side tool call(s)") {
		t.Errorf("注释帧丢了托管工具块措辞：%q", s)
	}
	if strings.ContainsAny(s, "\r") || strings.Count(s, "\n") != 2 {
		t.Errorf("注释帧必须单行且以空行收尾：%q", s)
	}
}
