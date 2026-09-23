package relay

import (
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
	"github.com/aceaura/ModelSurge/agent/proto"
	_ "github.com/aceaura/ModelSurge/agent/proto/anthropic"
)

// R91：历史消息里的不透明块（Anthropic 服务端工具结果、search_result、
// mid_conv_system）仅 anthropic 上游能接住。外族没有承载其载荷的槽位，必须报出
// 整块丢失，且注记不能抄出块体——它属会话内容，会随诊断头 X-ModelSurge-Notes
// 回到客户端与日志。

const r91WireType = "web_fetch_tool_result"

const r91Body = `{"type":"web_fetch_tool_result","tool_use_id":"tfu_1","content":{"type":"web_fetch_result","content":[{"type":"text","text":"fetched page body"}]}}`

const r91Secret = "fetched page body"

func r91Block() ir.Block {
	return ir.Block{Type: ir.BlockOpaque,
		Opaque: &ir.Opaque{WireType: r91WireType, Body: []byte(r91Body)}}
}

// agent 侧的外族出站协议。
var r91ForeignOutbound = []string{"codex", "openai-chat", "openai-responses"}

func r91Req() *ir.Request {
	return &ir.Request{Messages: []ir.Message{
		{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}},
		{Role: ir.RoleAssistant, Content: []ir.Block{
			r91Block(),
			{Type: ir.BlockText, Text: "answer"},
		}},
	}}
}

func TestDiagnoseOpaqueDroppedOffAnthropic(t *testing.T) {
	req := r91Req()
	for _, name := range r91ForeignOutbound {
		got := strings.Join(Diagnose(req, name, capsOf(t, name)), "; ")
		if !strings.Contains(got, "dropped 1 opaque content block(s)") {
			t.Errorf("%s 应报不透明块丢失：%q", name, got)
		}
		if strings.Contains(got, r91Secret) || strings.Contains(got, r91Body) {
			t.Errorf("%s 注记泄漏块体：%q", name, got)
		}
	}
	if notes := Diagnose(req, "anthropic", capsOf(t, "anthropic")); len(notes) != 0 {
		t.Errorf("anthropic 自家接得住，误报：%v", notes)
	}
}

// 与 redacted_thinking、container_upload 各自独立计数：三者都「外族无槽位」，
// 但客户端要能分辨丢的是哪一种，合并计数会把三条不同的说明压成一条。
func TestDiagnoseOpaqueCountedSeparatelyFromSiblings(t *testing.T) {
	req := &ir.Request{Messages: []ir.Message{{Role: ir.RoleAssistant, Content: []ir.Block{
		r91Block(),
		{Type: ir.BlockRedactedThinking, RedactedData: "cipher"},
		{Type: ir.BlockContainerUpload, ContainerUpload: &ir.ContainerUploadRef{FileID: "file_1"}},
	}}}}
	got := strings.Join(Diagnose(req, "openai-chat", capsOf(t, "openai-chat")), "; ")
	for _, want := range []string{
		"dropped 1 opaque content block(s)",
		"dropped 1 redacted thinking block(s)",
		"dropped 1 container upload",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("缺少独立注记 %q：%s", want, got)
		}
	}
}

func TestDiagnoseOpaqueCountAndAbsent(t *testing.T) {
	req := &ir.Request{Messages: []ir.Message{{Role: ir.RoleAssistant, Content: []ir.Block{
		r91Block(), r91Block(),
	}}}}
	got := strings.Join(Diagnose(req, "openai-chat", capsOf(t, "openai-chat")), "; ")
	if !strings.Contains(got, "dropped 2 opaque content block(s)") {
		t.Errorf("多块应计数：%q", got)
	}
	clean := &ir.Request{Messages: []ir.Message{{Role: ir.RoleUser,
		Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}}}
	for _, name := range append([]string{"anthropic"}, r91ForeignOutbound...) {
		if notes := Diagnose(clean, name, capsOf(t, name)); len(notes) != 0 {
			t.Errorf("%s 无不透明块却报损耗：%v", name, notes)
		}
	}
}

// 「上游非流式、客户端流式」这条重放路径要把不透明块整块带过去。它没有增量
// 形态，块体只能随 EvBlockStart 全量下发；给它补 delta 事件反而是无中生有。
func TestEventsFromResponseCarriesOpaqueBlock(t *testing.T) {
	resp := &ir.Response{ID: "msg", Model: "m", StopReason: ir.StopEndTurn,
		Content: []ir.Block{r91Block(), {Type: ir.BlockText, Text: "visible answer"}}}
	events := EventsFromResponse(resp)
	var got *ir.Block
	starts, stops := 0, 0
	for _, ev := range events {
		switch ev.Type {
		case ir.EvBlockStart:
			starts++
			if ev.Index == 0 {
				got = ev.Block
			}
		case ir.EvBlockStop:
			stops++
		case ir.EvTextDelta, ir.EvThinkingDelta, ir.EvSigDelta:
			if ev.Index == 0 {
				t.Errorf("不透明块不该有增量事件：%s %q", ev.Type, ev.Text)
			}
		}
	}
	if starts != 2 || stops != 2 {
		t.Fatalf("块框架不配平：start=%d stop=%d", starts, stops)
	}
	if got == nil || got.Type != ir.BlockOpaque || got.Opaque == nil ||
		string(got.Opaque.Body) != r91Body {
		t.Fatalf("块开始事件丢块体：%+v", got)
	}
	// 重放不得改动调用方持有的聚合响应（usage 估算与日志摘要还在读它）。
	if len(resp.Content) != 2 || string(resp.Content[0].Opaque.Body) != r91Body {
		t.Errorf("原响应被就地改坏：%+v", resp.Content)
	}
}

// 端到端：重放出的事件流交给 anthropic 编码器，客户端要能在 content_block_start
// 里收到完整块体——否则这一轮之后客户端无从回传，上游的配平校验拒下一轮。
func TestEventsFromResponseOpaqueReachesAnthropicStream(t *testing.T) {
	resp := &ir.Response{ID: "msg", Model: "m", StopReason: ir.StopEndTurn,
		Content: []ir.Block{r91Block(), {Type: ir.BlockText, Text: "visible answer"}}}
	enc := proto.MustInbound("anthropic").NewStreamEncoder()
	var sb strings.Builder
	for _, ev := range EventsFromResponse(resp) {
		frames, err := enc.Encode(ev)
		if err != nil {
			t.Fatalf("Encode(%s idx=%d): %v", ev.Type, ev.Index, err)
		}
		for _, fr := range frames {
			sb.Write(fr)
		}
	}
	for _, fr := range enc.Finish() {
		sb.Write(fr)
	}
	out := sb.String()
	if !strings.Contains(out, `"content_block":`+r91Body) {
		t.Errorf("客户端流里没有完整不透明块：\n%s", out)
	}
	if !strings.Contains(out, "visible answer") {
		t.Errorf("正文被误删：\n%s", out)
	}
	if notes := enc.Notes(); len(notes) != 0 {
		t.Errorf("anthropic 自家误报损耗：%v", notes)
	}
}

// 抑制思考时不得顺手删掉不透明块：它不是思考族，里面是工具抓回来的网页正文、
// 命令输出，删了客户端这一轮就少了一段模型确实产出过的内容。
func TestStripThinkingKeepsOpaqueBlock(t *testing.T) {
	if isThoughtBlock(ir.BlockOpaque) {
		t.Fatal("不透明块被归入思考族")
	}
	resp := &ir.Response{ID: "msg", Model: "m", StopReason: ir.StopEndTurn,
		Content: []ir.Block{
			{Type: ir.BlockThinking, Thinking: &ir.Thinking{Text: "secret reasoning"}},
			r91Block(),
			{Type: ir.BlockText, Text: "visible answer"},
		}}
	got := stripThinking(resp)
	if len(got.Content) != 2 {
		t.Fatalf("块数 = %d，want 2（思考块被删、其余保留）：%+v", len(got.Content), got.Content)
	}
	if got.Content[0].Type != ir.BlockOpaque || string(got.Content[0].Opaque.Body) != r91Body {
		t.Errorf("不透明块被一并删掉：%+v", got.Content[0])
	}
	// 原响应不得被就地改坏。
	if len(resp.Content) != 3 {
		t.Errorf("原响应被就地改坏：块数 = %d", len(resp.Content))
	}
}
