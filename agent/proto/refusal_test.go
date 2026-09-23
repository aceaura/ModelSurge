package proto_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
	"github.com/aceaura/ModelSurge/agent/proto"
	_ "github.com/aceaura/ModelSurge/agent/proto/anthropic"
	_ "github.com/aceaura/ModelSurge/agent/proto/gemini"
	_ "github.com/aceaura/ModelSurge/agent/proto/openaichat"
	_ "github.com/aceaura/ModelSurge/agent/proto/openairesponses"
)

// 模型拒绝作答时，正文在 OpenAI 两系走独立字段（Chat 的 message.refusal、
// Responses 的 refusal content part / response.refusal.delta），此前一概不读，
// 客户端拿到的是 stop_reason=refusal 配一条空消息——看起来像成功的空回复。
// 这里的断言一律直查 wire 或 IR 块，不靠往返相等：两侧同时漏掉某个字段时
// 往返照样成立。

const refusalText = "I can't help with that."

func refusalResp() *ir.Response {
	return &ir.Response{
		ID: "r1", Model: "m",
		Content:    []ir.Block{{Type: ir.BlockRefusal, Text: refusalText}},
		StopReason: ir.StopRefusal,
	}
}

func decodeRespOf(t *testing.T, name, body string) *ir.Response {
	t.Helper()
	resp, err := proto.MustOutbound(name).DecodeResponse([]byte(body))
	if err != nil {
		t.Fatalf("DecodeResponse(%s): %v", name, err)
	}
	return resp
}

func encodeRespOf(t *testing.T, name string, resp *ir.Response) string {
	t.Helper()
	// EncodeResponse 挂在入站侧：它渲染的是给客户端看的响应。
	body, err := proto.MustInbound(name).EncodeResponse(resp)
	if err != nil {
		t.Fatalf("EncodeResponse(%s): %v", name, err)
	}
	if !json.Valid(body) {
		t.Fatalf("%s 产出非法 JSON：%s", name, body)
	}
	return string(body)
}

func onlyRefusalBlock(t *testing.T, resp *ir.Response) ir.Block {
	t.Helper()
	var out []ir.Block
	for _, b := range resp.Content {
		if b.Type == ir.BlockRefusal {
			out = append(out, b)
		}
	}
	if len(out) != 1 {
		t.Fatalf("拒绝块数量 = %d, want 1：%+v", len(out), resp.Content)
	}
	return out[0]
}

// ---- 解码：上游的拒绝必须进 IR ----

func TestChatDecodesRefusal(t *testing.T) {
	body := `{"id":"c1","model":"m","choices":[{"index":0,"finish_reason":"content_filter",
		"message":{"role":"assistant","content":null,"refusal":"` + refusalText + `"}}]}`
	resp := decodeRespOf(t, "openai-chat", body)
	if got := onlyRefusalBlock(t, resp).Text; got != refusalText {
		t.Errorf("拒绝正文 = %q, want %q", got, refusalText)
	}
	if resp.StopReason != ir.StopRefusal {
		t.Errorf("StopReason = %q, want refusal", resp.StopReason)
	}
}

func TestResponsesDecodesRefusalPart(t *testing.T) {
	body := `{"id":"resp_1","model":"m","status":"incomplete",
		"incomplete_details":{"reason":"content_filter"},
		"output":[{"type":"message","role":"assistant",
		"content":[{"type":"refusal","refusal":"` + refusalText + `"}]}]}`
	resp := decodeRespOf(t, "openai-responses", body)
	if got := onlyRefusalBlock(t, resp).Text; got != refusalText {
		t.Errorf("拒绝正文 = %q, want %q", got, refusalText)
	}
}

// 拒绝不得被读成普通文本：若解码时归成 BlockText，客户端会把它当模型的正常
// 回答渲染，stop_reason 也救不回来（很多客户端只读 content）。
func TestRefusalIsNotDecodedAsText(t *testing.T) {
	for _, c := range []struct{ name, body string }{
		{"openai-chat", `{"id":"c1","model":"m","choices":[{"index":0,
			"message":{"role":"assistant","refusal":"` + refusalText + `"}}]}`},
		{"openai-responses", `{"id":"r1","model":"m","output":[{"type":"message","role":"assistant",
			"content":[{"type":"refusal","refusal":"` + refusalText + `"}]}]}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			resp := decodeRespOf(t, c.name, c.body)
			for _, b := range resp.Content {
				if b.Type == ir.BlockText && b.Text == refusalText {
					t.Error("拒绝正文被读成普通文本块")
				}
			}
			onlyRefusalBlock(t, resp) // 且确实存在拒绝块
		})
	}
}

// 拒绝与正文并存：官方允许 content 有内容同时给出 refusal，两者都要留。
func TestChatKeepsBothTextAndRefusal(t *testing.T) {
	body := `{"id":"c1","model":"m","choices":[{"index":0,
		"message":{"role":"assistant","content":"partial answer","refusal":"` + refusalText + `"}}]}`
	resp := decodeRespOf(t, "openai-chat", body)
	if got := onlyRefusalBlock(t, resp).Text; got != refusalText {
		t.Errorf("拒绝正文 = %q", got)
	}
	var sawText bool
	for _, b := range resp.Content {
		if b.Type == ir.BlockText && b.Text == "partial answer" {
			sawText = true
		}
	}
	if !sawText {
		t.Errorf("同时给出的普通正文丢了：%+v", resp.Content)
	}
}

// 空 refusal 不得凭空造块：多数响应里这个字段是 null/空串，造块会让每条
// 正常回复都多出一个空拒绝块，下游可能据此误判成拒绝。
func TestEmptyRefusalMakesNoBlock(t *testing.T) {
	for _, c := range []struct{ name, body string }{
		{"openai-chat", `{"id":"c1","model":"m","choices":[{"index":0,
			"message":{"role":"assistant","content":"hi","refusal":null}}]}`},
		{"openai-responses", `{"id":"r1","model":"m","output":[{"type":"message","role":"assistant",
			"content":[{"type":"output_text","text":"hi"}]}]}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			resp := decodeRespOf(t, c.name, c.body)
			for _, b := range resp.Content {
				if b.Type == ir.BlockRefusal {
					t.Errorf("凭空造了拒绝块：%+v", b)
				}
			}
		})
	}
}

// ---- 编码：有槽位的写进槽位 ----

func TestOutboundWritesRefusalIntoOwnSlot(t *testing.T) {
	for _, c := range []struct{ name, wantKey string }{
		{"openai-chat", `"refusal":"` + refusalText + `"`},
		{"openai-responses", `"type":"refusal"`},
	} {
		t.Run(c.name, func(t *testing.T) {
			wire := encodeRespOf(t, c.name, refusalResp())
			if !strings.Contains(wire, c.wantKey) {
				t.Errorf("拒绝没落进专属槽位（缺 %s）：\n%s", c.wantKey, wire)
			}
			if !strings.Contains(wire, refusalText) {
				t.Errorf("拒绝正文整条丢了：\n%s", wire)
			}
		})
	}
}

// Chat 的拒绝不得同时写进 content：客户端读 content 会把拒绝当回答，
// 而 refusal 槽位里那份就成了重复内容。
func TestChatRefusalNotDuplicatedIntoContent(t *testing.T) {
	wire := encodeRespOf(t, "openai-chat", refusalResp())
	var got struct {
		Choices []struct {
			Message struct {
				Content any    `json:"content"`
				Refusal string `json:"refusal"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(wire), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Choices) != 1 {
		t.Fatalf("choices = %d", len(got.Choices))
	}
	if got.Choices[0].Message.Refusal != refusalText {
		t.Errorf("refusal = %q", got.Choices[0].Message.Refusal)
	}
	if s, ok := got.Choices[0].Message.Content.(string); ok && s != "" {
		t.Errorf("拒绝同时被写进 content：%q", s)
	}
}

// ---- 编码：无槽位的降级为文本，不得丢 ----

func TestRefusalDegradesToTextWhereNoSlot(t *testing.T) {
	// anthropic 只有 stop_reason 能表达「这是拒绝」，正文只能并入文本。
	// 丢弃会让客户端看到一条空消息配 stop_reason=refusal。
	for _, name := range []string{"anthropic", "gemini"} {
		t.Run(name, func(t *testing.T) {
			if wire := encodeRespOf(t, name, refusalResp()); !strings.Contains(wire, refusalText) {
				t.Errorf("降级后拒绝正文消失：\n%s", wire)
			}
		})
	}
}

// 降级不得加标注前缀：正文会被模型在后续轮次里读到，注入的说明文字会变成
// 模型自己说过的话。
func TestRefusalDegradationAddsNoPrefix(t *testing.T) {
	wire := encodeRespOf(t, "anthropic", refusalResp())
	var got struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal([]byte(wire), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Content) != 1 {
		t.Fatalf("块数 = %d, want 1：%s", len(got.Content), wire)
	}
	if got.Content[0].Type != "text" {
		t.Errorf("块类型 = %q, want text", got.Content[0].Type)
	}
	if got.Content[0].Text != refusalText {
		t.Errorf("正文被改写成 %q, want %q", got.Content[0].Text, refusalText)
	}
}

// ---- 跨协议端到端 ----

// chat 上游拒绝 -> anthropic 客户端：正文必须仍在，且 stop_reason 仍是 refusal。
// 这条最贴近真实故障：拒绝正文没了，客户端只能看到一个空消息。
func TestChatRefusalReachesAnthropicWithText(t *testing.T) {
	upstream := `{"id":"c1","model":"m","choices":[{"index":0,"finish_reason":"content_filter",
		"message":{"role":"assistant","content":null,"refusal":"` + refusalText + `"}}]}`
	resp := decodeRespOf(t, "openai-chat", upstream)
	wire := encodeRespOf(t, "anthropic", resp)
	if !strings.Contains(wire, refusalText) {
		t.Errorf("跨协议后拒绝正文丢失：\n%s", wire)
	}
	if !strings.Contains(wire, `"stop_reason":"refusal"`) {
		t.Errorf("stop_reason 没保住：\n%s", wire)
	}
}

// 同协议往返：Chat -> IR -> Chat 必须仍在专属槽位，不能退化成普通文本。
// 本仓没有透传快路径，同协议也真的过一遍 IR，所以这条会真的测到东西。
func TestRefusalRoundTripsSameProtocol(t *testing.T) {
	for _, c := range []struct{ name, body string }{
		{"openai-chat", `{"id":"c1","model":"m","choices":[{"index":0,
			"message":{"role":"assistant","refusal":"` + refusalText + `"}}]}`},
		{"openai-responses", `{"id":"r1","model":"m","output":[{"type":"message","role":"assistant",
			"content":[{"type":"refusal","refusal":"` + refusalText + `"}]}]}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			resp := decodeRespOf(t, c.name, c.body)
			wire := encodeRespOf(t, c.name, resp)
			again := decodeRespOf(t, c.name, wire)
			if got := onlyRefusalBlock(t, again).Text; got != refusalText {
				t.Errorf("往返后拒绝正文 = %q, want %q\n%s", got, refusalText, wire)
			}
		})
	}
}

// responses 上游拒绝 -> chat 客户端：槽位换了名字但语义必须保住。
func TestResponsesRefusalReachesChatSlot(t *testing.T) {
	upstream := `{"id":"r1","model":"m","output":[{"type":"message","role":"assistant",
		"content":[{"type":"refusal","refusal":"` + refusalText + `"}]}]}`
	resp := decodeRespOf(t, "openai-responses", upstream)
	wire := encodeRespOf(t, "openai-chat", resp)
	if !strings.Contains(wire, `"refusal":"`+refusalText+`"`) {
		t.Errorf("拒绝没进 Chat 的 refusal 槽位：\n%s", wire)
	}
}

// ---- 流式 ----

func TestChatStreamDecodesRefusalDelta(t *testing.T) {
	d := proto.MustOutbound("openai-chat").NewStreamDecoder()
	evs, err := d.Feed("", `{"id":"c1","model":"m","choices":[{"index":0,
		"delta":{"role":"assistant","refusal":"I can't"}}]}`)
	if err != nil {
		t.Fatal(err)
	}
	more, err := d.Feed("", `{"id":"c1","model":"m","choices":[{"index":0,"delta":{"refusal":" help."}}]}`)
	if err != nil {
		t.Fatal(err)
	}
	evs = append(evs, more...)

	var refusalBlockIdx = -1
	for _, ev := range evs {
		if ev.Type == ir.EvBlockStart && ev.Block != nil && ev.Block.Type == ir.BlockRefusal {
			refusalBlockIdx = ev.Index
		}
	}
	if refusalBlockIdx == -1 {
		t.Fatalf("没开出拒绝块：%+v", evs)
	}
	var text string
	for _, ev := range evs {
		if ev.Type == ir.EvTextDelta && ev.Index == refusalBlockIdx {
			text += ev.Text
		}
	}
	if text != "I can't help." {
		t.Errorf("拼出的拒绝正文 = %q", text)
	}
}

func TestResponsesStreamDecodesRefusalDelta(t *testing.T) {
	d := proto.MustOutbound("openai-responses").NewStreamDecoder()
	var evs []ir.Event
	for _, frame := range []string{
		`{"type":"response.created","response":{"id":"r1","model":"m"}}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_1"}}`,
		`{"type":"response.refusal.delta","output_index":0,"delta":"I can't"}`,
		`{"type":"response.refusal.delta","output_index":0,"delta":" help."}`,
		`{"type":"response.refusal.done","output_index":0}`,
	} {
		got, err := d.Feed("", frame)
		if err != nil {
			t.Fatalf("Feed(%s): %v", frame, err)
		}
		evs = append(evs, got...)
	}
	idx := -1
	for _, ev := range evs {
		if ev.Type == ir.EvBlockStart && ev.Block != nil && ev.Block.Type == ir.BlockRefusal {
			idx = ev.Index
		}
	}
	if idx == -1 {
		t.Fatalf("没开出拒绝块：%+v", evs)
	}
	var text string
	var closed bool
	for _, ev := range evs {
		switch {
		case ev.Type == ir.EvTextDelta && ev.Index == idx:
			text += ev.Text
		case ev.Type == ir.EvBlockStop && ev.Index == idx:
			closed = true
		}
	}
	if text != "I can't help." {
		t.Errorf("拼出的拒绝正文 = %q", text)
	}
	if !closed {
		t.Error("refusal.done 没有关块：下游编码器会认为块仍开着")
	}
}

// 拒绝块序号不得与同一条 message 的文本块相撞：撞了会让拒绝正文被并进文本块，
// 客户端又分不出来了。
func TestResponsesStreamRefusalUsesDistinctBlock(t *testing.T) {
	d := proto.MustOutbound("openai-responses").NewStreamDecoder()
	var evs []ir.Event
	for _, frame := range []string{
		`{"type":"response.created","response":{"id":"r1","model":"m"}}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_1"}}`,
		`{"type":"response.output_text.delta","output_index":0,"delta":"partial"}`,
		`{"type":"response.refusal.delta","output_index":0,"delta":"nope"}`,
	} {
		got, err := d.Feed("", frame)
		if err != nil {
			t.Fatal(err)
		}
		evs = append(evs, got...)
	}
	textIdx, refIdx := -1, -1
	for _, ev := range evs {
		if ev.Type == ir.EvBlockStart && ev.Block != nil {
			switch ev.Block.Type {
			case ir.BlockText:
				textIdx = ev.Index
			case ir.BlockRefusal:
				refIdx = ev.Index
			}
		}
	}
	if refIdx == -1 {
		t.Fatalf("没开出拒绝块：%+v", evs)
	}
	if textIdx == refIdx {
		t.Errorf("拒绝块与文本块撞在同一序号 %d", refIdx)
	}
}

// 未收到 refusal.done 就终止时必须补关块，否则拒绝正文卡在下游缓冲里发不出去。
func TestResponsesStreamClosesRefusalOnTerminate(t *testing.T) {
	d := proto.MustOutbound("openai-responses").NewStreamDecoder()
	var evs []ir.Event
	for _, frame := range []string{
		`{"type":"response.created","response":{"id":"r1","model":"m"}}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_1"}}`,
		`{"type":"response.refusal.delta","output_index":0,"delta":"nope"}`,
		`{"type":"response.incomplete","response":{"id":"r1","model":"m","status":"incomplete",
			"incomplete_details":{"reason":"content_filter"}}}`,
	} {
		got, err := d.Feed("", frame)
		if err != nil {
			t.Fatal(err)
		}
		evs = append(evs, got...)
	}
	idx := -1
	for _, ev := range evs {
		if ev.Type == ir.EvBlockStart && ev.Block != nil && ev.Block.Type == ir.BlockRefusal {
			idx = ev.Index
		}
	}
	var closed bool
	for _, ev := range evs {
		if ev.Type == ir.EvBlockStop && ev.Index == idx {
			closed = true
		}
	}
	if !closed {
		t.Errorf("终止时未补关拒绝块：%+v", evs)
	}
}

// 流式出站：拒绝块的增量必须用拒绝专属事件名 / 字段，不能走普通文本通道。
func TestStreamEncodersUseRefusalChannel(t *testing.T) {
	for _, c := range []struct{ name, want, forbid string }{
		{"openai-chat", `"refusal":"` + refusalText + `"`, `"content":"` + refusalText + `"`},
		{"openai-responses", "response.refusal.delta", "response.output_text.delta"},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := proto.MustInbound(c.name).NewStreamEncoder()
			var sb strings.Builder
			for _, ev := range []ir.Event{
				{Type: ir.EvMessageStart, MessageID: "r1", Model: "m"},
				{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockRefusal}},
				{Type: ir.EvTextDelta, Index: 0, Text: refusalText},
				{Type: ir.EvBlockStop, Index: 0},
			} {
				frames, err := e.Encode(ev)
				if err != nil {
					t.Fatalf("Encode(%s): %v", ev.Type, err)
				}
				for _, f := range frames {
					sb.Write(f)
				}
			}
			out := sb.String()
			if !strings.Contains(out, c.want) {
				t.Errorf("缺拒绝专属通道 %q：\n%s", c.want, out)
			}
			if strings.Contains(out, c.forbid) {
				t.Errorf("拒绝走了普通文本通道 %q：\n%s", c.forbid, out)
			}
		})
	}
}

// openai-responses 的流式拒绝不止 delta 一帧：content_part.added 的 part 类型
// 与 output_item.done 的 item 正文同样是客户端渲染依据。只断言 delta 事件名的
// 话，这两处写成 output_text / 空正文都照样全绿——客户端会把拒绝当普通回答
// 渲染，或收到一条空消息。
func TestResponsesStreamRefusalFramesAreComplete(t *testing.T) {
	e := proto.MustInbound("openai-responses").NewStreamEncoder()
	var frames []string
	for _, ev := range []ir.Event{
		{Type: ir.EvMessageStart, MessageID: "r1", Model: "m"},
		{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockRefusal}},
		{Type: ir.EvTextDelta, Index: 0, Text: refusalText},
		{Type: ir.EvBlockStop, Index: 0},
	} {
		out, err := e.Encode(ev)
		if err != nil {
			t.Fatalf("Encode(%s): %v", ev.Type, err)
		}
		for _, f := range out {
			frames = append(frames, string(f))
		}
	}

	var partType, doneContent string
	for _, f := range frames {
		payload := f
		if i := strings.Index(f, "data: "); i >= 0 {
			payload = f[i+len("data: "):]
		}
		var se struct {
			Type string `json:"type"`
			Part *struct {
				Type string `json:"type"`
			} `json:"part"`
			Item *struct {
				Content json.RawMessage `json:"content"`
			} `json:"item"`
		}
		if err := json.Unmarshal([]byte(strings.TrimSpace(payload)), &se); err != nil {
			continue
		}
		switch se.Type {
		case "response.content_part.added":
			if se.Part != nil {
				partType = se.Part.Type
			}
		case "response.output_item.done":
			if se.Item != nil {
				doneContent = string(se.Item.Content)
			}
		}
	}
	if partType != "refusal" {
		t.Errorf("content_part.added 的 part 类型 = %q, want refusal", partType)
	}
	if !strings.Contains(doneContent, refusalText) {
		t.Errorf("output_item.done 里拒绝正文丢了：%s", doneContent)
	}
	if !strings.Contains(doneContent, `"refusal"`) {
		t.Errorf("output_item.done 的 part 类型不是 refusal：%s", doneContent)
	}
}

// 无槽位协议的流式出站同样不得丢正文。
func TestStreamEncodersDegradeRefusalWithoutLoss(t *testing.T) {
	for _, name := range []string{"anthropic", "gemini"} {
		t.Run(name, func(t *testing.T) {
			e := proto.MustInbound(name).NewStreamEncoder()
			var sb strings.Builder
			for _, ev := range []ir.Event{
				{Type: ir.EvMessageStart, MessageID: "r1", Model: "m"},
				{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockRefusal}},
				{Type: ir.EvTextDelta, Index: 0, Text: refusalText},
				{Type: ir.EvBlockStop, Index: 0},
			} {
				frames, err := e.Encode(ev)
				if err != nil {
					t.Fatalf("Encode(%s): %v", ev.Type, err)
				}
				for _, f := range frames {
					sb.Write(f)
				}
			}
			if !strings.Contains(sb.String(), refusalText) {
				t.Errorf("流式降级丢了拒绝正文：\n%s", sb.String())
			}
		})
	}
}

// ---- 聚合路径（上游流式、客户端非流式）----

func TestAggregatorKeepsRefusalText(t *testing.T) {
	a := ir.NewAggregator()
	for _, ev := range []ir.Event{
		{Type: ir.EvMessageStart, MessageID: "r1", Model: "m"},
		{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockRefusal}},
		{Type: ir.EvTextDelta, Index: 0, Text: "I can't"},
		{Type: ir.EvTextDelta, Index: 0, Text: " help."},
		{Type: ir.EvBlockStop, Index: 0},
		{Type: ir.EvMessageDelta, StopReason: ir.StopRefusal},
		{Type: ir.EvMessageStop},
	} {
		a.Feed(ev)
	}
	resp, errOut := a.Finish()
	if errOut != nil {
		t.Fatalf("聚合出错：%v", errOut)
	}
	b := onlyRefusalBlock(t, resp)
	if b.Text != "I can't help." {
		t.Errorf("聚合后拒绝正文 = %q", b.Text)
	}
}

// ---- 请求侧：历史里的拒绝 ----

func refusalHistoryReq() *ir.Request {
	return &ir.Request{Model: "m", MaxTokens: 100, Messages: []ir.Message{
		{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "do X"}}},
		{Role: ir.RoleAssistant, Content: []ir.Block{{Type: ir.BlockRefusal, Text: refusalText}}},
		{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "please"}}},
	}}
}

// 上一轮的拒绝是下一轮的上下文。丢了会让模型看不到自己拒绝过，
// 同样的追问可能直接把它绕过去。
func TestRefusalInHistorySurvivesOutbound(t *testing.T) {
	for _, name := range []string{"anthropic", "openai-chat", "openai-responses"} {
		t.Run(name, func(t *testing.T) {
			wire := string(encodeReq(t, name, refusalHistoryReq()))
			if !strings.Contains(wire, refusalText) {
				t.Errorf("历史里的拒绝整块丢失：\n%s", wire)
			}
		})
	}
}

// Chat 请求侧：历史拒绝要落回 refusal 字段而不是 content，
// 否则上游会把它当成模型的正常回答。
func TestChatHistoryRefusalUsesOwnField(t *testing.T) {
	wire := string(encodeReq(t, "openai-chat", refusalHistoryReq()))
	if !strings.Contains(wire, `"refusal":"`+refusalText+`"`) {
		t.Errorf("历史拒绝没落进 refusal 字段：\n%s", wire)
	}
}

// 入站请求解码：客户端回传的历史拒绝必须进 IR，否则第一跳就丢了。
func TestInboundDecodesHistoryRefusal(t *testing.T) {
	body := `{"model":"m","max_tokens":100,"messages":[
		{"role":"user","content":"do X"},
		{"role":"assistant","refusal":"` + refusalText + `"},
		{"role":"user","content":"please"}]}`
	req, err := proto.MustInbound("openai-chat").DecodeRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	var found string
	for _, m := range req.Messages {
		for _, b := range m.Content {
			if b.Type == ir.BlockRefusal {
				found = b.Text
			}
		}
	}
	if found != refusalText {
		t.Errorf("历史拒绝没进 IR：%+v", req.Messages)
	}
}

// 拒绝块参与 token 计数：漏计会低估上下文占用，客户端据此判断还能塞多少，
// 估少了直接超限。relay/autocompact.go 允许出站自带 tokenizer 覆盖这条估算，
// 目前没有出站这么做，通用估算是唯一一条路径。
func TestRefusalCountsTowardTokens(t *testing.T) {
	est := ir.EstimateRequestTokens
	bare := &ir.Request{Model: "m", MaxTokens: 100, Messages: []ir.Message{
		{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "do X"}}},
		{Role: ir.RoleAssistant, Content: []ir.Block{{Type: ir.BlockText, Text: ""}}},
		{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "please"}}},
	}}
	with, without := est(refusalHistoryReq()), est(bare)
	if with <= without {
		t.Errorf("拒绝正文没计入 token：with=%d bare=%d", with, without)
	}
	// 必须与同样文本的 text 块等价。只断言「比空的大」不够：块类型
	// 若落进未知块的 JSON 兜底分支，整个块结构都被计入，数值偏大但
	// 断言照样通过——那是把拒绝当成了认不出的块。
	asText := est(&ir.Request{Model: "m", MaxTokens: 100, Messages: []ir.Message{
		{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "do X"}}},
		{Role: ir.RoleAssistant, Content: []ir.Block{{Type: ir.BlockText, Text: refusalText}}},
		{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "please"}}},
	}})
	if with != asText {
		t.Errorf("拒绝没按纯文本计费：refusal=%d text=%d", with, asText)
	}
}

// Clone 是 JSON 往返，新块类型漏进序列化结构会在重试路径上静默丢内容。
func TestRefusalSurvivesClone(t *testing.T) {
	got := refusalHistoryReq().Clone()
	var found string
	for _, m := range got.Messages {
		for _, b := range m.Content {
			if b.Type == ir.BlockRefusal {
				found = b.Text
			}
		}
	}
	if found != refusalText {
		t.Errorf("Clone 后拒绝丢失：%+v", got.Messages)
	}
}
