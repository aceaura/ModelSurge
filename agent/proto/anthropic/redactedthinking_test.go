package anthropic

import (
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// R90：anthropic redacted_thinking 内容块全链路贯通。块形
// {type:"redacted_thinking", data}：被安全系统涂抹掉的思考，载荷只有一段不透明
// 密文，没有明文也没有签名。官方 SDK 对照：messages.ts RedactedThinkingBlock
// （响应 ContentBlock 联合与流式块联合）与 RedactedThinkingBlockParam（请求
// ContentBlockParam 联合）。
// 独立块型而非并入 BlockThinking：两者的回传契约相反——thinking 块要过签名校验、
// 外族签名一律降级，redacted_thinking 没有签名可言且 Anthropic 要求原样回传，
// 任何改写（含降级成空文本块）都会让下一轮请求被拒。

// r90Cipher 形态取自官方文档示例：base64 密文，含 + / = 三类特殊字符，
// 用来顺带钉住「不做任何转义或裁剪」。
const r90Cipher = "EmwKAhgBEgy3va3pzix/LafPsn4aDFIT2Xlxh0L5L8rLVyIwxtE3rAFBa8cwF4LHqJo="

func r90Block() ir.Block {
	return ir.Block{Type: ir.BlockRedactedThinking, RedactedData: r90Cipher}
}

// ---- 请求侧：多轮历史里的涂抹块必须原样回到上游 ----

func TestRedactedThinkingDecodeRequest(t *testing.T) {
	body := `{"model":"claude-x","max_tokens":16,"messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":[
			{"type":"redacted_thinking","data":"` + r90Cipher + `"},
			{"type":"text","text":"answer"}]},
		{"role":"user","content":"go on"}]}`
	r, err := codec{}.DecodeRequest([]byte(body))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	blocks := r.Messages[1].Content
	if len(blocks) != 2 {
		t.Fatalf("块数 = %d，want 2", len(blocks))
	}
	b := blocks[0]
	if b.Type != ir.BlockRedactedThinking {
		t.Fatalf("块型 = %q，want redacted_thinking", b.Type)
	}
	if b.RedactedData != r90Cipher {
		t.Errorf("密文被改动：\n got %q\nwant %q", b.RedactedData, r90Cipher)
	}
	// 不得被误认成 thinking 块：那会让 degradeThinking 把它降级成文本。
	if b.Thinking != nil {
		t.Errorf("涂抹块不应带 thinking 载荷：%+v", b.Thinking)
	}
}

func TestRedactedThinkingEncodeRequestRoundTrip(t *testing.T) {
	req := &ir.Request{Model: "claude-x", MaxTokens: 16, Messages: []ir.Message{
		{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}},
		{Role: ir.RoleAssistant, Content: []ir.Block{r90Block(), {Type: ir.BlockText, Text: "answer"}}},
		{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "go on"}}},
	}}
	out, err := codec{}.EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, `"type":"redacted_thinking"`) {
		t.Fatalf("块型丢失：%s", s)
	}
	if !strings.Contains(s, `"data":"`+r90Cipher+`"`) {
		t.Errorf("密文未逐字回写：%s", s)
	}
	// 降级形态是一个 text 字段被 omitempty 吃掉的裸文本块：客户端下一轮无从回传，
	// Anthropic 的续话校验直接拒整个请求。
	if strings.Contains(s, `"type":"text"}`) {
		t.Errorf("涂抹块被降级成空文本块：%s", s)
	}
	back, err := codec{}.DecodeRequest(out)
	if err != nil {
		t.Fatalf("DecodeRequest(往返): %v", err)
	}
	got := back.Messages[1].Content[0]
	if got.Type != ir.BlockRedactedThinking || got.RedactedData != r90Cipher {
		t.Errorf("同族往返漂移：%+v", got)
	}
}

// 签名降级只该碰 thinking 块：外族签名的 thinking 降级成文本是刻意的
// （透传必 400），涂抹块没有签名可言，一起降级等于销毁加密续话状态。
func TestRedactedThinkingSurvivesDegradeThinking(t *testing.T) {
	req := &ir.Request{Model: "claude-x", MaxTokens: 16, Messages: []ir.Message{
		{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}},
		{Role: ir.RoleAssistant, Content: []ir.Block{
			{Type: ir.BlockThinking, Thinking: &ir.Thinking{
				Text: "foreign reasoning", Signature: "sig-from-chat",
				SignatureFrom: ir.SigFrom("openai-chat", "sig-from-chat"),
			}},
			r90Block(),
		}},
		{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "go on"}}},
	}}
	out, err := codec{}.EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	s := string(out)
	if strings.Contains(s, "sig-from-chat") {
		t.Errorf("外族签名不该透传：%s", s)
	}
	if !strings.Contains(s, "foreign reasoning") {
		t.Errorf("夹具无效：thinking 块本应降级成文本保留明文：%s", s)
	}
	if !strings.Contains(s, `"type":"redacted_thinking"`) || !strings.Contains(s, `"data":"`+r90Cipher+`"`) {
		t.Errorf("涂抹块被签名降级一并吃掉：%s", s)
	}
}

// 只有涂抹块的 assistant 消息不得被塞占位文本：normalize 把非 text 块算作
// 有内容，塞了占位就会在历史里凭空多出一句模型没说过的话。
func TestRedactedThinkingOnlyMessageGetsNoPlaceholder(t *testing.T) {
	req := &ir.Request{Model: "claude-x", MaxTokens: 16, Messages: []ir.Message{
		{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}},
		{Role: ir.RoleAssistant, Content: []ir.Block{r90Block()}},
		{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "go on"}}},
	}}
	out, err := codec{}.EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if strings.Count(string(out), `"type":"text"}`) != 0 {
		t.Errorf("凭空多出空文本块：%s", out)
	}
	if !strings.Contains(string(out), `"data":"`+r90Cipher+`"`) {
		t.Errorf("密文丢失：%s", out)
	}
}

// 上游没给 data 时不得凭空造一段：空值原样收原样发，块型保住即可。
func TestRedactedThinkingEmptyDataNotInvented(t *testing.T) {
	body := `{"model":"claude-x","max_tokens":16,"messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":[{"type":"redacted_thinking"}]},
		{"role":"user","content":"go on"}]}`
	r, err := codec{}.DecodeRequest([]byte(body))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	b := r.Messages[1].Content[0]
	if b.Type != ir.BlockRedactedThinking || b.RedactedData != "" {
		t.Fatalf("块 = %+v", b)
	}
	out, err := codec{}.EncodeRequest(r)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if !strings.Contains(string(out), `"type":"redacted_thinking"`) {
		t.Errorf("块型丢失：%s", out)
	}
	if strings.Contains(string(out), `"data"`) {
		t.Errorf("不该造出 data 字段：%s", out)
	}
}

// ---- 响应侧 ----

func TestRedactedThinkingResponseRoundTrip(t *testing.T) {
	body := `{"id":"msg_1","type":"message","role":"assistant","model":"claude-x",
		"content":[{"type":"redacted_thinking","data":"` + r90Cipher + `"},
		{"type":"text","text":"answer"}],
		"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
	resp, err := codec{}.DecodeResponse([]byte(body))
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if len(resp.Content) != 2 {
		t.Fatalf("块数 = %d", len(resp.Content))
	}
	b := resp.Content[0]
	if b.Type != ir.BlockRedactedThinking || b.RedactedData != r90Cipher {
		t.Fatalf("响应块 = %+v", b)
	}
	out, err := codec{}.EncodeResponse(resp)
	if err != nil {
		t.Fatalf("EncodeResponse: %v", err)
	}
	if !strings.Contains(string(out), `"type":"redacted_thinking"`) ||
		!strings.Contains(string(out), `"data":"`+r90Cipher+`"`) {
		t.Errorf("响应回写错：%s", out)
	}
	if strings.Contains(string(out), `"type":"text"}`) {
		t.Errorf("响应侧被降级成空文本块：%s", out)
	}
}

// ---- 流式 ----

// 涂抹块没有增量形态：密文随 content_block_start 全量下发，与
// web_search_tool_result 的 content 同一处置。
func TestRedactedThinkingStreamDecode(t *testing.T) {
	d := codec{}.NewStreamDecoder()
	if _, err := d.Feed("message_start", `{"type":"message_start","message":{"id":"msg_1","model":"claude-x","usage":{"input_tokens":1,"output_tokens":1}}}`); err != nil {
		t.Fatalf("Feed start: %v", err)
	}
	evs, err := d.Feed("content_block_start",
		`{"type":"content_block_start","index":0,"content_block":{"type":"redacted_thinking","data":"`+r90Cipher+`"}}`)
	if err != nil || len(evs) != 1 {
		t.Fatalf("Feed block_start: %v %d", err, len(evs))
	}
	ev := evs[0]
	if ev.Type != ir.EvBlockStart || ev.Block == nil {
		t.Fatalf("事件 = %+v", ev)
	}
	if ev.Block.Type != ir.BlockRedactedThinking || ev.Block.RedactedData != r90Cipher {
		t.Fatalf("块 = %+v", ev.Block)
	}
	if _, err := d.Feed("content_block_stop", `{"type":"content_block_stop","index":0}`); err != nil {
		t.Fatalf("Feed block_stop: %v", err)
	}
	// 聚合：非流式回退与重试判定都读聚合结果，密文在这里丢了等于全丢。
	a := ir.NewAggregator()
	a.Feed(ir.Event{Type: ir.EvMessageStart, MessageID: "msg_1"})
	a.Feed(ev)
	a.Feed(ir.Event{Type: ir.EvBlockStop, Index: 0})
	a.Feed(ir.Event{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn})
	got, _ := a.Finish()
	if len(got.Content) != 1 {
		t.Fatalf("聚合块数 = %d", len(got.Content))
	}
	if got.Content[0].Type != ir.BlockRedactedThinking || got.Content[0].RedactedData != r90Cipher {
		t.Errorf("聚合丢失密文：%+v", got.Content[0])
	}
}

func TestRedactedThinkingStreamEncode(t *testing.T) {
	e := codec{}.NewStreamEncoder()
	if _, err := e.Encode(ir.Event{Type: ir.EvMessageStart, MessageID: "msg_1", Model: "claude-x"}); err != nil {
		t.Fatalf("Encode start: %v", err)
	}
	b := r90Block()
	frames, err := e.Encode(ir.Event{Type: ir.EvBlockStart, Index: 0, Block: &b})
	if err != nil || len(frames) != 1 {
		t.Fatalf("Encode block_start: %v %d", err, len(frames))
	}
	f := string(frames[0])
	if !strings.Contains(f, `"content_block":{"type":"redacted_thinking"`) {
		t.Fatalf("block_start 帧块型错：%s", f)
	}
	if !strings.Contains(f, `"data":"`+r90Cipher+`"`) {
		t.Errorf("block_start 帧丢密文：%s", f)
	}
	frames, err = e.Encode(ir.Event{Type: ir.EvBlockStop, Index: 0})
	if err != nil || len(frames) != 1 {
		t.Fatalf("Encode block_stop: %v %d", err, len(frames))
	}
	if !strings.Contains(string(frames[0]), `"type":"content_block_stop"`) {
		t.Errorf("block_stop 帧：%s", frames[0])
	}
	// 自家协议不报损耗。
	if notes := e.Notes(); len(notes) != 0 {
		t.Errorf("anthropic 误报损耗：%v", notes)
	}
}
