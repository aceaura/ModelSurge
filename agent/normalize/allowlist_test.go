package normalize

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// R100 规整流水线的三步 tool_choice 处置。三步都在 Request 的最前面跑，且判据都
// 落在 ir 上（AllowlistNarrow / ForcedToolUndeclared / ToolChoiceWithoutTools），
// relay.Diagnose 报损耗用的是同一组函数——两处各写一遍必然漂移，漂移之后要么明明
// 收窄成功却照报损耗，要么明明无从收窄却不报。

func r100names(tools []ir.Tool) string {
	var out []string
	for _, t := range tools {
		out = append(out, t.Name)
	}
	return strings.Join(out, ",")
}

func r100tools(names ...string) []ir.Tool {
	var out []ir.Tool
	for _, n := range names {
		out = append(out, ir.Tool{Name: n})
	}
	return out
}

// ---- 收窄 ----

// 白名单落地就是把声明的工具收窄成交集：上游看不见别的工具就调不到。不收窄而照
// 原样声明，等于把客户端明令禁止的工具又递了回去。
func TestEnforceToolAllowlistNarrowsToIntersection(t *testing.T) {
	req := &ir.Request{
		Tools:      r100tools("alpha", "beta", "gamma"),
		ToolChoice: &ir.ToolChoice{Mode: ir.ChoiceAny, AllowedTools: []string{"beta", "alpha"}},
	}
	EnforceToolAllowlist(req)
	// 顺序按原声明序保留：收窄是过滤，不是重排，重排会让客户端日志里的工具序与
	// 上游看到的对不上。
	if got := r100names(req.Tools); got != "alpha,beta" {
		t.Errorf("收窄结果 = %q, want alpha,beta", got)
	}
}

// 交集为空时不收窄。收窄到零个工具会触发 StripToolsIfNoTools，把整段工具历史
// 改写成文本——那是比白名单失效大得多的破坏。
func TestEnforceToolAllowlistNeverEmptiesToolList(t *testing.T) {
	req := &ir.Request{
		Tools:      r100tools("alpha", "beta"),
		ToolChoice: &ir.ToolChoice{Mode: ir.ChoiceAny, AllowedTools: []string{"nosuch"}},
	}
	EnforceToolAllowlist(req)
	if got := r100names(req.Tools); got != "alpha,beta" {
		t.Errorf("白名单落空时不该动工具列表，得到 %q", got)
	}
}

// 托管工具一律保留，且不计入交集。Gemini 的 allowedFunctionNames 管的是
// functionDeclarations，把 google_search 连带删掉是删了客户端声明过的东西。
func TestEnforceToolAllowlistKeepsHostedTools(t *testing.T) {
	req := &ir.Request{
		Tools: []ir.Tool{
			{Name: "google_search", Hosted: ir.HostedWebSearch},
			{Name: "alpha"},
			{Name: "gamma"},
		},
		ToolChoice: &ir.ToolChoice{Mode: ir.ChoiceAuto, AllowedTools: []string{"alpha"}},
	}
	EnforceToolAllowlist(req)
	if got := r100names(req.Tools); got != "google_search,alpha" {
		t.Errorf("收窄结果 = %q, want google_search,alpha", got)
	}
}

// 指名与禁止两档不收窄：前者上游本来就只会调那一个，后者一个都不调，多声明的工具
// 调不到，删掉只是白丢客户端的声明。
func TestEnforceToolAllowlistSkipsForcedAndNoneModes(t *testing.T) {
	for _, mode := range []ir.ChoiceMode{ir.ChoiceTool, ir.ChoiceNone} {
		req := &ir.Request{
			Tools:      r100tools("alpha", "beta"),
			ToolChoice: &ir.ToolChoice{Mode: mode, ToolName: "alpha", AllowedTools: []string{"alpha"}},
		}
		EnforceToolAllowlist(req)
		if got := r100names(req.Tools); got != "alpha,beta" {
			t.Errorf("%s 档不该收窄，得到 %q", mode, got)
		}
	}
}

// 没有白名单、没有 tool_choice、请求为 nil：三种情况都不得改动任何东西，也不得 panic。
func TestEnforceToolAllowlistNoopsWithoutAllowlist(t *testing.T) {
	req := &ir.Request{Tools: r100tools("alpha"), ToolChoice: &ir.ToolChoice{Mode: ir.ChoiceAny}}
	EnforceToolAllowlist(req)
	if got := r100names(req.Tools); got != "alpha" {
		t.Errorf("无白名单却收窄了：%q", got)
	}
	noChoice := &ir.Request{Tools: r100tools("alpha")}
	EnforceToolAllowlist(noChoice)
	EnforceToolAllowlist(nil)
	if got := r100names(noChoice.Tools); got != "alpha" {
		t.Errorf("无 tool_choice 却收窄了：%q", got)
	}
}

// ---- 指名回落 ----

// 指着一个没声明的工具是接收侧必 400 的形状，回落成 auto 让这一轮能跑完。
func TestRelaxUndeclaredForcedTool(t *testing.T) {
	req := &ir.Request{
		Tools:      r100tools("alpha"),
		ToolChoice: &ir.ToolChoice{Mode: ir.ChoiceTool, ToolName: "nosuch"},
	}
	RelaxUndeclaredForcedTool(req)
	if req.ToolChoice.Mode != ir.ChoiceAuto || req.ToolChoice.ToolName != "" {
		t.Errorf("回落不彻底：%+v", req.ToolChoice)
	}
}

// 指名已声明的工具、以及非指名档，都不许动。改坏一个能用的请求比不改更糟。
func TestRelaxUndeclaredForcedToolLeavesValidChoicesAlone(t *testing.T) {
	declared := &ir.Request{Tools: r100tools("alpha", "beta"),
		ToolChoice: &ir.ToolChoice{Mode: ir.ChoiceTool, ToolName: "beta"}}
	RelaxUndeclaredForcedTool(declared)
	if declared.ToolChoice.Mode != ir.ChoiceTool || declared.ToolChoice.ToolName != "beta" {
		t.Errorf("指名已声明工具被改坏了：%+v", declared.ToolChoice)
	}
	for _, mode := range []ir.ChoiceMode{ir.ChoiceAuto, ir.ChoiceAny, ir.ChoiceNone} {
		req := &ir.Request{Tools: r100tools("alpha"), ToolChoice: &ir.ToolChoice{Mode: mode}}
		RelaxUndeclaredForcedTool(req)
		if req.ToolChoice.Mode != mode {
			t.Errorf("%s 档被误回落成 %s", mode, req.ToolChoice.Mode)
		}
	}
	// 一个工具都没声明时不归这一步管（见下），否则报错会指向「名字写错」这个错方向。
	empty := &ir.Request{ToolChoice: &ir.ToolChoice{Mode: ir.ChoiceTool, ToolName: "alpha"}}
	RelaxUndeclaredForcedTool(empty)
	if empty.ToolChoice.Mode != ir.ChoiceTool {
		t.Errorf("零工具该由 DropToolChoiceWithoutTools 处置，这里不该动：%+v", empty.ToolChoice)
	}
}

// 托管工具按名字比对即可命中：跨协议时它的线上名字会被换成目标协议的固定名，
// 那一层错位不是「名字没声明」，在这里回落只会把能用的请求改坏。
func TestRelaxUndeclaredForcedToolAcceptsHostedName(t *testing.T) {
	req := &ir.Request{
		Tools:      []ir.Tool{{Name: "google_search", Hosted: ir.HostedWebSearch}},
		ToolChoice: &ir.ToolChoice{Mode: ir.ChoiceTool, ToolName: "google_search"},
	}
	RelaxUndeclaredForcedTool(req)
	if req.ToolChoice.Mode != ir.ChoiceTool {
		t.Errorf("指名托管工具被误回落：%+v", req.ToolChoice)
	}
}

// ---- 零工具删除 ----

// 零工具 + 要求有工具的 tool_choice：整条删掉。tools 键因空数组被省略，留着它就是
// 「必须调工具，但没有工具」。
func TestDropToolChoiceWithoutTools(t *testing.T) {
	for _, mode := range []ir.ChoiceMode{ir.ChoiceAny, ir.ChoiceTool} {
		req := &ir.Request{ToolChoice: &ir.ToolChoice{Mode: mode, ToolName: "alpha"}}
		DropToolChoiceWithoutTools(req)
		if req.ToolChoice != nil {
			t.Errorf("%s 档零工具时该整条删掉，得到 %+v", mode, req.ToolChoice)
		}
	}
}

// auto 与 none 在零工具下是接收侧接受的形状，且与不带 tool_choice 同义：不删。
// 有工具时四档一律不删。
func TestDropToolChoiceWithoutToolsIsNarrow(t *testing.T) {
	for _, mode := range []ir.ChoiceMode{ir.ChoiceAuto, ir.ChoiceNone} {
		req := &ir.Request{ToolChoice: &ir.ToolChoice{Mode: mode}}
		DropToolChoiceWithoutTools(req)
		if req.ToolChoice == nil {
			t.Errorf("%s 档不该被删", mode)
		}
	}
	for _, mode := range []ir.ChoiceMode{ir.ChoiceAuto, ir.ChoiceAny, ir.ChoiceTool, ir.ChoiceNone} {
		req := &ir.Request{Tools: r100tools("alpha"),
			ToolChoice: &ir.ToolChoice{Mode: mode, ToolName: "alpha"}}
		DropToolChoiceWithoutTools(req)
		if req.ToolChoice == nil {
			t.Errorf("有工具时 %s 档不该被删", mode)
		}
	}
	DropToolChoiceWithoutTools(nil)
	DropToolChoiceWithoutTools(&ir.Request{})
}

// ---- 三步在流水线里的次序 ----

// 收窄跑在硬性校验之前：被白名单排除的工具压根不会发给上游，它的名字长度不再是
// 这一轮的问题，不该为它整轮失败。
func TestAllowlistNarrowingRunsBeforeNameLengthCheck(t *testing.T) {
	long := strings.Repeat("x", 80)
	req := &ir.Request{
		Tools:      []ir.Tool{{Name: "alpha"}, {Name: long}},
		ToolChoice: &ir.ToolChoice{Mode: ir.ChoiceAny, AllowedTools: []string{"alpha"}},
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}},
	}
	o := Strict()
	o.MaxToolNameLength = 64
	if err := Request(req, o); err != nil {
		t.Fatalf("被排除的超长工具名不该让整轮失败：%v", err)
	}
	if got := r100names(req.Tools); got != "alpha" {
		t.Errorf("收窄结果 = %q, want alpha", got)
	}
}

// 同一条超长名字若没被白名单排除，硬性校验照旧生效——上一步不许把校验一起关掉。
func TestNameLengthCheckStillFiresForRetainedTools(t *testing.T) {
	req := &ir.Request{
		Tools:      []ir.Tool{{Name: strings.Repeat("x", 80)}},
		ToolChoice: &ir.ToolChoice{Mode: ir.ChoiceAny, AllowedTools: []string{strings.Repeat("x", 80)}},
	}
	o := Strict()
	o.MaxToolNameLength = 64
	if err := Request(req, o); err == nil {
		t.Fatal("保留下来的超长工具名仍该让整轮失败")
	}
}

// 收窄之后工具列表非空，就不该触发无工具分支：工具历史必须保持块型，不能被渲染成
// 文本。这是「交集为空不收窄」那条边界的正面见证。
func TestNarrowedRequestKeepsToolHistoryTyped(t *testing.T) {
	req := &ir.Request{
		Tools:      r100tools("alpha", "gamma"),
		ToolChoice: &ir.ToolChoice{Mode: ir.ChoiceAuto, AllowedTools: []string{"alpha"}},
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "run gamma"}}},
			{Role: ir.RoleAssistant, Content: []ir.Block{{Type: ir.BlockToolUse,
				ToolUse: &ir.ToolUse{ID: "t1", Name: "gamma", Input: json.RawMessage(`{"x":1}`)}}}},
			{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockToolResult,
				ToolResult: &ir.ToolResult{ToolUseID: "t1",
					Content: []ir.Block{{Type: ir.BlockText, Text: "ok"}}}}}},
		},
	}
	if err := Request(req, Strict()); err != nil {
		t.Fatalf("Request: %v", err)
	}
	if got := r100names(req.Tools); got != "alpha" {
		t.Fatalf("收窄结果 = %q, want alpha", got)
	}
	typed := 0
	for _, m := range req.Messages {
		for _, b := range m.Content {
			if b.Type == ir.BlockToolUse || b.Type == ir.BlockToolResult {
				typed++
			}
			if b.Type == ir.BlockText && strings.Contains(b.Text, "[Tool") {
				t.Errorf("被排除工具的历史被渲染成了文本：%q", b.Text)
			}
		}
	}
	if typed != 2 {
		t.Errorf("工具历史块型只剩 %d 个，该是调用 + 结果两个", typed)
	}
}

// 白名单落空 + 工具历史齐全：不许收窄，也就不许触发无工具分支。这一态的落差由
// relay.Diagnose 报出，规整流水线只负责不把请求改坏。
func TestUnenforceableAllowlistDoesNotStripToolHistory(t *testing.T) {
	req := &ir.Request{
		Tools:      r100tools("alpha"),
		ToolChoice: &ir.ToolChoice{Mode: ir.ChoiceAny, AllowedTools: []string{"nosuch"}},
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}},
			{Role: ir.RoleAssistant, Content: []ir.Block{{Type: ir.BlockToolUse,
				ToolUse: &ir.ToolUse{ID: "t1", Name: "alpha", Input: json.RawMessage(`{}`)}}}},
			{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockToolResult,
				ToolResult: &ir.ToolResult{ToolUseID: "t1",
					Content: []ir.Block{{Type: ir.BlockText, Text: "ok"}}}}}},
		},
	}
	if err := Request(req, Strict()); err != nil {
		t.Fatalf("Request: %v", err)
	}
	if got := r100names(req.Tools); got != "alpha" {
		t.Errorf("白名单落空时不该动工具列表，得到 %q", got)
	}
	for _, m := range req.Messages {
		for _, b := range m.Content {
			if b.Type == ir.BlockText && strings.Contains(b.Text, "[Tool") {
				t.Errorf("工具历史被渲染成了文本：%q", b.Text)
			}
		}
	}
}

// 零工具那一态走完流水线之后，tool_choice 必须已经没了，而历史照旧渲染成文本、
// 正文一个字不丢。
func TestZeroToolsPipelineDropsChoiceAndKeepsText(t *testing.T) {
	req := &ir.Request{
		ToolChoice: &ir.ToolChoice{Mode: ir.ChoiceTool, ToolName: "alpha"},
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}},
			{Role: ir.RoleAssistant, Content: []ir.Block{{Type: ir.BlockToolUse,
				ToolUse: &ir.ToolUse{ID: "t1", Name: "alpha", Input: json.RawMessage(`{}`)}}}},
			{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockToolResult,
				ToolResult: &ir.ToolResult{ToolUseID: "t1",
					Content: []ir.Block{{Type: ir.BlockText, Text: "ok"}}}}}},
		},
	}
	if err := Request(req, Strict()); err != nil {
		t.Fatalf("Request: %v", err)
	}
	if req.ToolChoice != nil {
		t.Errorf("零工具时 tool_choice 该被删掉：%+v", req.ToolChoice)
	}
	var text []string
	for _, m := range req.Messages {
		for _, b := range m.Content {
			if b.Type == ir.BlockText {
				text = append(text, b.Text)
			}
		}
	}
	joined := strings.Join(text, "|")
	for _, want := range []string{"hi", "ok", "alpha", "t1"} {
		if !strings.Contains(joined, want) {
			t.Errorf("历史正文丢了 %q：%s", want, joined)
		}
	}
}

// 这一步不看 StripToolsIfNoTools 开关：空 tools 是既成事实，与用哪套选项无关。
// 关掉该开关的调用方（若有）同样不能把必 400 的形状放出去。
func TestDropToolChoiceHappensEvenWhenStrippingDisabled(t *testing.T) {
	req := &ir.Request{
		ToolChoice: &ir.ToolChoice{Mode: ir.ChoiceAny},
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}},
	}
	o := Strict()
	o.StripToolsIfNoTools = false
	if err := Request(req, o); err != nil {
		t.Fatalf("Request: %v", err)
	}
	if req.ToolChoice != nil {
		t.Errorf("关掉无工具分支也该删掉 tool_choice：%+v", req.ToolChoice)
	}
}
