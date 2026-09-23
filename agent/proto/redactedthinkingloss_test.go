package proto_test

import (
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
	"github.com/aceaura/ModelSurge/agent/proto"
	_ "github.com/aceaura/ModelSurge/agent/proto/anthropic"
	_ "github.com/aceaura/ModelSurge/agent/proto/gemini"
	_ "github.com/aceaura/ModelSurge/agent/proto/kiro"
	_ "github.com/aceaura/ModelSurge/agent/proto/openaichat"
	_ "github.com/aceaura/ModelSurge/agent/proto/openairesponses"
)

// R90：redacted_thinking 块的跨族损耗注记与跳过安全性。该块只有 anthropic
// 一族有槽位：密文由 Anthropic 加密与解密，外族既没有承载不透明推理状态的字段，
// 也不能据此恢复思考链。整块跳过不降级（密文拼进正文会污染回答，塞进
// reasoning.encrypted_content / thoughtSignature 则是伪造别家的密文槽位），
// 损耗经 ResponseNotes / 流式 Notes() 报出。密文本身属会话内容，不进注记。

const r90Cipher = "EmwKAhgBEgy3va3pzix/LafPsn4aDFIT2Xlxh0L5L8rLVyIwxtE3rAFBa8cwF4LHqJo="

func r90Block() ir.Block {
	return ir.Block{Type: ir.BlockRedactedThinking, RedactedData: r90Cipher}
}

func r90Stream() []ir.Event {
	return []ir.Event{
		{Type: ir.EvMessageStart, MessageID: "m", Model: "m"},
		{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockRedactedThinking, RedactedData: r90Cipher}},
		{Type: ir.EvBlockStop, Index: 0},
		{Type: ir.EvBlockStart, Index: 1, Block: &ir.Block{Type: ir.BlockText}},
		{Type: ir.EvTextDelta, Index: 1, Text: "visible answer"},
		{Type: ir.EvBlockStop, Index: 1},
		{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn, Usage: &ir.Usage{OutputTokens: 3}},
		{Type: ir.EvMessageStop},
	}
}

// 外族流式：密文不出现在线上，正文不受影响。
func TestRedactedThinkingStreamForeignSkip(t *testing.T) {
	for _, name := range []string{"openai-chat", "openai-responses", "gemini"} {
		t.Run(name, func(t *testing.T) {
			out := streamOut(t, name, r90Stream())
			if strings.Contains(out, r90Cipher) {
				t.Errorf("密文泄漏进流：\n%s", out)
			}
			if strings.Contains(out, "redacted_thinking") {
				t.Errorf("块名泄漏进流：\n%s", out)
			}
			if !strings.Contains(out, "visible answer") {
				t.Errorf("正文被误删：\n%s", out)
			}
		})
	}
}

func TestRedactedThinkingStreamForeignNotes(t *testing.T) {
	for _, name := range []string{"openai-chat", "openai-responses", "gemini"} {
		t.Run(name, func(t *testing.T) {
			e := proto.MustInbound(name).NewStreamEncoder()
			for _, ev := range r90Stream() {
				if _, err := e.Encode(ev); err != nil {
					t.Fatalf("Encode(%s): %v", ev.Type, err)
				}
			}
			got := strings.Join(e.Notes(), "; ")
			if !strings.Contains(got, "dropped 1 redacted thinking block(s)") {
				t.Errorf("应报 1 块丢失：%q", got)
			}
			if strings.Contains(got, r90Cipher) {
				t.Errorf("注记抄出密文：%q", got)
			}
			if again := e.Notes(); len(again) != 0 {
				t.Errorf("Notes 应幂等排干：%v", again)
			}
		})
	}
	// 多块计数聚合为一条。
	e := proto.MustInbound("openai-chat").NewStreamEncoder()
	e.Encode(ir.Event{Type: ir.EvMessageStart, MessageID: "m", Model: "m"})
	b0, b1 := r90Block(), r90Block()
	e.Encode(ir.Event{Type: ir.EvBlockStart, Index: 0, Block: &b0})
	e.Encode(ir.Event{Type: ir.EvBlockStop, Index: 0})
	e.Encode(ir.Event{Type: ir.EvBlockStart, Index: 1, Block: &b1})
	e.Encode(ir.Event{Type: ir.EvBlockStop, Index: 1})
	e.Encode(ir.Event{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn})
	got := strings.Join(e.Notes(), "; ")
	if !strings.Contains(got, "dropped 2 redacted thinking block(s)") {
		t.Errorf("多块应计数：%q", got)
	}
	// anthropic 自家 encoder 不报。
	ea := proto.MustInbound("anthropic").NewStreamEncoder()
	for _, ev := range r90Stream() {
		ea.Encode(ev)
	}
	if notes := ea.Notes(); len(notes) != 0 {
		t.Errorf("anthropic 误报：%v", notes)
	}
}

// responses 的 blockStart 有 default(text) 兜底：不显式拦住涂抹块，客户端会
// 凭空多出一个空 output_text 条目——看起来像模型说了一句空话。
func TestRedactedThinkingAbsentFromResponsesFullOutput(t *testing.T) {
	out := streamOut(t, "openai-responses", r90Stream())
	idx := strings.LastIndex(out, `"type":"response.completed"`)
	if idx < 0 {
		t.Fatalf("没有 response.completed：\n%s", out)
	}
	if final := out[idx:]; strings.Contains(final, r90Cipher) {
		t.Errorf("全量 output 泄漏密文：\n%s", final)
	}
	if n := strings.Count(out, `"type":"response.output_item.added"`); n != 1 {
		t.Errorf("output_item.added 应只有正文一条，实得 %d（多出的是幽灵空文本条目）：\n%s", n, out)
	}
	if strings.Contains(out, `"type":"response.output_text.done","text":""`) {
		t.Errorf("凭空多出空 output_text：\n%s", out)
	}
}

// chat 侧同理：被跳过的块不得留下任何分片。整条流应只剩「开场 role + 正文 +
// 终止」三个 chunk；多出来的那一个就是涂抹块漏出的空 content 分片。
func TestRedactedThinkingNoGhostChatDelta(t *testing.T) {
	out := streamOut(t, "openai-chat", r90Stream())
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

// 非流式响应侧：ScanResponseLosses 报出，anthropic 自家静默，无块全静默。
func TestRedactedThinkingResponseNotes(t *testing.T) {
	resp := &ir.Response{ID: "m", Model: "m", Content: []ir.Block{
		r90Block(), {Type: ir.BlockText, Text: "visible answer"}}}
	for _, name := range []string{"openai-chat", "openai-responses", "gemini"} {
		got := strings.Join(proto.MustInbound(name).ResponseNotes(resp), "; ")
		if !strings.Contains(got, "dropped 1 redacted thinking block(s)") {
			t.Errorf("%s 应报块丢失：%q", name, got)
		}
		if strings.Contains(got, r90Cipher) {
			t.Errorf("%s 注记抄出密文：%q", name, got)
		}
	}
	if notes := proto.MustInbound("anthropic").ResponseNotes(resp); len(notes) != 0 {
		t.Errorf("anthropic 误报：%v", notes)
	}
	resp.Content = resp.Content[1:]
	for _, name := range []string{"anthropic", "openai-chat", "openai-responses", "gemini"} {
		if notes := proto.MustInbound(name).ResponseNotes(resp); len(notes) != 0 {
			t.Errorf("%s 无块误报：%v", name, notes)
		}
	}
}

// 非流式编码：外族线上不出现密文，正文保留。
func TestRedactedThinkingNotLeakedIntoResponse(t *testing.T) {
	resp := &ir.Response{ID: "m", Model: "m", StopReason: ir.StopEndTurn,
		Content: []ir.Block{r90Block(), {Type: ir.BlockText, Text: "visible answer"}}}
	for _, name := range []string{"openai-chat", "openai-responses", "gemini"} {
		t.Run(name, func(t *testing.T) {
			body, err := proto.MustInbound(name).EncodeResponse(resp)
			if err != nil {
				t.Fatalf("EncodeResponse: %v", err)
			}
			if strings.Contains(string(body), r90Cipher) || strings.Contains(string(body), "redacted_thinking") {
				t.Errorf("非流式响应泄漏密文：%s", body)
			}
			if !strings.Contains(string(body), "visible answer") {
				t.Errorf("正文被误删：%s", body)
			}
		})
	}
}

// 请求方向：历史里的涂抹块投给外族上游时，密文不得被塞进任何槽位
// （reasoning.encrypted_content / thoughtSignature 都是别家的密文，伪造过去
// 下一轮必被拒）。
func TestRedactedThinkingNotLeakedIntoForeignRequest(t *testing.T) {
	req := &ir.Request{Model: "m", MaxTokens: 16, Messages: []ir.Message{
		{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}},
		{Role: ir.RoleAssistant, Content: []ir.Block{r90Block(), {Type: ir.BlockText, Text: "answer"}}},
		{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "go on"}}},
	}}
	for _, name := range proto.OutboundNames() {
		if name == "anthropic" {
			continue
		}
		t.Run(name, func(t *testing.T) {
			body, err := proto.MustOutbound(name).EncodeRequest(req)
			if err != nil {
				t.Fatalf("EncodeRequest: %v", err)
			}
			if strings.Contains(string(body), r90Cipher) {
				t.Errorf("出站请求泄漏密文：%s", body)
			}
			if !strings.Contains(string(body), "answer") {
				t.Errorf("同一条消息的正文被误删：%s", body)
			}
		})
	}
}
