package openairesponses

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// R103-1 item 级 vs part 级不透明块判别。官方 item 型并不都以 _call 结尾
// （computer_call_output、mcp_list_tools、mcp_approval_request、compaction
// 等，见 OpenAI SDK response_output_item 的 union），旧的按名字后缀判型会
// 把无后缀的 item 级载荷错塞进 message 的 part 数组——那是一个本族上游不
// 认识的 part 型，必 400。判别改为捕获时打 Item 标记，编码侧读标记。

// 请求历史 input 数组里无 _call 后缀的未知 item：decodeItem 兜底归不透明块，
// 必须打 Item=true 标记。
func TestDecodeRequestNonCallItemOpaqueMarked(t *testing.T) {
	body := []byte(`{"model":"m","input":[` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},` +
		`{"type":"mcp_list_tools","id":"mcplt_1","server_label":"srv","tools":[]}]}`)
	req, err := New().DecodeRequest(body)
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	var o *ir.Opaque
	for _, m := range req.Messages {
		for _, b := range m.Content {
			if b.Type == ir.BlockOpaque {
				o = b.Opaque
			}
		}
	}
	if o == nil || o.WireType != "mcp_list_tools" || o.From != Name {
		t.Fatalf("未知 item 没归本族不透明块：%+v", req.Messages)
	}
	if !o.Item {
		t.Errorf("item 位捕获的不透明块没打 Item 标记：%+v", o)
	}
}

// 非流式响应 output 数组里的 computer_call_output（无 _call 后缀的 item 型）
// 同样要打成 item 级不透明块。
func TestDecodeResponseNonCallItemOpaqueMarked(t *testing.T) {
	body := []byte(`{"id":"r1","model":"m","status":"completed","output":[` +
		`{"type":"computer_call_output","call_id":"cu_1","output":{"type":"computer_screenshot"}}]}`)
	resp, err := New().DecodeResponse(body)
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if len(resp.Content) != 1 || resp.Content[0].Type != ir.BlockOpaque {
		t.Fatalf("computer_call_output 没归不透明块：%+v", resp.Content)
	}
	o := resp.Content[0].Opaque
	if o.WireType != "computer_call_output" || o.From != Name || !o.Item {
		t.Errorf("不透明块判别值不对：%+v", o)
	}
}

// 流式 added/done：无 _call 后缀的托管 item 走 done 帧兜底，同样打 Item 标记。
func TestStreamNonCallItemOpaqueMarked(t *testing.T) {
	dec := New().NewStreamDecoder()
	evs := feedAll(t, dec,
		`{"type":"response.output_item.added","output_index":0,`+
			`"item":{"type":"mcp_list_tools","id":"mcplt_1","server_label":"srv"}}`,
		`{"type":"response.output_item.done","output_index":0,`+
			`"item":{"type":"mcp_list_tools","id":"mcplt_1","server_label":"srv","tools":[]}}`)
	starts := blockStarts(evs)
	if len(starts) != 1 || starts[0].Type != ir.BlockOpaque {
		t.Fatalf("mcp_list_tools 没归不透明块：%+v", starts)
	}
	if o := starts[0].Opaque; o.WireType != "mcp_list_tools" || !o.Item {
		t.Errorf("流式捕获没打 Item 标记：%+v", o)
	}
}

// 编码请求：Item=true 的本族不透明块必须作为独立 input item 回吐，而不是
// 塞进 message 的 content part 数组（后者是本族上游不认识的 part 型，必 400）。
func TestEncodeRequestItemOpaqueStaysStandaloneItem(t *testing.T) {
	raw := json.RawMessage(`{"type":"computer_call_output","call_id":"cu_1","output":{"type":"computer_screenshot"}}`)
	body, err := New().EncodeRequest(&ir.Request{
		Model: "m", MaxTokens: 64,
		Messages: []ir.Message{{Role: ir.RoleAssistant, Content: []ir.Block{
			{Type: ir.BlockOpaque, Opaque: &ir.Opaque{
				WireType: "computer_call_output", Body: raw, From: Name, Item: true}},
		}}},
	})
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	var wire struct {
		Input []struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
			} `json:"content"`
		} `json:"input"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatalf("出站请求不是合法 JSON：%v（%s）", err, body)
	}
	var standalone, nested bool
	for _, it := range wire.Input {
		if it.Type == "computer_call_output" {
			standalone = true
		}
		for _, p := range it.Content {
			if p.Type == "computer_call_output" {
				nested = true
			}
		}
	}
	if !standalone {
		t.Errorf("item 级不透明块没作为独立 item 回吐：%s", body)
	}
	if nested {
		t.Errorf("item 级载荷被错塞进 part 数组：%s", body)
	}
	if !strings.Contains(string(body), `"call_id":"cu_1"`) {
		t.Errorf("item 原文没逐字带回：%s", body)
	}
}

// 对照组：part 位捕获（Item=false）的本族不透明块仍然回吐进 part 数组。
func TestEncodeRequestPartOpaqueStaysPart(t *testing.T) {
	raw := json.RawMessage(`{"type":"future_part","data":"x"}`)
	body, err := New().EncodeRequest(&ir.Request{
		Model: "m", MaxTokens: 64,
		Messages: []ir.Message{{Role: ir.RoleAssistant, Content: []ir.Block{
			{Type: ir.BlockOpaque, Opaque: &ir.Opaque{
				WireType: "future_part", Body: raw, From: Name}},
		}}},
	})
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	var wire struct {
		Input []struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
			} `json:"content"`
		} `json:"input"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatalf("出站请求不是合法 JSON：%v（%s）", err, body)
	}
	var standalone, nested bool
	for _, it := range wire.Input {
		if it.Type == "future_part" {
			standalone = true
		}
		for _, p := range it.Content {
			if p.Type == "future_part" {
				nested = true
			}
		}
	}
	if standalone {
		t.Errorf("part 级不透明块被错提成独立 item：%s", body)
	}
	if !nested {
		t.Errorf("part 级不透明块没回吐进 part 数组：%s", body)
	}
}

// 流式编码：Item=true 的本族不透明块（无 _call 后缀）走 added/done item 帧
// 原文带回，不得落进 text 兜底凭空造空 output_text，也不得计数丢弃。
func TestStreamEncodeNonCallItemOpaqueRoundTrip(t *testing.T) {
	enc := codec{}.NewStreamEncoder()
	raw := json.RawMessage(`{"type":"mcp_list_tools","id":"mcplt_1","server_label":"srv","tools":[]}`)
	var out []byte
	for _, ev := range []ir.Event{
		{Type: ir.EvMessageStart, MessageID: "r1", Model: "m"},
		{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockOpaque,
			Opaque: &ir.Opaque{WireType: "mcp_list_tools", Body: raw, From: Name, Item: true}}},
		{Type: ir.EvBlockStop, Index: 0},
		{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn},
	} {
		frames, err := enc.Encode(ev)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range frames {
			out = append(out, f...)
		}
	}
	s := string(out)
	if !strings.Contains(s, `"response.output_item.added"`) ||
		!strings.Contains(s, `"response.output_item.done"`) {
		t.Errorf("item 级不透明块没走 item 帧带回：\n%s", s)
	}
	if !strings.Contains(s, `"type":"mcp_list_tools"`) || !strings.Contains(s, `"server_label":"srv"`) {
		t.Errorf("item 原文没逐字带回：\n%s", s)
	}
	if strings.Contains(s, "output_text") {
		t.Errorf("item 级载荷落进了 text 兜底：\n%s", s)
	}
	if notes := enc.(interface{ Notes() []string }).Notes(); len(notes) != 0 {
		t.Errorf("本族原样带回不该有损耗注记：%q", notes)
	}
}
