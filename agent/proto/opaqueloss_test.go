package proto_test

import (
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
	"github.com/aceaura/ModelSurge/agent/proto"
	_ "github.com/aceaura/ModelSurge/agent/proto/anthropic"
	_ "github.com/aceaura/ModelSurge/agent/proto/gemini"
	_ "github.com/aceaura/ModelSurge/agent/proto/openaichat"
	_ "github.com/aceaura/ModelSurge/agent/proto/openairesponses"
)

// R91：不透明块的跨族损耗注记与跳过安全性。这类块（Anthropic 的 web_fetch /
// code_execution / bash_code_execution / text_editor_code_execution / tool_search
// 服务端工具结果、search_result、mid_conv_system）的判别值只在源协议里有定义，
// 外族没有承载其载荷的槽位。整块跳过不降级：把块体拼进正文会污染回答，塞进
// tool 结果槽位则是伪造一次没发生过的工具调用。损耗经 ResponseNotes / 流式
// Notes() 报出；块体属会话内容，不进注记。

// r91Body 取自官方 web_fetch_tool_result 形状。里面的正文用来检测泄漏。
const r91WireType = "web_fetch_tool_result"

const r91Body = `{"type":"web_fetch_tool_result","tool_use_id":"tfu_1","content":{"type":"web_fetch_result","content":[{"type":"text","text":"fetched page body"}]}}`

const r91Secret = "fetched page body"

func r91Block() ir.Block {
	return ir.Block{Type: ir.BlockOpaque,
		Opaque: &ir.Opaque{WireType: r91WireType, Body: []byte(r91Body), From: "anthropic"}}
}

// 入站外族：anthropic 之外的三家客户端协议。
var r91ForeignInbound = []string{"openai-chat", "openai-responses", "gemini"}

func r91Stream() []ir.Event {
	b := r91Block()
	return []ir.Event{
		{Type: ir.EvMessageStart, MessageID: "m", Model: "m"},
		{Type: ir.EvBlockStart, Index: 0, Block: &b},
		{Type: ir.EvBlockStop, Index: 0},
		{Type: ir.EvBlockStart, Index: 1, Block: &ir.Block{Type: ir.BlockText}},
		{Type: ir.EvTextDelta, Index: 1, Text: "visible answer"},
		{Type: ir.EvBlockStop, Index: 1},
		{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn, Usage: &ir.Usage{OutputTokens: 3}},
		{Type: ir.EvMessageStop},
	}
}

// 外族流式：块体不出现在线上，正文不受影响。
func TestOpaqueStreamForeignSkip(t *testing.T) {
	for _, name := range r91ForeignInbound {
		t.Run(name, func(t *testing.T) {
			out := streamOut(t, name, r91Stream())
			if strings.Contains(out, r91Secret) {
				t.Errorf("块体泄漏进流：\n%s", out)
			}
			if strings.Contains(out, r91WireType) {
				t.Errorf("源协议块名泄漏进流：\n%s", out)
			}
			if strings.Contains(out, "tfu_1") {
				t.Errorf("工具 id 泄漏进流：\n%s", out)
			}
			if !strings.Contains(out, "visible answer") {
				t.Errorf("正文被误删：\n%s", out)
			}
		})
	}
}

func TestOpaqueStreamForeignNotes(t *testing.T) {
	for _, name := range r91ForeignInbound {
		t.Run(name, func(t *testing.T) {
			e := proto.MustInbound(name).NewStreamEncoder()
			for _, ev := range r91Stream() {
				if _, err := e.Encode(ev); err != nil {
					t.Fatalf("Encode(%s): %v", ev.Type, err)
				}
			}
			got := strings.Join(e.Notes(), "; ")
			if !strings.Contains(got, "dropped 1 opaque content block(s)") {
				t.Errorf("应报 1 块丢失：%q", got)
			}
			if strings.Contains(got, r91Secret) || strings.Contains(got, r91Body) {
				t.Errorf("注记抄出块体：%q", got)
			}
			if again := e.Notes(); len(again) != 0 {
				t.Errorf("Notes 应幂等排干：%v", again)
			}
		})
	}
	// 多块计数聚合为一条。
	e := proto.MustInbound("openai-chat").NewStreamEncoder()
	e.Encode(ir.Event{Type: ir.EvMessageStart, MessageID: "m", Model: "m"})
	b0, b1 := r91Block(), r91Block()
	e.Encode(ir.Event{Type: ir.EvBlockStart, Index: 0, Block: &b0})
	e.Encode(ir.Event{Type: ir.EvBlockStop, Index: 0})
	e.Encode(ir.Event{Type: ir.EvBlockStart, Index: 1, Block: &b1})
	e.Encode(ir.Event{Type: ir.EvBlockStop, Index: 1})
	e.Encode(ir.Event{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn})
	got := strings.Join(e.Notes(), "; ")
	if !strings.Contains(got, "dropped 2 opaque content block(s)") {
		t.Errorf("多块应计数：%q", got)
	}
	// anthropic 自家 encoder 不报。
	ea := proto.MustInbound("anthropic").NewStreamEncoder()
	for _, ev := range r91Stream() {
		ea.Encode(ev)
	}
	if notes := ea.Notes(); len(notes) != 0 {
		t.Errorf("anthropic 误报：%v", notes)
	}
}

// responses 的 blockStart 有 default(text) 兜底：不显式拦住不透明块，客户端会
// 凭空多出一个空 output_text 条目——看起来像模型说了一句空话。
func TestOpaqueAbsentFromResponsesFullOutput(t *testing.T) {
	out := streamOut(t, "openai-responses", r91Stream())
	idx := strings.LastIndex(out, `"type":"response.completed"`)
	if idx < 0 {
		t.Fatalf("没有 response.completed：\n%s", out)
	}
	if final := out[idx:]; strings.Contains(final, r91Secret) || strings.Contains(final, r91WireType) {
		t.Errorf("全量 output 泄漏块体：\n%s", final)
	}
	if n := strings.Count(out, `"type":"response.output_item.added"`); n != 1 {
		t.Errorf("output_item.added 应只有正文一条，实得 %d（多出的是幽灵空文本条目）：\n%s", n, out)
	}
	if strings.Contains(out, `"type":"response.output_text.done","text":""`) {
		t.Errorf("凭空多出空 output_text：\n%s", out)
	}
}

// chat 侧同理：被跳过的块不得留下任何分片。整条流应只剩「开场 role + 正文 +
// 终止」三个 chunk；多出来的那一个就是不透明块漏出的空 content 分片。
func TestOpaqueNoGhostChatDelta(t *testing.T) {
	out := streamOut(t, "openai-chat", r91Stream())
	if strings.Contains(out, `"content":""`) {
		t.Errorf("凭空多出空 content 分片：\n%s", out)
	}
	if n := strings.Count(out, `"object":"chat.completion.chunk"`); n != 3 {
		t.Errorf("chunk 数应为 3（role/正文/终止），实得 %d：\n%s", n, out)
	}
	if strings.Count(out, "visible answer") != 1 {
		t.Errorf("正文分片数不对：\n%s", out)
	}
}

// gemini 侧：跳过的块不得留下空 part。
func TestOpaqueNoGhostGeminiPart(t *testing.T) {
	out := streamOut(t, "gemini", r91Stream())
	if strings.Contains(out, r91Secret) {
		t.Errorf("块体泄漏进 gemini 流：\n%s", out)
	}
	if !strings.Contains(out, "visible answer") {
		t.Errorf("正文被误删：\n%s", out)
	}
}

// 非流式响应侧：ScanResponseLosses 报出，anthropic 自家静默，无块全静默。
func TestOpaqueResponseNotes(t *testing.T) {
	resp := &ir.Response{ID: "m", Model: "m", Content: []ir.Block{
		r91Block(), {Type: ir.BlockText, Text: "visible answer"}}}
	for _, name := range r91ForeignInbound {
		got := strings.Join(proto.MustInbound(name).ResponseNotes(resp), "; ")
		if !strings.Contains(got, "dropped 1 opaque content block(s)") {
			t.Errorf("%s 应报块丢失：%q", name, got)
		}
		if strings.Contains(got, r91Secret) {
			t.Errorf("%s 注记抄出块体：%q", name, got)
		}
	}
	if notes := proto.MustInbound("anthropic").ResponseNotes(resp); len(notes) != 0 {
		t.Errorf("anthropic 误报：%v", notes)
	}
	resp.Content = resp.Content[1:]
	for _, name := range append([]string{"anthropic"}, r91ForeignInbound...) {
		if notes := proto.MustInbound(name).ResponseNotes(resp); len(notes) != 0 {
			t.Errorf("%s 无块误报：%v", name, notes)
		}
	}
}

// 非流式编码：外族线上不出现块体，正文保留。
func TestOpaqueNotLeakedIntoResponse(t *testing.T) {
	resp := &ir.Response{ID: "m", Model: "m", StopReason: ir.StopEndTurn,
		Content: []ir.Block{r91Block(), {Type: ir.BlockText, Text: "visible answer"}}}
	for _, name := range r91ForeignInbound {
		t.Run(name, func(t *testing.T) {
			body, err := proto.MustInbound(name).EncodeResponse(resp)
			if err != nil {
				t.Fatalf("EncodeResponse: %v", err)
			}
			if strings.Contains(string(body), r91Secret) || strings.Contains(string(body), r91WireType) {
				t.Errorf("非流式响应泄漏块体：%s", body)
			}
			if !strings.Contains(string(body), "visible answer") {
				t.Errorf("正文被误删：%s", body)
			}
		})
	}
}

// 请求方向：历史里的不透明块投给外族上游时不得被塞进任何槽位。塞进 tool 结果
// 等于伪造一次外族上游从没发起过的工具调用，下一轮必被拒。
func TestOpaqueNotLeakedIntoForeignRequest(t *testing.T) {
	req := &ir.Request{Model: "m", MaxTokens: 16, Messages: []ir.Message{
		{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}},
		{Role: ir.RoleAssistant, Content: []ir.Block{r91Block(), {Type: ir.BlockText, Text: "answer"}}},
		{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "go on"}}},
	}}
	for _, name := range []string{"codex", "openai-chat", "openai-responses"} {
		t.Run(name, func(t *testing.T) {
			body, err := proto.MustOutbound(name).EncodeRequest(req)
			if err != nil {
				t.Fatalf("EncodeRequest: %v", err)
			}
			if strings.Contains(string(body), r91Secret) || strings.Contains(string(body), r91WireType) {
				t.Errorf("出站请求泄漏块体：%s", body)
			}
			if !strings.Contains(string(body), "answer") {
				t.Errorf("同一条消息的正文被误删：%s", body)
			}
		})
	}
}
