package normalize

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// 降级把一个输入块展开成多个输出块（文本 + 抽出的媒体），此时若复用
// m.Content 的底层数组，写头会越过读游标，把同一条消息里后面还没读到的块
// 覆盖掉。真实场景：客户端在同一条 user 消息里先放 tool_result 再放新指令，
// 指令整块消失而上游返回 200——比报错更难发现。
//
// 这类测试必须断言「后续块还在」且「媒体没重复」：只断言 tool 块消失、或
// 只断言媒体出现过，覆盖发生时依然全绿（媒体被写了两遍，断言照样命中）。

const inplacePDF = "JVBERi0xLjQK"

func toolResultBlock(id string) ir.Block {
	return ir.Block{Type: ir.BlockToolResult, ToolResult: &ir.ToolResult{
		ToolUseID: id,
		Content: []ir.Block{
			{Type: ir.BlockText, Text: "tool said"},
			{Type: ir.BlockMedia, Media: &ir.Media{
				Kind: ir.MediaDocument, MediaType: "application/pdf", Data: inplacePDF,
			}},
		},
	}}
}

func countBlocks(blocks []ir.Block, typ ir.BlockType) int {
	n := 0
	for _, b := range blocks {
		if b.Type == typ {
			n++
		}
	}
	return n
}

func joinText(blocks []ir.Block) string {
	var sb strings.Builder
	for _, b := range blocks {
		if b.Type == ir.BlockText {
			sb.WriteString(b.Text)
			sb.WriteString("\n")
		}
	}
	return sb.String()
}

// stripToolContent 路径：无 tools 时降级。
func TestStripToolContentKeepsTrailingBlocks(t *testing.T) {
	req := &ir.Request{Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{
		toolResultBlock("t1"),
		{Type: ir.BlockText, Text: "NOW-PLEASE-DO-X"},
	}}}}
	if err := Request(req, Options{StripToolsIfNoTools: true}); err != nil {
		t.Fatal(err)
	}
	got := req.Messages[0].Content
	if !strings.Contains(joinText(got), "NOW-PLEASE-DO-X") {
		t.Errorf("降级覆盖了后续用户指令：%+v", got)
	}
	if n := countBlocks(got, ir.BlockMedia); n != 1 {
		t.Errorf("媒体块数量 = %d, want 1（原地写入会重复）", n)
	}
}

// fixOrphanToolResults 路径：声明了 tools 但无配对 tool_use。
func TestOrphanDowngradeKeepsTrailingBlocks(t *testing.T) {
	req := &ir.Request{
		Tools: []ir.Tool{{Name: "read", InputSchema: json.RawMessage(`{"type":"object"}`)}},
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{
			toolResultBlock("orphan"),
			{Type: ir.BlockText, Text: "NOW-PLEASE-DO-X"},
		}}},
	}
	if err := Request(req, Options{FixOrphanToolResults: true}); err != nil {
		t.Fatal(err)
	}
	got := req.Messages[0].Content
	if !strings.Contains(joinText(got), "NOW-PLEASE-DO-X") {
		t.Errorf("降级覆盖了后续用户指令：%+v", got)
	}
	if n := countBlocks(got, ir.BlockMedia); n != 1 {
		t.Errorf("媒体块数量 = %d, want 1（原地写入会重复）", n)
	}
	if n := countBlocks(got, ir.BlockToolResult); n != 0 {
		t.Errorf("孤儿 tool_result 未降级：%d 个仍在", n)
	}
}

// 多个 tool_result 连续出现时越界更深：两次展开会吃掉两个后续块。
func TestStripToolContentMultipleExpansions(t *testing.T) {
	req := &ir.Request{Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{
		toolResultBlock("t1"),
		toolResultBlock("t2"),
		{Type: ir.BlockText, Text: "KEEP-A"},
		{Type: ir.BlockText, Text: "KEEP-B"},
	}}}}
	if err := Request(req, Options{StripToolsIfNoTools: true}); err != nil {
		t.Fatal(err)
	}
	got := req.Messages[0].Content
	text := joinText(got)
	for _, want := range []string{"KEEP-A", "KEEP-B"} {
		if !strings.Contains(text, want) {
			t.Errorf("块 %s 被覆盖：%+v", want, got)
		}
	}
	if n := countBlocks(got, ir.BlockMedia); n != 2 {
		t.Errorf("媒体块数量 = %d, want 2", n)
	}
}

// 不展开的消息（纯文本）不得因改用独立切片而改变内容或顺序。
func TestStripToolContentPreservesOrder(t *testing.T) {
	req := &ir.Request{Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{
		{Type: ir.BlockText, Text: "one"},
		{Type: ir.BlockImage, Image: &ir.Image{MediaType: "image/png", Data: "iVBOR"}},
		{Type: ir.BlockText, Text: "two"},
	}}}}
	if err := Request(req, Options{StripToolsIfNoTools: true}); err != nil {
		t.Fatal(err)
	}
	got := req.Messages[0].Content
	want := []ir.BlockType{ir.BlockText, ir.BlockImage, ir.BlockText}
	if len(got) != len(want) {
		t.Fatalf("块数 = %d, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].Type != want[i] {
			t.Errorf("块[%d] = %q, want %q", i, got[i].Type, want[i])
		}
	}
	if got[0].Text != "one" || got[2].Text != "two" {
		t.Errorf("文本顺序被打乱：%+v", got)
	}
}

// 调用方持有的原始切片不得被就地改写：Normalize 之外的代码（诊断、日志、
// 重试时的原请求）还会读它，原地写入会让它们看到半改写的内容。
func TestStripToolContentDoesNotMutateCallerSlice(t *testing.T) {
	orig := []ir.Block{
		toolResultBlock("t1"),
		{Type: ir.BlockText, Text: "NOW-PLEASE-DO-X"},
	}
	req := &ir.Request{Messages: []ir.Message{{Role: ir.RoleUser, Content: orig}}}
	if err := Request(req, Options{StripToolsIfNoTools: true}); err != nil {
		t.Fatal(err)
	}
	if orig[1].Type != ir.BlockText || orig[1].Text != "NOW-PLEASE-DO-X" {
		t.Errorf("调用方切片被就地改写：%+v", orig[1])
	}
	if orig[0].Type != ir.BlockToolResult {
		t.Errorf("调用方切片首块被就地改写：%+v", orig[0])
	}
}

func TestOrphanDowngradeDoesNotMutateCallerSlice(t *testing.T) {
	orig := []ir.Block{
		toolResultBlock("orphan"),
		{Type: ir.BlockText, Text: "NOW-PLEASE-DO-X"},
	}
	req := &ir.Request{
		Tools:    []ir.Tool{{Name: "read", InputSchema: json.RawMessage(`{"type":"object"}`)}},
		Messages: []ir.Message{{Role: ir.RoleUser, Content: orig}},
	}
	if err := Request(req, Options{FixOrphanToolResults: true}); err != nil {
		t.Fatal(err)
	}
	if orig[1].Type != ir.BlockText || orig[1].Text != "NOW-PLEASE-DO-X" {
		t.Errorf("调用方切片被就地改写：%+v", orig[1])
	}
}

// mergeAdjacent 同样复用 req.Messages 的底层数组，但它只做 1->0/1 的收缩，
// 写头永不超过读游标。钉住这一点：将来若在合并里插入消息就会立刻变红。
func TestMergeAdjacentKeepsAllContent(t *testing.T) {
	req := &ir.Request{Messages: []ir.Message{
		{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "a"}}},
		{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "b"}}},
		{Role: ir.RoleAssistant, Content: []ir.Block{{Type: ir.BlockText, Text: "c"}}},
		{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "d"}}},
	}}
	if err := Request(req, Options{MergeAdjacentRoles: true}); err != nil {
		t.Fatal(err)
	}
	var all strings.Builder
	for _, m := range req.Messages {
		all.WriteString(m.Text())
	}
	if got := all.String(); got != "abcd" {
		t.Errorf("合并后内容 = %q, want \"abcd\"", got)
	}
}

// 降级后的文本块必须真的带上工具结果正文，否则「后续块还在」的断言可能
// 因为渲染出空文本而被误判成通过。
func TestDowngradedTextCarriesToolOutput(t *testing.T) {
	req := &ir.Request{Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{
		toolResultBlock("t1"),
		{Type: ir.BlockText, Text: "NOW-PLEASE-DO-X"},
	}}}}
	if err := Request(req, Options{StripToolsIfNoTools: true}); err != nil {
		t.Fatal(err)
	}
	text := joinText(req.Messages[0].Content)
	if !strings.Contains(text, "tool said") {
		t.Errorf("工具结果正文丢失：%q", text)
	}
	if !strings.Contains(text, "t1") {
		t.Errorf("工具结果未带 tool_use_id：%q", text)
	}
}
