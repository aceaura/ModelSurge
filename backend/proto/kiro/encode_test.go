package kiro

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"relayd/backend/ir"
)

// payloadOf 编码辅助：BuildPayload + 断言不报错。
func payloadOf(t *testing.T, req *ir.Request, modelID string) map[string]any {
	t.Helper()
	p, err := BuildPayload(req, modelID)
	if err != nil {
		t.Fatalf("BuildPayload: %v", err)
	}
	return p
}

func convState(t *testing.T, p map[string]any) map[string]any {
	t.Helper()
	cs, ok := p["conversationState"].(map[string]any)
	if !ok {
		t.Fatalf("payload missing conversationState: %v", p)
	}
	return cs
}

func historyOf(t *testing.T, p map[string]any) []any {
	t.Helper()
	h, _ := convState(t, p)["history"].([]any)
	return h
}

func userInput(t *testing.T, p map[string]any) map[string]any {
	t.Helper()
	cur, ok := convState(t, p)["currentMessage"].(map[string]any)
	if !ok {
		t.Fatalf("missing currentMessage: %v", p)
	}
	u, ok := cur["userInputMessage"].(map[string]any)
	if !ok {
		t.Fatalf("missing userInputMessage: %v", cur)
	}
	return u
}

func textReq(msgs ...ir.Message) *ir.Request {
	return &ir.Request{Model: "claude-sonnet-4", Messages: msgs}
}

// 纯文本对话：user/assistant/user -> 首两条 history + 末条 currentMessage。
func TestBuildPayload_PlainConversation(t *testing.T) {
	p := payloadOf(t, textReq(
		ir.Message{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "Read the file test.py"}}},
		ir.Message{Role: ir.RoleAssistant, Content: []ir.Block{{Type: ir.BlockText, Text: "I see a hello function."}}},
		ir.Message{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "Summarize what we did"}}},
	), "claude-sonnet-4")

	if got := userInput(t, p)["content"]; got != "Summarize what we did" {
		t.Errorf("current content = %v", got)
	}
	if got := userInput(t, p)["modelId"]; got != "claude-sonnet-4" {
		t.Errorf("modelId = %v", got)
	}
	h := historyOf(t, p)
	if len(h) != 2 {
		t.Fatalf("history len = %d, want 2", len(h))
	}
	if u, ok := h[0].(map[string]any)["userInputMessage"].(map[string]any); !ok || u["content"] != "Read the file test.py" {
		t.Errorf("history[0] = %v", h[0])
	}
	if a, ok := h[1].(map[string]any)["assistantResponseMessage"].(map[string]any); !ok || a["content"] != "I see a hello function." {
		t.Errorf("history[1] = %v", h[1])
	}
	if _, has := userInput(t, p)["userInputMessageContext"]; has {
		t.Errorf("no context expected for plain text")
	}
}

// 单条 user：无 history，content 即当前消息。
func TestBuildPayload_SingleUserMessage(t *testing.T) {
	p := payloadOf(t, textReq(
		ir.Message{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}},
	), "claude-sonnet-4")
	if len(historyOf(t, p)) != 0 {
		t.Errorf("history should be empty")
	}
	if got := userInput(t, p)["content"]; got != "hi" {
		t.Errorf("content = %v", got)
	}
}

// 系统块并入历史首条 user 文本（Kiro 无顶层 system）。
func TestBuildPayload_SystemMergedIntoFirstUser(t *testing.T) {
	req := textReq(
		ir.Message{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hello"}}},
	)
	req.System = []ir.Block{{Type: ir.BlockText, Text: "You are a helpful assistant."}}
	p := payloadOf(t, req, "claude-sonnet-4")
	if len(historyOf(t, p)) != 0 {
		t.Fatalf("system should merge into current when single message")
	}
	content := userInput(t, p)["content"].(string)
	if !strings.HasPrefix(content, "You are a helpful assistant.") || !strings.Contains(content, "hello") {
		t.Errorf("system+content merge = %q", content)
	}
}

// 计费归因行剥离（保 prompt cache 前缀纯净）。
func TestBuildPayload_BillingHeaderStripped(t *testing.T) {
	req := textReq(ir.Message{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}})
	req.System = []ir.Block{{Type: ir.BlockText, Text: "x-anthropic-billing-header: extra-1\nReal system prompt."}}
	p := payloadOf(t, req, "claude-sonnet-4")
	content := userInput(t, p)["content"].(string)
	if strings.Contains(content, "billing-header") {
		t.Errorf("billing header not stripped: %q", content)
	}
	if !strings.Contains(content, "Real system prompt.") {
		t.Errorf("rest of system prompt lost: %q", content)
	}
}

// 工具定义 + 调用链：tools 进 context，toolUses 保留在 assistant history，
// toolResults 保留在当前 user 的 context。
func TestBuildPayload_ToolChain(t *testing.T) {
	req := &ir.Request{
		Model: "claude-sonnet-4",
		Tools: []ir.Tool{{
			Name:        "read_file",
			Description: "A test tool",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
		}},
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "Call a tool"}}},
			{Role: ir.RoleAssistant, Content: []ir.Block{{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{
				ID: "call_123", Name: "read_file", Input: json.RawMessage(`{"path":"test.py"}`),
			}}}},
			{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockToolResult, ToolResult: &ir.ToolResult{
				ToolUseID: "call_123",
				Content:   []ir.Block{{Type: ir.BlockText, Text: "def hello():\n    print('world')"}},
			}}}},
		},
	}
	p := payloadOf(t, req, "claude-sonnet-4")

	ctx, ok := userInput(t, p)["userInputMessageContext"].(map[string]any)
	if !ok {
		t.Fatalf("missing userInputMessageContext")
	}
	tools, _ := ctx["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools len = %d, want 1", len(tools))
	}
	spec := tools[0].(map[string]any)["toolSpecification"].(map[string]any)
	if spec["name"] != "read_file" {
		t.Errorf("tool name = %v", spec["name"])
	}
	schema := spec["inputSchema"].(map[string]any)["json"].(map[string]any)
	if _, has := schema["required"]; !has {
		t.Errorf("required array missing (sanitizer should keep non-empty)")
	}

	h := historyOf(t, p)
	assistant, ok := h[1].(map[string]any)["assistantResponseMessage"].(map[string]any)
	if !ok {
		t.Fatalf("history[1] not assistant: %v", h[1])
	}
	uses, _ := assistant["toolUses"].([]any)
	if len(uses) != 1 {
		t.Fatalf("toolUses len = %d, want 1 (preserved when tools defined)", len(uses))
	}
	results, _ := ctx["toolResults"].([]any)
	if len(results) != 1 {
		t.Fatalf("current toolResults len = %d, want 1", len(results))
	}
	if results[0].(map[string]any)["toolUseId"] != "call_123" {
		t.Errorf("toolUseId = %v", results[0].(map[string]any)["toolUseId"])
	}
}

// Issue #20（OpenCode compaction）：历史带 toolUses/toolResults 但无工具定义
// -> 全部转文本表示，载荷不再含 tool 字段，数据不丢。
func TestBuildPayload_CompactionWithoutTools(t *testing.T) {
	req := &ir.Request{
		Model: "claude-sonnet-4",
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "Read the file test.py"}}},
			{Role: ir.RoleAssistant, Content: []ir.Block{{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{
				ID: "tooluse_abc123", Name: "read_file", Input: json.RawMessage(`{"path":"test.py"}`),
			}}}},
			{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockToolResult, ToolResult: &ir.ToolResult{
				ToolUseID: "tooluse_abc123",
				Content:   []ir.Block{{Type: ir.BlockText, Text: "IMPORTANT_DATA_12345"}},
			}}}},
			{Role: ir.RoleAssistant, Content: []ir.Block{{Type: ir.BlockText, Text: "I see the file contains a hello function."}}},
			{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "Summarize what we did"}}},
		},
	}
	p := payloadOf(t, req, "claude-sonnet-4")

	var foundToolText, foundData bool
	for _, e := range historyOf(t, p) {
		m := e.(map[string]any)
		if a, ok := m["assistantResponseMessage"].(map[string]any); ok {
			if _, has := a["toolUses"]; has {
				t.Errorf("toolUses must be stripped: %v", a)
			}
			if strings.Contains(a["content"].(string), "[Tool: read_file") {
				foundToolText = true
			}
		}
		if u, ok := m["userInputMessage"].(map[string]any); ok {
			if c, ok := u["userInputMessageContext"].(map[string]any); ok {
				if _, has := c["toolResults"]; has {
					t.Errorf("toolResults must be stripped: %v", c)
				}
			}
			if strings.Contains(u["content"].(string), "IMPORTANT_DATA_12345") {
				foundData = true
			}
		}
	}
	if cur, ok := userInput(t, p)["userInputMessageContext"].(map[string]any); ok {
		if _, has := cur["toolResults"]; has {
			t.Errorf("current toolResults must be stripped")
		}
	}
	if !foundToolText {
		t.Errorf("tool call text representation missing")
	}
	if !foundData {
		t.Errorf("tool result content lost (IMPORTANT_DATA_12345)")
	}
}

// 空工具列表同样触发剥离（tools=nil 与 tools=[] 同义）。
func TestBuildPayload_EmptyToolsStrips(t *testing.T) {
	req := &ir.Request{
		Model: "claude-sonnet-4",
		Tools: []ir.Tool{},
		Messages: []ir.Message{
			{Role: ir.RoleAssistant, Content: []ir.Block{{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{
				ID: "call_123", Name: "some_tool", Input: json.RawMessage(`{}`),
			}}}},
			{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "next"}}},
		},
	}
	p := payloadOf(t, req, "claude-sonnet-4")
	for _, e := range historyOf(t, p) {
		if a, ok := e.(map[string]any)["assistantResponseMessage"].(map[string]any); ok {
			if _, has := a["toolUses"]; has {
				t.Errorf("toolUses must be stripped for empty tools list")
			}
		}
	}
	if _, has := userInput(t, p)["userInputMessageContext"]; has {
		t.Errorf("no context expected")
	}
}

// 图片：当前消息图片进 images（format + source.bytes，剥 data URL 前缀）。
func TestBuildPayload_ImagesInCurrentMessage(t *testing.T) {
	req := textReq(ir.Message{Role: ir.RoleUser, Content: []ir.Block{
		{Type: ir.BlockText, Text: "what is this"},
		{Type: ir.BlockImage, Image: &ir.Image{MediaType: "image/png", Data: "data:image/png;base64,aGVsbG8="}},
	}})
	p := payloadOf(t, req, "claude-sonnet-4")
	imgs, _ := userInput(t, p)["images"].([]any)
	if len(imgs) != 1 {
		t.Fatalf("images len = %d, want 1", len(imgs))
	}
	img := imgs[0].(map[string]any)
	if img["format"] != "png" {
		t.Errorf("format = %v", img["format"])
	}
	if img["source"].(map[string]any)["bytes"] != "aGVsbG8=" {
		t.Errorf("bytes = %v", img["source"])
	}
}

// URL 形态图片 Kiro 不支持，跳过不报错。
func TestBuildPayload_URLImageSkipped(t *testing.T) {
	req := textReq(ir.Message{Role: ir.RoleUser, Content: []ir.Block{
		{Type: ir.BlockImage, Image: &ir.Image{MediaType: "image/png", URL: "https://example.com/x.png"}},
	}})
	p := payloadOf(t, req, "claude-sonnet-4")
	if _, has := userInput(t, p)["images"]; has {
		t.Errorf("URL images must be skipped")
	}
	if got := userInput(t, p)["content"]; got != "(empty placeholder)" {
		t.Errorf("empty text should placeholder, got %v", got)
	}
}

// tool_result 内嵌图片并入同消息 images：convertToolResult 只承载文本，
// 图片走 userInputMessage.images（KiroaaS converters_anthropic.py:169-208 对齐）。
func TestBuildPayload_ToolResultEmbeddedImages(t *testing.T) {
	// 当前消息：assistant toolUse + user toolResult(文本+图片) —— 图片进当前 userInput
	// （须声明 tools，否则 toolResults 会被无声明剥离逻辑转为文本摘要）
	req := textReq(
		ir.Message{Role: ir.RoleAssistant, Content: []ir.Block{{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{
			ID: "call_1", Name: "shot", Input: json.RawMessage(`{}`),
		}}}},
		ir.Message{Role: ir.RoleUser, Content: []ir.Block{
			{Type: ir.BlockToolResult, ToolResult: &ir.ToolResult{ToolUseID: "call_1", Content: []ir.Block{
				{Type: ir.BlockText, Text: "screenshot"},
				{Type: ir.BlockImage, Image: &ir.Image{MediaType: "image/png", Data: "data:image/png;base64,c2hvdA=="}},
			}}},
		}},
	)
	req.Tools = []ir.Tool{{Name: "shot", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	p := payloadOf(t, req, "claude-sonnet-4")
	imgs, _ := userInput(t, p)["images"].([]any)
	if len(imgs) != 1 {
		t.Fatalf("current images len = %d, want 1", len(imgs))
	}
	if img := imgs[0].(map[string]any); img["format"] != "png" || img["source"].(map[string]any)["bytes"] != "c2hvdA==" {
		t.Errorf("image = %v", img)
	}
	// toolResults 条目仍为纯文本
	ctx := userInput(t, p)["userInputMessageContext"].(map[string]any)
	trs := ctx["toolResults"].([]any)
	if len(trs) != 1 {
		t.Fatalf("toolResults len = %d, want 1", len(trs))
	}
	content := trs[0].(map[string]any)["content"].([]any)
	if len(content) != 1 || content[0].(map[string]any)["text"] != "screenshot" {
		t.Errorf("toolResult content = %v, want text-only", content)
	}

	// 历史消息：tool_result 带图并入该历史 user 条目的 images
	hist := textReq(
		ir.Message{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "first"}}},
		ir.Message{Role: ir.RoleAssistant, Content: []ir.Block{{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{
			ID: "call_2", Name: "shot", Input: json.RawMessage(`{}`),
		}}}},
		ir.Message{Role: ir.RoleUser, Content: []ir.Block{
			{Type: ir.BlockToolResult, ToolResult: &ir.ToolResult{ToolUseID: "call_2", Content: []ir.Block{
				{Type: ir.BlockImage, Image: &ir.Image{MediaType: "image/jpeg", Data: "data:image/jpeg;base64,aGlzdA=="}},
			}}},
		}},
		// 中间隔一条 assistant：否则相邻 user 被 mergeAdjacent 并入当前消息
		ir.Message{Role: ir.RoleAssistant, Content: []ir.Block{{Type: ir.BlockText, Text: "got it"}}},
		ir.Message{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "next"}}},
	)
	hist.Tools = []ir.Tool{{Name: "shot", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	p2 := payloadOf(t, hist, "claude-sonnet-4")
	found := false
	for _, e := range historyOf(t, p2) {
		ui, ok := e.(map[string]any)["userInputMessage"].(map[string]any)
		if !ok {
			continue
		}
		if imgs, ok := ui["images"].([]any); ok && len(imgs) == 1 {
			if img := imgs[0].(map[string]any); img["format"] == "jpeg" && img["source"].(map[string]any)["bytes"] == "aGlzdA==" {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("history user entry missing embedded tool_result image")
	}
}

// 无图 tool_result 回归：不出 images 键。
func TestBuildPayload_ToolResultNoImages(t *testing.T) {
	req := textReq(
		ir.Message{Role: ir.RoleAssistant, Content: []ir.Block{{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{
			ID: "call_3", Name: "t", Input: json.RawMessage(`{}`),
		}}}},
		ir.Message{Role: ir.RoleUser, Content: []ir.Block{
			{Type: ir.BlockToolResult, ToolResult: &ir.ToolResult{ToolUseID: "call_3", Content: []ir.Block{
				{Type: ir.BlockText, Text: "plain"},
			}}},
		}},
	)
	req.Tools = []ir.Tool{{Name: "t", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	p := payloadOf(t, req, "claude-sonnet-4")
	if _, has := userInput(t, p)["images"]; has {
		t.Errorf("images must be absent for text-only tool_result")
	}
}

// 消息规整链：user 打头的 assistant 起始、连续 user 合并、连续同角色插合成。
func TestBuildPayload_RoleShaping(t *testing.T) {
	// assistant 打头 -> 补合成 user
	p := payloadOf(t, textReq(
		ir.Message{Role: ir.RoleAssistant, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}},
		ir.Message{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hello"}}},
	), "claude-sonnet-4")
	h := historyOf(t, p)
	if len(h) < 2 {
		t.Fatalf("history len = %d, want >= 2 (synthetic user prepended)", len(h))
	}
	if _, ok := h[0].(map[string]any)["userInputMessage"]; !ok {
		t.Errorf("history[0] must be user after ensureFirstIsUser")
	}

	// 连续 user -> 合并进单条（中间不再插 assistant，merge 在 ensureAlternating 前）
	p2 := payloadOf(t, textReq(
		ir.Message{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "a"}}},
		ir.Message{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "b"}}},
	), "claude-sonnet-4")
	h2 := historyOf(t, p2)
	if len(h2) != 0 {
		t.Fatalf("adjacent users merge into current, history len = %d", len(h2))
	}
	if got := userInput(t, p2)["content"]; got != "a\nb" {
		t.Errorf("merged content = %v", got)
	}

	// 未知角色（跨协议残留）归一为 user，载荷可用且不报错
	p3 := payloadOf(t, textReq(
		ir.Message{Role: ir.Role("developer"), Content: []ir.Block{{Type: ir.BlockText, Text: "instr"}}},
		ir.Message{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "go"}}},
	), "claude-sonnet-4")
	h3 := historyOf(t, p3)
	if len(h3) != 0 {
		t.Errorf("unknown-role message merged into current user, history len = %d", len(h3))
	}
	if got := userInput(t, p3)["content"]; got != "instr\ngo" {
		t.Errorf("unknown role normalized content = %v", got)
	}
}

// 孤儿 toolUses（assistant toolUse 后无配对 toolResult）合成占位结果。
func TestBuildPayload_RepairUnpairedToolUses(t *testing.T) {
	req := &ir.Request{
		Model: "claude-sonnet-4",
		Tools: []ir.Tool{{Name: "t1", Description: "d", InputSchema: json.RawMessage(`{}`)}},
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "go"}}},
			{Role: ir.RoleAssistant, Content: []ir.Block{{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{
				ID: "call_x", Name: "t1", Input: json.RawMessage(`{}`),
			}}}},
			{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "then"}}},
		},
	}
	p := payloadOf(t, req, "claude-sonnet-4")
	results, _ := userInput(t, p)["userInputMessageContext"].(map[string]any)["toolResults"].([]any)
	if len(results) != 1 {
		t.Fatalf("synthetic toolResults len = %d, want 1", len(results))
	}
	if results[0].(map[string]any)["toolUseId"] != "call_x" {
		t.Errorf("toolUseId = %v", results[0])
	}
}

// 超长工具名 -> 别名 t_{hash}_{suffix}，双向可还原。
func TestBuildPayload_LongToolNameAlias(t *testing.T) {
	longName := strings.Repeat("mcp__server__", 10) + "very_long_tool_name"
	req := &ir.Request{
		Model: "claude-sonnet-4",
		Tools: []ir.Tool{{Name: longName, Description: "d", InputSchema: json.RawMessage(`{}`)}},
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "go"}}},
			{Role: ir.RoleAssistant, Content: []ir.Block{{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{
				ID: "call_1", Name: longName, Input: json.RawMessage(`{}`),
			}}}},
			{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockToolResult, ToolResult: &ir.ToolResult{
				ToolUseID: "call_1", Content: []ir.Block{{Type: ir.BlockText, Text: "ok"}},
			}}}},
		},
	}
	ResetToolAliases()
	t.Cleanup(ResetToolAliases)
	p := payloadOf(t, req, "claude-sonnet-4")

	ctx, _ := userInput(t, p)["userInputMessageContext"].(map[string]any)
	spec := ctx["tools"].([]any)[0].(map[string]any)["toolSpecification"].(map[string]any)
	alias := spec["name"].(string)
	if alias == longName {
		t.Fatalf("long name should be aliased")
	}
	if len(alias) > MaxKiroToolNameLength {
		t.Errorf("alias %q exceeds %d chars", alias, MaxKiroToolNameLength)
	}
	if got := OriginalForToolName(alias); got != longName {
		t.Errorf("alias not reversible: %q -> %q", alias, got)
	}
	// 历史中的 toolUse 名称也用别名
	h := historyOf(t, p)
	uses := h[1].(map[string]any)["assistantResponseMessage"].(map[string]any)["toolUses"].([]any)
	if uses[0].(map[string]any)["name"] != alias {
		t.Errorf("history toolUse name not aliased")
	}
}

// schema 清理：空 required 与 additionalProperties 递归剔除。
func TestSanitizeJSONSchema(t *testing.T) {
	in := json.RawMessage(`{"type":"object","properties":{"a":{"type":"object","required":[],"additionalProperties":{}},"b":{"type":"array","items":{"required":["x"]}}},"required":[]}`)
	out := sanitizeJSONSchema(in)
	if _, has := out["required"]; has {
		t.Errorf("top-level empty required not removed")
	}
	props := out["properties"].(map[string]any)
	a := props["a"].(map[string]any)
	if _, has := a["required"]; has {
		t.Errorf("nested empty required not removed")
	}
	if _, has := a["additionalProperties"]; has {
		t.Errorf("additionalProperties not removed")
	}
	b := props["b"].(map[string]any)
	items := b["items"].(map[string]any)
	if _, has := items["required"]; !has {
		t.Errorf("non-empty required must be preserved")
	}
}

// thinking 注入（fake_reasoning）：控制标签在当前 user 内容前置。
func TestBuildPayload_FakeReasoningInjection(t *testing.T) {
	old := opt
	t.Cleanup(func() { opt = old })
	opt = Options{FakeReasoning: true, FakeReasoningMaxTokens: 4000, FakeReasoningBudgetCap: 10000}

	req := textReq(ir.Message{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}})
	req.Thinking = &ir.ThinkingConfig{Enabled: true, BudgetTokens: 5000}
	p := payloadOf(t, req, "claude-sonnet-4")
	content := userInput(t, p)["content"].(string)
	if !strings.HasPrefix(content, "<thinking_mode>enabled</thinking_mode>") {
		t.Errorf("thinking_mode tag missing: %q", content[:60])
	}
	if !strings.Contains(content, "<max_thinking_length>5000</max_thinking_length>") {
		t.Errorf("budget tag missing (within cap)")
	}
	if !strings.Contains(content, "<thinking_instruction>") {
		t.Errorf("thinking_instruction tag missing")
	}
	if !strings.Contains(content, "\n\nhi") {
		t.Errorf("original content must follow tags")
	}
}

// 账号级 fake_reasoning（Metadata 标志）：全局关闭时账号开关单独生效；
// 预算超上限时按 FakeReasoningBudgetCap 截断。
func TestBuildPayload_FakeReasoningAccountFlag(t *testing.T) {
	old := opt
	t.Cleanup(func() { opt = old })
	opt = Options{FakeReasoningMaxTokens: 4000, FakeReasoningBudgetCap: 10000} // 全局关

	req := textReq(ir.Message{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}})
	req.Thinking = &ir.ThinkingConfig{Enabled: true, BudgetTokens: 50000}
	req.Metadata = map[string]string{"kiro_fake_reasoning": "1"}
	p := payloadOf(t, req, "claude-sonnet-4")
	content := userInput(t, p)["content"].(string)
	if !strings.HasPrefix(content, "<thinking_mode>enabled</thinking_mode>") {
		t.Errorf("account flag must enable injection: %q", content[:60])
	}
	if !strings.Contains(content, "<max_thinking_length>10000</max_thinking_length>") {
		t.Errorf("budget must be capped at 10000")
	}
	// 系统提示合法化附加同随账号开关生效
	joined := joinedSystem(t, p)
	if !strings.Contains(joined, "Extended Thinking Mode") {
		t.Errorf("system addition missing: %q", joined)
	}

	// 无标志且全局关：不注入
	req2 := textReq(ir.Message{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}})
	req2.Thinking = &ir.ThinkingConfig{Enabled: true}
	p2 := payloadOf(t, req2, "claude-sonnet-4")
	if c := userInput(t, p2)["content"].(string); strings.Contains(c, "thinking_mode") {
		t.Errorf("no injection expected without flag: %q", c)
	}
}

// joinedSystem 从载荷取合并后的系统提示（历史首条 user 前缀或当前内容前缀）。
func joinedSystem(t *testing.T, p map[string]any) string {
	t.Helper()
	h := historyOf(t, p)
	if len(h) > 0 {
		if u, ok := h[0].(map[string]any)["userInputMessage"].(map[string]any); ok {
			return fmt.Sprint(u["content"])
		}
	}
	return fmt.Sprint(userInput(t, p)["content"])
}

// toolChoice 指令注入系统提示。
func TestToolChoiceDirective(t *testing.T) {
	if ToolChoiceDirective(nil) != "" {
		t.Errorf("nil toolChoice should be empty")
	}
	any := ToolChoiceDirective(&ir.ToolChoice{Mode: ir.ChoiceAny})
	if !strings.Contains(any, "MUST call") {
		t.Errorf("any directive = %q", any)
	}
	none := ToolChoiceDirective(&ir.ToolChoice{Mode: ir.ChoiceNone})
	if !strings.Contains(none, "NOT call any tool") {
		t.Errorf("none directive = %q", none)
	}
	named := ToolChoiceDirective(&ir.ToolChoice{Mode: ir.ChoiceTool, ToolName: "read_file"})
	if !strings.Contains(named, "'read_file'") {
		t.Errorf("named directive = %q", named)
	}
	if ToolChoiceDirective(&ir.ToolChoice{Mode: ir.ChoiceAuto}) != "" {
		t.Errorf("auto directive should be empty")
	}
}

// effort 片段：模型有通道时按枚举夹紧进 additionalModelRequestFields。
func TestBuildPayload_EffortFragment(t *testing.T) {
	req := textReq(ir.Message{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}})
	req.Thinking = &ir.ThinkingConfig{Enabled: true, Effort: "high"}
	p := payloadOf(t, req, "claude-sonnet-4.6")
	frag, ok := p["additionalModelRequestFields"].(map[string]any)
	if !ok {
		t.Fatalf("missing additionalModelRequestFields for claude-sonnet-4.6")
	}
	oc, ok := frag["output_config"].(map[string]any)
	if !ok || oc["effort"] != "high" {
		t.Errorf("output_config.effort = %v", frag)
	}

	// budget -> 档位映射
	req2 := textReq(ir.Message{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}})
	req2.Thinking = &ir.ThinkingConfig{Enabled: true, BudgetTokens: 8000}
	p2 := payloadOf(t, req2, "claude-sonnet-4.6")
	oc2 := p2["additionalModelRequestFields"].(map[string]any)["output_config"].(map[string]any)
	if oc2["effort"] != "high" {
		t.Errorf("budget 8000 should map to high, got %v", oc2["effort"])
	}

	// 无通道模型省略
	p3 := payloadOf(t, req, "claude-sonnet-4-20250514")
	if _, has := p3["additionalModelRequestFields"]; has {
		t.Errorf("model without effort channel should omit field")
	}
}

// thinking 关闭时请求 effort="none"（NATIVE_EFFORT_NONE_ON_DISABLED）：
// gpt 系有 none 档 -> 原生关闭推理；claude 系无 none 档 -> 省略。
func TestBuildPayload_EffortNoneOnDisabled(t *testing.T) {
	req := textReq(ir.Message{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}})
	req.Thinking = &ir.ThinkingConfig{Enabled: false, Effort: "high"}
	p := payloadOf(t, req, "gpt-5.6-sol")
	frag, ok := p["additionalModelRequestFields"].(map[string]any)
	if !ok {
		t.Fatalf("gpt model with thinking disabled must send effort none")
	}
	r, ok := frag["reasoning"].(map[string]any)
	if !ok || r["effort"] != "none" {
		t.Errorf("reasoning.effort = %v, want none", frag)
	}

	// 请求无 thinking 字段：同 none 处理
	req2 := textReq(ir.Message{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}})
	p2 := payloadOf(t, req2, "gpt-5.5")
	r2 := p2["additionalModelRequestFields"].(map[string]any)["reasoning"].(map[string]any)
	if r2["effort"] != "none" {
		t.Errorf("gpt-5.5 without thinking config: reasoning.effort = %v, want none", r2)
	}

	// claude 系无 none 档：省略
	req3 := textReq(ir.Message{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}})
	req3.Thinking = &ir.ThinkingConfig{Enabled: false}
	p3 := payloadOf(t, req3, "claude-sonnet-4.6")
	if _, has := p3["additionalModelRequestFields"]; has {
		t.Errorf("claude model with thinking disabled should omit field")
	}
}

// profileArn 经 Metadata 注入顶层字段。
func TestBuildPayload_ProfileArn(t *testing.T) {
	req := textReq(ir.Message{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}})
	req.Metadata = map[string]string{"kiro_profile_arn": "arn:aws:codewhisperer:us-east-1:1:profile/X"}
	p := payloadOf(t, req, "claude-sonnet-4")
	if got := p["profileArn"]; got != "arn:aws:codewhisperer:us-east-1:1:profile/X" {
		t.Errorf("profileArn = %v", got)
	}
}

// ConversationID 稳定：同一会话前后两轮（追加消息）ID 不变；首条不同则变。
func TestConversationID_Stable(t *testing.T) {
	base := []ir.Message{
		{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "start the session"}}},
		{Role: ir.RoleAssistant, Content: []ir.Block{{Type: ir.BlockText, Text: "ok"}}},
		{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "q1"}}},
		{Role: ir.RoleAssistant, Content: []ir.Block{{Type: ir.BlockText, Text: "a1"}}},
	}
	withTail := func(tail string) *ir.Request {
		msgs := append(append([]ir.Message{}, base...),
			ir.Message{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: tail}}})
		return textReq(msgs...)
	}
	if ConversationID(withTail("next")) != ConversationID(withTail("another")) {
		t.Errorf("conversationId should be stable across turns of same session")
	}
	other := []ir.Message{
		{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "another session entirely"}}},
		{Role: ir.RoleAssistant, Content: []ir.Block{{Type: ir.BlockText, Text: "ok"}}},
		{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "q1"}}},
		{Role: ir.RoleAssistant, Content: []ir.Block{{Type: ir.BlockText, Text: "a1"}}},
		{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "different start"}}},
	}
	if ConversationID(withTail("next")) == ConversationID(textReq(other...)) {
		t.Errorf("different session should produce different id")
	}
}

// hosted 工具归一：客户端只给 Hosted 声明 -> 补全 Kiro 具名声明；不支持的丢弃。
func TestBuildPayload_HostedTools(t *testing.T) {
	old := opt
	t.Cleanup(func() { opt = old })
	opt = DefaultOptions

	req := textReq(ir.Message{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "search this"}}})
	req.Tools = []ir.Tool{
		{Hosted: ir.HostedWebSearch},
		{Hosted: ir.HostedCodeExecution},
	}
	p := payloadOf(t, req, "claude-sonnet-4")
	ctx, _ := userInput(t, p)["userInputMessageContext"].(map[string]any)
	tools, _ := ctx["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools len = %d, want 1 (web_search kept, code_execution dropped)", len(tools))
	}
	spec := tools[0].(map[string]any)["toolSpecification"].(map[string]any)
	if spec["name"] != "web_search" {
		t.Errorf("hosted tool name = %v", spec["name"])
	}
	schema := spec["inputSchema"].(map[string]any)["json"].(map[string]any)
	props := schema["properties"].(map[string]any)
	if _, has := props["query"]; !has {
		t.Errorf("web_search schema must declare query param")
	}
}

// DecodeRequest 往返：编码产物可解回 IR，模型/工具/消息/图片形态对得上。
func TestDecodeRequest_RoundTrip(t *testing.T) {
	ResetToolAliases()
	t.Cleanup(ResetToolAliases)
	req := &ir.Request{
		Model: "claude-sonnet-4",
		Tools: []ir.Tool{{Name: "read_file", Description: "d", InputSchema: json.RawMessage(`{"type":"object"}`)}},
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "go"}}},
			{Role: ir.RoleAssistant, Content: []ir.Block{{Type: ir.BlockText, Text: "calling"}, {Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{
				ID: "call_1", Name: "read_file", Input: json.RawMessage(`{"path":"x"}`),
			}}}},
			{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockToolResult, ToolResult: &ir.ToolResult{
				ToolUseID: "call_1", Content: []ir.Block{{Type: ir.BlockText, Text: "data"}},
			}}}},
		},
	}
	body, err := Codec{}.EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	got, err := Codec{}.DecodeRequest(body)
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if got.Model != "claude-sonnet-4" {
		t.Errorf("model = %q", got.Model)
	}
	if len(got.Tools) != 1 || got.Tools[0].Name != "read_file" {
		t.Errorf("tools = %+v", got.Tools)
	}
	if len(got.Messages) != 3 {
		t.Fatalf("messages len = %d, want 3", len(got.Messages))
	}
	if got.Messages[0].Role != ir.RoleUser || got.Messages[0].Text() != "go" {
		t.Errorf("messages[0] = %+v", got.Messages[0])
	}
	tu := got.Messages[1].Content[1].ToolUse
	if tu == nil || tu.ID != "call_1" || tu.Name != "read_file" {
		t.Errorf("messages[1] toolUse = %+v", tu)
	}
	tr := got.Messages[2].Content[len(got.Messages[2].Content)-1].ToolResult
	if tr == nil || tr.ToolUseID != "call_1" || tr.Content[0].Text != "data" {
		t.Errorf("messages[2] toolResult = %+v", tr)
	}
}

// 载荷上限守卫：超限按对裁剪最老历史（至少留 2 条），裁后体积达标。
func TestBuildPayload_SizeGuard(t *testing.T) {
	big := strings.Repeat("x", 110*1024)
	msgs := []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: big}}}}
	for i := 0; i < 4; i++ {
		msgs = append(msgs,
			ir.Message{Role: ir.RoleAssistant, Content: []ir.Block{{Type: ir.BlockText, Text: big}}},
			ir.Message{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: big}}},
		)
	}
	p, err := BuildPayload(textReq(msgs...), "claude-sonnet-4")
	if err != nil {
		t.Fatalf("oversize should trim, not fail: %v", err)
	}
	raw, _ := json.Marshal(p)
	if len(raw) > MaxPayloadBytes {
		t.Errorf("payload after trim = %d bytes, want <= %d", len(raw), MaxPayloadBytes)
	}
	h := historyOf(t, p)
	if len(h) < 2 {
		t.Errorf("trim should keep at least 2 history entries, got %d", len(h))
	}
	if _, ok := h[0].(map[string]any)["userInputMessage"]; !ok {
		t.Errorf("history must start at a user entry after alignment")
	}
}

// web_search 注入（Path B）：Metadata 标志驱动；已声明时不重复注入。
func TestBuildPayload_WebSearchInject(t *testing.T) {
	req := textReq(ir.Message{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}})
	req.Metadata = map[string]string{"kiro_web_search": "1"}
	p := payloadOf(t, req, "claude-sonnet-4")
	ctx, _ := userInput(t, p)["userInputMessageContext"].(map[string]any)
	tools, _ := ctx["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools len = %d, want 1 (injected web_search)", len(tools))
	}
	if spec := tools[0].(map[string]any)["toolSpecification"].(map[string]any); spec["name"] != "web_search" {
		t.Errorf("injected tool name = %v", spec["name"])
	}

	// 客户端已声明同义工具（Hosted 命中）时不重复注入
	req2 := textReq(ir.Message{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}})
	req2.Metadata = map[string]string{"kiro_web_search": "1"}
	req2.Tools = []ir.Tool{{Hosted: ir.HostedWebSearch}}
	p2 := payloadOf(t, req2, "claude-sonnet-4")
	ctx2, _ := userInput(t, p2)["userInputMessageContext"].(map[string]any)
	tools2, _ := ctx2["tools"].([]any)
	if len(tools2) != 1 {
		t.Errorf("tools len = %d, want 1 (no duplicate injection)", len(tools2))
	}

	// 无标志不注入
	p3 := payloadOf(t, textReq(ir.Message{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}), "claude-sonnet-4")
	ctx3, _ := userInput(t, p3)["userInputMessageContext"].(map[string]any)
	if tools3, ok := ctx3["tools"].([]any); ok && len(tools3) > 0 {
		t.Errorf("tools should be absent without inject flag, got %v", tools3)
	}
}

// 原生声明 name 但无 schema 的 hosted web_search 也补全 schema
// （Kiro 按普通工具要求非空 schema）。
func TestBuildPayload_HostedWebSearchNamedNoSchema(t *testing.T) {
	req := textReq(ir.Message{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}})
	req.Tools = []ir.Tool{{Name: "web_search", Hosted: ir.HostedWebSearch}}
	p := payloadOf(t, req, "claude-sonnet-4")
	ctx, _ := userInput(t, p)["userInputMessageContext"].(map[string]any)
	tools, _ := ctx["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools len = %d, want 1", len(tools))
	}
	spec := tools[0].(map[string]any)["toolSpecification"].(map[string]any)
	schema := spec["inputSchema"].(map[string]any)["json"].(map[string]any)
	if _, has := schema["properties"].(map[string]any)["query"]; !has {
		t.Errorf("schema must be filled for named hosted web_search: %v", schema)
	}
}

// 服务端工具块（server_tool_use / web_search_tool_result）不进 Kiro 载荷；
// 搜索结果由 assistant 文本块承载。
func TestBuildPayload_ServerToolBlocksSkipped(t *testing.T) {
	msgs := []ir.Message{
		{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "search go release notes"}}},
		{Role: ir.RoleAssistant, Content: []ir.Block{
			{Type: ir.BlockServerToolUse, ServerToolUse: &ir.ServerToolUse{
				ID: "srvtoolu_1", Name: "web_search", Input: json.RawMessage(`{"query":"go release notes"}`),
			}},
			{Type: ir.BlockText, Text: "<web_search>\nresults here\n</web_search>"},
		}},
		{Role: ir.RoleUser, Content: []ir.Block{
			{Type: ir.BlockWebSearchToolResult, WebSearchToolResult: &ir.WebSearchToolResult{
				ToolUseID: "srvtoolu_1",
				Results:   []ir.WebSearchResult{{Title: "Go 1.24", URL: "https://go.dev", Snippet: "released"}},
			}},
			{Type: ir.BlockText, Text: "summarize"},
		}},
	}
	p := payloadOf(t, textReq(msgs...), "claude-sonnet-4")
	h := historyOf(t, p)
	if len(h) != 2 {
		t.Fatalf("history len = %d, want 2", len(h))
	}
	asst, ok := h[1].(map[string]any)["assistantResponseMessage"].(map[string]any)
	if !ok {
		t.Fatalf("history[1] not assistant: %v", h[1])
	}
	if asst["toolUses"] != nil {
		t.Errorf("server_tool_use must not become toolUses: %v", asst["toolUses"])
	}
	if !strings.Contains(asst["content"].(string), "<web_search>") {
		t.Errorf("assistant text must carry search summary, got %q", asst["content"])
	}
	// user 消息的 web_search_tool_result 不产生 toolResults
	cur := userInput(t, p)
	if ctx, ok := cur["userInputMessageContext"].(map[string]any); ok {
		if trs, ok := ctx["toolResults"].([]any); ok && len(trs) > 0 {
			t.Errorf("web_search_tool_result must not become toolResults: %v", trs)
		}
	}
}
