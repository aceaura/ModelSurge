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

// R97：OpenAI 两系客户端发来的未知 content part（video_url、input_video、厂商私有
// 形态）此前在入站解码时被静默丢掉——part 数组的 switch 没有 default 分支，元素连
// IR 都进不去，于是 Diagnose 与 ResponseNotes 全看不到它。后果分三层：
//
//   - 同族往返有损：chat 客户端发 video_url、路由到 chat 上游，上游只看到文字，
//     模型答非所问，而客户端拿不到任何注记可循；
//   - 跨族静默：路由到 anthropic 上游时同样什么都不剩；
//   - 整条消息只有一个未知 part 时，消息内容变空，anthropic 出站写出
//     "content":[] ——Anthropic 要求每条消息 content 非空，直接 400 拒整轮。
//
// 修法与 anthropic 的未知块同源：原样留成不透明块。同时给不透明块打上来源族标记
// （ir.Opaque.From），因为只有产出它的那一族能逐字接回去；外族逐字发过去就是一个
// 目标上游不认识的 part 型 / 块型，被按型校验直接 400，那是比丢内容更糟的结果，
// 降级成文本又是把别家的载荷涂进正文。所以外族只有「整块跳过 + 报损耗」一条路。

const r97ChatVideo = `{"type":"video_url","video_url":{"url":"https://example.com/v.mp4","detail":"high"}}`

const r97ChatWidget = `{"type":"custom_widget","payload":{"a":1}}`

const r97RespVideo = `{"type":"input_video","video_url":"https://example.com/v.mp4"}`

// r97Secret 未知 part 的载荷。跨族出站时它不得出现在线上，也不得进注记。
const r97Secret = "https://example.com/v.mp4"

// 出站外族：agent 侧除 anthropic 之外的三家（codex 与 responses 同形）。
var r97ForeignOutbound = []string{"codex", "openai-chat", "openai-responses"}

func r97ChatReq(t *testing.T, parts ...string) *ir.Request {
	t.Helper()
	body := `{"model":"m","messages":[{"role":"user","content":[` + strings.Join(parts, ",") + `]}]}`
	req, err := proto.MustInbound("openai-chat").DecodeRequest([]byte(body))
	if err != nil {
		t.Fatalf("chat DecodeRequest: %v", err)
	}
	return req
}

func r97RespReq(t *testing.T, parts ...string) *ir.Request {
	t.Helper()
	body := `{"model":"m","input":[{"role":"user","content":[` + strings.Join(parts, ",") + `]}]}`
	req, err := proto.MustInbound("openai-responses").DecodeRequest([]byte(body))
	if err != nil {
		t.Fatalf("responses DecodeRequest: %v", err)
	}
	return req
}

func r97Encode(t *testing.T, outbound string, req *ir.Request) string {
	t.Helper()
	body, err := proto.MustOutbound(outbound).EncodeRequest(req)
	if err != nil {
		t.Fatalf("%s EncodeRequest: %v", outbound, err)
	}
	return string(body)
}

// ---- 入站：未知 part 不再蒸发 ----

func TestUnknownChatPartBecomesOpaque(t *testing.T) {
	req := r97ChatReq(t, `{"type":"text","text":"watch this"}`, r97ChatVideo, r97ChatWidget)
	blocks := req.Messages[0].Content
	if len(blocks) != 3 {
		t.Fatalf("块数 = %d，want 3（未知 part 蒸发的旧缺陷在这里表现为 1）：%+v", len(blocks), blocks)
	}
	if blocks[0].Type != ir.BlockText || blocks[0].Text != "watch this" {
		t.Errorf("兄弟文本块被牵连：%+v", blocks[0])
	}
	for i, tc := range []struct{ wireType, raw string }{
		{"video_url", r97ChatVideo},
		{"custom_widget", r97ChatWidget},
	} {
		b := blocks[i+1]
		if b.Type != ir.BlockOpaque || b.Opaque == nil {
			t.Fatalf("block[%d] = %+v，want opaque", i+1, b)
		}
		if b.Opaque.WireType != tc.wireType {
			t.Errorf("wire 判别值 = %q，want %q", b.Opaque.WireType, tc.wireType)
		}
		if string(b.Opaque.Body) != tc.raw {
			t.Errorf("part 体未逐字节保留：\n got %s\nwant %s", b.Opaque.Body, tc.raw)
		}
		// 来源族标记是跨族门控的唯一依据：缺了它就没法区分「本族未知 part」与
		// 「别家未知块」，而两者处置相反。
		if b.Opaque.From != "openai-chat" {
			t.Errorf("From = %q，want openai-chat", b.Opaque.From)
		}
		if b.Text != "" {
			t.Errorf("不透明块被降级成文本：%q", b.Text)
		}
	}
}

func TestUnknownResponsesPartBecomesOpaque(t *testing.T) {
	req := r97RespReq(t, `{"type":"input_text","text":"watch this"}`, r97RespVideo)
	blocks := req.Messages[0].Content
	if len(blocks) != 2 {
		t.Fatalf("块数 = %d，want 2：%+v", len(blocks), blocks)
	}
	b := blocks[1]
	if b.Type != ir.BlockOpaque || b.Opaque == nil || b.Opaque.WireType != "input_video" {
		t.Fatalf("block[1] = %+v，want opaque/input_video", b)
	}
	if string(b.Opaque.Body) != r97RespVideo {
		t.Errorf("part 体未逐字节保留：\n got %s\nwant %s", b.Opaque.Body, r97RespVideo)
	}
	if b.Opaque.From != "openai-responses" {
		t.Errorf("From = %q，want openai-responses", b.Opaque.From)
	}
}

// ---- 同族往返：逐字带回，含没建模的子字段 ----

func TestUnknownPartRoundTripsWithinFamily(t *testing.T) {
	t.Run("openai-chat", func(t *testing.T) {
		req := r97ChatReq(t, `{"type":"text","text":"watch this"}`, r97ChatVideo)
		out := r97Encode(t, "openai-chat", req)
		if !strings.Contains(out, r97ChatVideo) {
			t.Errorf("未知 part 未原样回吐：%s", out)
		}
		// detail 是本仓没有建模的子字段：逐字段重建会把它丢掉，上游按默认分辨率
		// 处理，客户端明明要了 high 却拿到 low，且没有任何注记可循。
		if !strings.Contains(out, `"detail":"high"`) {
			t.Errorf("未建模的 detail 子字段被丢掉：%s", out)
		}
		// 再解一次：同族往返不得漂移。
		back, err := proto.MustInbound("openai-chat").DecodeRequest([]byte(out))
		if err != nil {
			t.Fatalf("往返 DecodeRequest: %v", err)
		}
		got := back.Messages[0].Content[1]
		if got.Type != ir.BlockOpaque || string(got.Opaque.Body) != r97ChatVideo {
			t.Errorf("同族往返漂移：%+v", got)
		}
	})
	t.Run("openai-responses", func(t *testing.T) {
		req := r97RespReq(t, `{"type":"input_text","text":"watch this"}`, r97RespVideo)
		out := r97Encode(t, "openai-responses", req)
		if !strings.Contains(out, r97RespVideo) {
			t.Errorf("未知 part 未原样回吐：%s", out)
		}
	})
	// codex 与 openai-responses 是同一套线格式（codexCodec 内嵌 responses 的
	// codec）。按 codec 名逐字比来源族会把这条同形通道误判成跨族，把本来能原样
	// 带回的 part 丢掉。
	t.Run("codex", func(t *testing.T) {
		req := r97RespReq(t, `{"type":"input_text","text":"watch this"}`, r97RespVideo)
		out := r97Encode(t, "codex", req)
		if !strings.Contains(out, r97RespVideo) {
			t.Errorf("同形的 codex 通道被误判成跨族：%s", out)
		}
	})
}

// 助手回合的历史里也可能夹着未知 part（客户端把别处收到的历史原样回放）。
// responses 的 assistant content 是 part 数组，装得下；chat 的 assistant content
// 出站是纯字符串形态，装不下——那一条走跨族丢弃 + 诊断，不在这里断言。
func TestUnknownPartRoundTripsInResponsesAssistantHistory(t *testing.T) {
	body := `{"model":"m","input":[
		{"role":"user","content":[{"type":"input_text","text":"go"}]},
		{"role":"assistant","content":[{"type":"output_text","text":"ok"},` + r97RespVideo + `]}]}`
	req, err := proto.MustInbound("openai-responses").DecodeRequest([]byte(body))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if n := len(req.Messages[1].Content); n != 2 {
		t.Fatalf("assistant 块数 = %d，want 2", n)
	}
	out := r97Encode(t, "openai-responses", req)
	if !strings.Contains(out, r97RespVideo) {
		t.Errorf("assistant 历史里的未知 part 未原样回吐：%s", out)
	}
	if !strings.Contains(out, `"ok"`) {
		t.Errorf("兄弟正文被牵连：%s", out)
	}
}

// ---- 跨族：整块跳过，不得塞进任何槽位 ----

func TestUnknownPartNotLeakedIntoForeignRequest(t *testing.T) {
	req := r97ChatReq(t, `{"type":"text","text":"watch this"}`, r97ChatVideo, r97ChatWidget)
	for _, name := range []string{"anthropic", "openai-responses", "codex"} {
		t.Run("chat->"+name, func(t *testing.T) {
			out := r97Encode(t, name, req)
			if strings.Contains(out, r97Secret) || strings.Contains(out, "video_url") ||
				strings.Contains(out, "custom_widget") {
				t.Errorf("外族出站泄漏未知 part：%s", out)
			}
			if strings.Contains(out, `"type":""`) {
				t.Errorf("出站出现空 part 型：%s", out)
			}
			if !strings.Contains(out, "watch this") {
				t.Errorf("兄弟正文被误删：%s", out)
			}
		})
	}
	req2 := r97RespReq(t, `{"type":"input_text","text":"watch this"}`, r97RespVideo)
	for _, name := range []string{"anthropic", "openai-chat"} {
		t.Run("responses->"+name, func(t *testing.T) {
			out := r97Encode(t, name, req2)
			if strings.Contains(out, r97Secret) || strings.Contains(out, "input_video") {
				t.Errorf("外族出站泄漏未知 part：%s", out)
			}
			if !strings.Contains(out, "watch this") {
				t.Errorf("兄弟正文被误删：%s", out)
			}
		})
	}
}

// anthropic 自家来源的不透明块必须照旧逐字回吐：门控只拦外族，拦到本族就是把
// R91 修好的服务端工具结果块重新丢一遍。
func TestNativeOpaqueStillRoundTripsOnAnthropic(t *testing.T) {
	const native = `{"type":"web_fetch_tool_result","tool_use_id":"tfu_1","content":{"type":"web_fetch_result","content":[{"type":"text","text":"fetched page body"}]}}`
	body := `{"model":"m","max_tokens":16,"messages":[
		{"role":"user","content":[{"type":"text","text":"fetch it"}]},
		{"role":"assistant","content":[{"type":"server_tool_use","id":"tfu_1","name":"web_fetch","input":{"url":"https://e.com"}},` + native + `]},
		{"role":"user","content":[{"type":"text","text":"thanks"}]}]}`
	req, err := proto.MustInbound("anthropic").DecodeRequest([]byte(body))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if got := req.Messages[1].Content[1].Opaque; got == nil || got.From != "anthropic" {
		t.Fatalf("本族不透明块没打上来源标记：%+v", got)
	}
	out := r97Encode(t, "anthropic", req)
	if !strings.Contains(out, native) {
		t.Errorf("本族不透明块未原样回吐：%s", out)
	}
	if !strings.Contains(out, `"type":"server_tool_use"`) {
		t.Errorf("配平的 server_tool_use 丢失：%s", out)
	}
}

// ---- 逐元素解析：单个坏元素不得牵连兄弟 ----

// 一次性解 []part 的实现里，任一元素的形状冲突会让整条 content 的 Unmarshal 失败，
// 同消息里用户真正在问的那句话跟着一起蒸发，调用方只拿到一个 nil。
func TestMalformedPartDoesNotDestroySiblings(t *testing.T) {
	for _, tc := range []struct {
		name, content string
	}{
		{"裸字符串元素", `[{"type":"text","text":"before"},"not-an-object",{"type":"text","text":"after"}]`},
		{"同名键形状冲突", `[{"type":"text","text":"before"},{"type":"file","file":"not-an-object"},{"type":"text","text":"after"}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"model":"m","messages":[{"role":"user","content":` + tc.content + `}]}`
			req, err := proto.MustInbound("openai-chat").DecodeRequest([]byte(body))
			if err != nil {
				t.Fatalf("DecodeRequest: %v", err)
			}
			blocks := req.Messages[0].Content
			if len(blocks) != 2 {
				t.Fatalf("块数 = %d，want 2（坏元素牵连兄弟的旧缺陷在这里表现为 0）：%+v", len(blocks), blocks)
			}
			if blocks[0].Text != "before" || blocks[1].Text != "after" {
				t.Errorf("兄弟文本块丢失：%+v", blocks)
			}
		})
	}
}

// 连 type 都读不出来的元素不是 content part：留着会在出站时变成 {"type":""}
// 让上游 400（与 anthropic decodeRawBlock 同一口径）。
func TestPartWithoutTypeIsDroppedNotOpaque(t *testing.T) {
	req := r97ChatReq(t, `{"foo":1}`, `{"type":"text","text":"ok"}`)
	blocks := req.Messages[0].Content
	if len(blocks) != 1 {
		t.Fatalf("块数 = %d，want 1：%+v", len(blocks), blocks)
	}
	if blocks[0].Type != ir.BlockText || blocks[0].Text != "ok" {
		t.Errorf("有效块被牵连：%+v", blocks[0])
	}
	if out := r97Encode(t, "openai-chat", req); strings.Contains(out, `"type":""`) {
		t.Errorf("出站出现空 part 型：%s", out)
	}
}

// ---- 空消息兜底：整条消息只有一个未知 part ----

// 规整流水线补不上这个占位：它跑在编码之前，那会儿这条消息看起来还是有内容的
// （一个不透明块），是编码器随后把它整块丢掉了。Anthropic 要求 content 非空，
// 空数组直接 400。
func TestWholeMessageUnknownPartKeepsAnthropicContentNonEmpty(t *testing.T) {
	req := r97ChatReq(t, r97ChatVideo)
	out := r97Encode(t, "anthropic", req)
	if strings.Contains(out, `"content":[]`) {
		t.Errorf("anthropic 出站出现空 content（上游会 400）：%s", out)
	}
	if !strings.Contains(out, `"role":"user"`) {
		t.Errorf("整条消息消失：%s", out)
	}
	if strings.Contains(out, r97Secret) {
		t.Errorf("外族 part 载荷泄漏进占位：%s", out)
	}
	// 占位必须是非空的约定字面量（与 normalize.Placeholder 同值）：写成空文本块
	// 等于给上游一条「用户什么都没说」的消息，客户端也无从分辨这是占位还是正文。
	if !strings.Contains(out, `"text":"(empty)"`) {
		t.Errorf("占位文本不是约定的非空字面量：%s", out)
	}
}

// 同族来源但块体为空的不透明块不得被逐字回吐：Raw 为空时 MarshalJSON 会退回
// 逐字段形态，写出去就是一个只有判别值、没有载荷的块（chat/responses 是
// {"type":""}，anthropic 是缺必填字段的空壳），上游按块型校验直接 400。
// 门控里的 len(Body) > 0 就是拦这个的。
func TestEmptyBodyOpaqueIsNotEmittedPayloadless(t *testing.T) {
	for _, fam := range []string{"anthropic", "openai-chat", "openai-responses"} {
		t.Run(fam, func(t *testing.T) {
			req := &ir.Request{Model: "m", MaxTokens: 16, Messages: []ir.Message{
				{Role: ir.RoleUser, Content: []ir.Block{
					{Type: ir.BlockText, Text: "before"},
					{Type: ir.BlockOpaque, Opaque: &ir.Opaque{
						WireType: "web_fetch_tool_result", From: fam}},
				}},
			}}
			out := r97Encode(t, fam, req)
			if strings.Contains(out, `"type":""`) {
				t.Errorf("空块体被写成空判别值 part：%s", out)
			}
			if strings.Contains(out, "web_fetch_tool_result") {
				t.Errorf("空块体被写成没有载荷的空壳块：%s", out)
			}
			if !strings.Contains(out, "before") {
				t.Errorf("兄弟正文被牵连：%s", out)
			}
			if strings.Contains(out, `"content":[]`) {
				t.Errorf("整条消息被写空：%s", out)
			}
		})
	}
}

// ---- 损耗可见：外族来源的不透明块在 anthropic 侧也要报 ----

// 不透明块此前只有「anthropic 之外」才报损耗，因为唯一的产出方是 anthropic。
// 现在 OpenAI 两系也产出，判定必须改成按来源族逐块判，否则 chat 客户端发来的
// 未知 part 投给 anthropic 上游时是静默丢的。
func TestForeignOpaqueReportedOnAnthropicResponseSide(t *testing.T) {
	resp := &ir.Response{ID: "m", Model: "m", Content: []ir.Block{
		{Type: ir.BlockOpaque, Opaque: &ir.Opaque{
			WireType: "video_url", Body: json.RawMessage(r97ChatVideo), From: "openai-chat"}},
		{Type: ir.BlockText, Text: "visible answer"},
	}}
	got := strings.Join(proto.MustInbound("anthropic").ResponseNotes(resp), "; ")
	if !strings.Contains(got, "dropped 1 opaque content block(s)") {
		t.Errorf("anthropic 应报外族不透明块丢失：%q", got)
	}
	if strings.Contains(got, r97Secret) {
		t.Errorf("注记抄出块体：%q", got)
	}
	// 本族来源照旧静默：过度报告会让客户端对每一次正常往返都收到损耗头。
	native := &ir.Response{ID: "m", Model: "m", Content: []ir.Block{
		{Type: ir.BlockOpaque, Opaque: &ir.Opaque{
			WireType: "web_fetch_tool_result", Body: json.RawMessage(`{"u":"x"}`), From: "anthropic"}},
	}}
	if notes := proto.MustInbound("anthropic").ResponseNotes(native); len(notes) != 0 {
		t.Errorf("本族不透明块误报：%v", notes)
	}
	// 来源不明（From 为空）按外族处理：宁丢不伪造。
	unknown := &ir.Response{ID: "m", Model: "m", Content: []ir.Block{
		{Type: ir.BlockOpaque, Opaque: &ir.Opaque{WireType: "x", Body: json.RawMessage(`{"u":"x"}`)}}}}
	for _, name := range []string{"anthropic", "openai-chat", "openai-responses", "gemini"} {
		if notes := proto.MustInbound(name).ResponseNotes(unknown); len(notes) == 0 {
			t.Errorf("%s 对来源不明的不透明块未报损耗", name)
		}
	}
}

// 流式侧同一判定：外族块不开 content_block_start（留个空壳块会让客户端多出一个
// 没有内容的块），但要报损耗；本族块照旧全量下发且不报。
func TestForeignOpaqueSkippedAndReportedInAnthropicStream(t *testing.T) {
	foreign := ir.Block{Type: ir.BlockOpaque, Opaque: &ir.Opaque{
		WireType: "video_url", Body: json.RawMessage(r97ChatVideo), From: "openai-chat"}}
	events := []ir.Event{
		{Type: ir.EvMessageStart, MessageID: "m", Model: "m"},
		{Type: ir.EvBlockStart, Index: 0, Block: &foreign},
		{Type: ir.EvBlockStop, Index: 0},
		{Type: ir.EvBlockStart, Index: 1, Block: &ir.Block{Type: ir.BlockText}},
		{Type: ir.EvTextDelta, Index: 1, Text: "visible answer"},
		{Type: ir.EvBlockStop, Index: 1},
		{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn},
		{Type: ir.EvMessageStop},
	}
	out := streamOut(t, "anthropic", events)
	if strings.Contains(out, r97Secret) || strings.Contains(out, "video_url") {
		t.Errorf("外族块体泄漏进流：\n%s", out)
	}
	if n := strings.Count(out, `"type":"content_block_start"`); n != 1 {
		t.Errorf("应只开正文一个块，实得 %d（多出的是空壳块）：\n%s", n, out)
	}
	if n := strings.Count(out, `"type":"content_block_stop"`); n != 1 {
		t.Errorf("块开合不配平：stop = %d\n%s", n, out)
	}
	if !strings.Contains(out, "visible answer") {
		t.Errorf("正文被误删：\n%s", out)
	}

	e := proto.MustInbound("anthropic").NewStreamEncoder()
	for _, ev := range events {
		if _, err := e.Encode(ev); err != nil {
			t.Fatalf("Encode(%s): %v", ev.Type, err)
		}
	}
	got := strings.Join(e.Notes(), "; ")
	if !strings.Contains(got, "dropped 1 opaque content block(s)") {
		t.Errorf("流式应报外族不透明块丢失：%q", got)
	}
	if strings.Contains(got, r97Secret) {
		t.Errorf("注记抄出块体：%q", got)
	}
	if again := e.Notes(); len(again) != 0 {
		t.Errorf("Notes 应幂等排干：%v", again)
	}

	// 本族块：全量下发且不报损耗。
	native := ir.Block{Type: ir.BlockOpaque, Opaque: &ir.Opaque{
		WireType: "web_fetch_tool_result", Body: json.RawMessage(`{"u":"x"}`), From: "anthropic"}}
	en := proto.MustInbound("anthropic").NewStreamEncoder()
	en.Encode(ir.Event{Type: ir.EvMessageStart, MessageID: "m", Model: "m"})
	en.Encode(ir.Event{Type: ir.EvBlockStart, Index: 0, Block: &native})
	en.Encode(ir.Event{Type: ir.EvBlockStop, Index: 0})
	if notes := en.Notes(); len(notes) != 0 {
		t.Errorf("本族不透明块误报损耗：%v", notes)
	}
}

// 外族客户端协议收到 anthropic 来源的不透明块：照旧整块跳过并报损耗（R91 行为
// 不得因为来源标记的引入而回退）。
func TestNativeOpaqueStillDroppedByForeignInbound(t *testing.T) {
	resp := &ir.Response{ID: "m", Model: "m", Content: []ir.Block{
		{Type: ir.BlockOpaque, Opaque: &ir.Opaque{
			WireType: "web_fetch_tool_result", Body: json.RawMessage(`{"u":"x"}`), From: "anthropic"}},
		{Type: ir.BlockText, Text: "visible answer"}}}
	for _, name := range r97ForeignOutbound {
		if name == "codex" {
			continue // codex 是出站协议名，没有入站 codec
		}
		got := strings.Join(proto.MustInbound(name).ResponseNotes(resp), "; ")
		if !strings.Contains(got, "dropped 1 opaque content block(s)") {
			t.Errorf("%s 应报不透明块丢失：%q", name, got)
		}
	}
}

// responses 侧同口径：连 type 都读不出来的元素不是 content part，留着会在出站时
// 变成 {"type":""} 让上游 400。归成不透明块等于把一个没有判别值的元素原样发出去。
func TestResponsesPartWithoutTypeIsDroppedNotOpaque(t *testing.T) {
	req := r97RespReq(t, `{"foo":1}`, `{"type":"input_text","text":"ok"}`)
	blocks := req.Messages[0].Content
	if len(blocks) != 1 {
		t.Fatalf("块数 = %d，want 1：%+v", len(blocks), blocks)
	}
	if blocks[0].Type != ir.BlockText || blocks[0].Text != "ok" {
		t.Errorf("有效块被牵连：%+v", blocks[0])
	}
	if out := r97Encode(t, "openai-responses", req); strings.Contains(out, `"type":""`) {
		t.Errorf("出站出现空 part 型：%s", out)
	}
}

// 门控判定的两条边界：块体为空时不得判成「可逐字回吐」——Raw 为空会让
// MarshalJSON 退回逐字段形态，写出去就是一个只有判别值、没有载荷的空壳块。
func TestOpaqueVerbatimForRequiresNonEmptyBody(t *testing.T) {
	if proto.OpaqueVerbatimFor(&ir.Opaque{WireType: "w", From: "anthropic"}, "anthropic") {
		t.Error("空块体被判成可逐字回吐")
	}
	if proto.OpaqueVerbatimFor(nil, "anthropic") {
		t.Error("nil 不透明块被判成可逐字回吐")
	}
	if !proto.OpaqueVerbatimFor(
		&ir.Opaque{WireType: "w", Body: json.RawMessage(`{"a":1}`), From: "anthropic"}, "anthropic") {
		t.Error("同族且有块体却被判成不可回吐")
	}
	// 来源不明按外族处理：宁丢不伪造。
	if proto.OpaqueVerbatimFor(&ir.Opaque{WireType: "w", Body: json.RawMessage(`{"a":1}`)}, "anthropic") {
		t.Error("来源不明的块被判成本族")
	}
}
