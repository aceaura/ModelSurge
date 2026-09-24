package openairesponses

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// R105：responses 一族剩余声明字段保真。truncation / max_tool_calls /
// stream_options.include_obfuscation 此前入站即蒸发（struct 没建模），
// 同协议回写也回不来；托管工具未建模声明参数与 anthropic 侧 HostedRaw
// 同病；流式 output_item 整块解析失败静默丢失。

// truncation / max_tool_calls 入站进 IR，同族回写逐字回来。
func TestTruncationAndMaxToolCallsRoundTrip(t *testing.T) {
	body := []byte(`{"model":"m","input":"hi","truncation":"disabled","max_tool_calls":3}`)
	req, err := New().DecodeRequest(body)
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if req.Truncation != "disabled" {
		t.Errorf("truncation 没进 IR：%q", req.Truncation)
	}
	if req.MaxToolCalls == nil || *req.MaxToolCalls != 3 {
		t.Errorf("max_tool_calls 没进 IR：%v", req.MaxToolCalls)
	}
	out, err := New().EncodeRequest(req.Clone())
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, `"truncation":"disabled"`) {
		t.Errorf("truncation 没回写：%s", s)
	}
	if !strings.Contains(s, `"max_tool_calls":3`) {
		t.Errorf("max_tool_calls 没回写：%s", s)
	}
}

// 没给就不造：缺省时出站一个键也不多出（替客户端表态等于改写请求）。
func TestTruncationAndMaxToolCallsAbsentStayAbsent(t *testing.T) {
	req, err := New().DecodeRequest([]byte(`{"model":"m","input":"hi"}`))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if req.Truncation != "" || req.MaxToolCalls != nil {
		t.Fatalf("缺省被伪造：truncation=%q maxToolCalls=%v", req.Truncation, req.MaxToolCalls)
	}
	out, err := New().EncodeRequest(req.Clone())
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	s := string(out)
	if strings.Contains(s, "truncation") || strings.Contains(s, "max_tool_calls") {
		t.Errorf("缺省字段被编造出站：%s", s)
	}
}

// stream_options.include_obfuscation 三态贯通：显式 false 是「关掉上游
// 默认开着的混淆保护」，与没提语义不同，两态布尔会把它吞回缺省。
func TestIncludeObfuscationRoundTrip(t *testing.T) {
	req, err := New().DecodeRequest([]byte(
		`{"model":"m","input":"hi","stream":true,"stream_options":{"include_obfuscation":false}}`))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if req.IncludeObfuscation == nil || *req.IncludeObfuscation {
		t.Fatalf("显式 false 没留住：%v", req.IncludeObfuscation)
	}
	out, err := New().EncodeRequest(req.Clone())
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if !strings.Contains(string(out), `"include_obfuscation":false`) {
		t.Errorf("include_obfuscation 没回写：%s", out)
	}
	// 没提的留 nil，出站不造键。
	req2, err := New().DecodeRequest([]byte(`{"model":"m","input":"hi","stream":true}`))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if req2.IncludeObfuscation != nil {
		t.Fatalf("没提被伪造：%v", req2.IncludeObfuscation)
	}
	out2, err := New().EncodeRequest(req2.Clone())
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if strings.Contains(string(out2), "include_obfuscation") {
		t.Errorf("缺省字段被编造出站：%s", out2)
	}
}

// 托管工具未建模声明参数：同族回写整块原文回吐（anthropic HostedRaw 同款）。
// web_search 的未来声明键建模跟进永远慢半拍，逐字段重建必丢。
func TestHostedRawRoundTripKeepsUnmodeledParams(t *testing.T) {
	body := []byte(`{"model":"m","input":"hi","tools":[` +
		`{"type":"web_search","search_context_size":"high","future_knob":{"x":1}}]}`)
	req, err := New().DecodeRequest(body)
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if len(req.Tools) != 1 || len(req.Tools[0].HostedRaw) == 0 {
		t.Fatalf("HostedRaw 没收下：%+v", req.Tools)
	}
	out, err := New().EncodeRequest(req.Clone())
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, `"future_knob":{"x":1}`) {
		t.Errorf("未建模声明参数没原文回吐：%s", s)
	}
	if !strings.Contains(s, `"search_context_size":"high"`) {
		t.Errorf("已建模参数没随原文带回：%s", s)
	}
	if strings.Contains(s, `null`) {
		t.Errorf("Clone 往返伪造了 null 键：%s", s)
	}
}

// 函数工具不能误吃 HostedRaw 通道：原文回吐只属托管工具。
func TestFunctionToolDoesNotCarveRaw(t *testing.T) {
	body := []byte(`{"model":"m","input":"hi","tools":[` +
		`{"type":"function","name":"f","parameters":{"type":"object"}}]}`)
	req, err := New().DecodeRequest(body)
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if len(req.Tools) != 1 || len(req.Tools[0].HostedRaw) != 0 {
		t.Fatalf("函数工具不该有 HostedRaw：%+v", req.Tools)
	}
}

// 流式 output_item 整块解析失败（字段形状冲突到连 type 都解不出）：
// 归不透明块 + Notes 计数，不再静默蒸发。
func TestStreamUnparseableItemGoesOpaque(t *testing.T) {
	dec := New().NewStreamDecoder()
	evs := feedAll(t, dec,
		`{"type":"response.output_item.added","output_index":0,`+
			`"item":{"type":"message","role":123}}`,
		`{"type":"response.output_item.done","output_index":0,`+
			`"item":{"type":"message","role":123,"content":[]}}`)
	starts := blockStarts(evs)
	if len(starts) != 1 || starts[0].Type != ir.BlockOpaque {
		t.Fatalf("解析失败的 item 没归不透明块：%+v", starts)
	}
	o := starts[0].Opaque
	if o.WireType != "message" || !o.Item || o.From != Name {
		t.Errorf("不透明块判别值不对：%+v", o)
	}
	if !strings.Contains(string(o.Body), `"role":123`) {
		t.Errorf("item 原文没逐字保留：%s", o.Body)
	}
	type noter interface{ Notes() []string }
	notes := dec.(noter).Notes()
	if len(notes) == 0 {
		t.Errorf("损耗没报出来：Notes 为空")
	}
}

// added 帧原文在 done 帧缺席时也要兜住（done 的 item 解析失败但 added 存过原文）。
func TestStreamUnparseableItemFallsBackToAddedRaw(t *testing.T) {
	dec := New().NewStreamDecoder()
	evs := feedAll(t, dec,
		`{"type":"response.output_item.added","output_index":0,`+
			`"item":{"type":"future_widget","role":123}}`,
		`{"type":"response.output_item.done","output_index":0}`)
	starts := blockStarts(evs)
	if len(starts) != 1 || starts[0].Type != ir.BlockOpaque {
		t.Fatalf("added 缓存的原文没兜住：%+v", starts)
	}
	if o := starts[0].Opaque; o.WireType != "future_widget" {
		t.Errorf("wire 类型没从 added 原文探出：%+v", o)
	}
}

// text.format json_schema 的 description：官方键，逐字段重建时不能丢。
func TestTextFormatDescriptionRoundTrip(t *testing.T) {
	body := []byte(`{"model":"m","input":"hi","text":{"format":{` +
		`"type":"json_schema","name":"s","description":"回执结构","schema":{"type":"object"}}}}`)
	req, err := New().DecodeRequest(body)
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if req.ResponseFormat == nil || req.ResponseFormat.Description != "回执结构" {
		t.Fatalf("description 没进 IR：%+v", req.ResponseFormat)
	}
	out, err := New().EncodeRequest(req.Clone())
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if !strings.Contains(string(out), `"description":"回执结构"`) {
		t.Errorf("description 没回写：%s", out)
	}
}

// 跨族方向：Truncation/MaxToolCalls/HostedRaw 不进外族出站（anthropic 没有
// 这些键的槽位）。泄漏标记用原文独有的 max_uses；anthropic 原生名
// web_search_20250305 属 anthropic 家族，同族判定为假。
func TestResponsesExtrasIgnoredCrossFamily(t *testing.T) {
	req := &ir.Request{
		Model: "m", MaxTokens: 64,
		Messages:   []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}},
		Truncation: "auto",
		Tools: []ir.Tool{{Hosted: ir.HostedWebSearch, HostedType: "web_search_20250305",
			HostedRaw: json.RawMessage(`{"type":"web_search_20250305","max_uses":9}`)}},
	}
	out, err := New().EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	s := string(out)
	if strings.Contains(s, "max_uses") {
		t.Errorf("外族来路的 HostedRaw 被原文回吐：%s", s)
	}
	if !strings.Contains(s, `"type":"web_search"`) {
		t.Errorf("外族原生名没落回本族默认名：%s", s)
	}
}
