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

// R64：响应侧 service_tier 回显贯通。官方 SDK 核对（2026-09-22）：
// anthropic Message.service_tier ∈ standard|priority|batch；chat chunk/响应
// ∈ auto|default|flex|scale|priority|fast；responses 另有 ultrafast。
// 此前三族回显全部静默蒸发——客户端看不到实际用了哪档容量。

func TestMapServiceTierEchoValues(t *testing.T) {
	for _, c := range []struct {
		tier, target, want string
		ok                 bool
	}{
		{"standard", "anthropic", "standard", true},
		{"standard", "openai-chat", "default", true},
		{"default", "anthropic", "standard", true},
		{"standard_only", "openai-responses", "default", true},
		{"priority", "anthropic", "priority", true},
		{"priority", "openai-chat", "priority", true},
		{"batch", "anthropic", "batch", true},
		{"batch", "openai-chat", "", false},
		{"batch", "openai-responses", "", false},
		{"ultrafast", "openai-responses", "ultrafast", true},
		{"ultrafast", "codex", "ultrafast", true},
		{"ultrafast", "openai-chat", "", false},
		{"ultrafast", "anthropic", "", false},
		{"auto", "openai-chat", "auto", true},
		{"fast", "openai-chat", "fast", true},
		{"auto", "anthropic", "", false},
		{"flex", "anthropic", "", false},
		{"priority", "gemini", "", false},
		{"priority", "kiro", "", false},
	} {
		got, ok := proto.MapServiceTierEcho(c.tier, c.target)
		if ok != c.ok || got != c.want {
			t.Errorf("MapServiceTierEcho(%q, %q) = %q,%v，want %q,%v",
				c.tier, c.target, got, ok, c.want, c.ok)
		}
	}
}

// 三族流式解码：回显进 IR 事件。
func TestTierEchoStreamDecode(t *testing.T) {
	evs, err := proto.MustOutbound("anthropic").NewStreamDecoder().Feed("message_start",
		`{"type":"message_start","message":{"id":"m1","model":"c","service_tier":"priority"}}`)
	if err != nil || len(evs) == 0 || evs[0].ServiceTier != "priority" {
		t.Errorf("anthropic message_start 回显 = %+v, %v", evs, err)
	}

	dec := proto.MustOutbound("openai-chat").NewStreamDecoder()
	evs, _ = dec.Feed("", `{"id":"c1","model":"g","service_tier":"flex","choices":[]}`)
	if len(evs) == 0 || evs[0].ServiceTier != "flex" {
		t.Errorf("chat 首帧回显 = %+v", evs)
	}

	// 晚到形态：首帧没带，后续 chunk 才给——随 Finish 的 message_delta 交付。
	dec = proto.MustOutbound("openai-chat").NewStreamDecoder()
	if _, err := dec.Feed("", `{"id":"c1","model":"g","choices":[]}`); err != nil {
		t.Fatal(err)
	}
	if _, err := dec.Feed("", `{"id":"c1","service_tier":"fast","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`); err != nil {
		t.Fatal(err)
	}
	var tier string
	for _, ev := range dec.Finish() {
		if ev.Type == ir.EvMessageDelta {
			tier = ev.ServiceTier
		}
	}
	if tier != "fast" {
		t.Errorf("chat 晚到回显 = %q", tier)
	}

	evs, _ = proto.MustOutbound("openai-responses").NewStreamDecoder().Feed("response.created",
		`{"type":"response.created","response":{"id":"r1","model":"g","service_tier":"ultrafast"}}`)
	if len(evs) == 0 || evs[0].ServiceTier != "ultrafast" {
		t.Errorf("responses created 回显 = %+v", evs)
	}

	// completed 才带形态
	dec = proto.MustOutbound("openai-responses").NewStreamDecoder()
	_, _ = dec.Feed("response.created", `{"type":"response.created","response":{"id":"r1","model":"g"}}`)
	evs, _ = dec.Feed("response.completed",
		`{"type":"response.completed","response":{"id":"r1","service_tier":"priority"}}`)
	tier = ""
	for _, ev := range evs {
		if ev.Type == ir.EvMessageDelta {
			tier = ev.ServiceTier
		}
	}
	if tier != "priority" {
		t.Errorf("responses completed 回显 = %q", tier)
	}
}

// 流式编码：同族原样回家，跨族按目标值集互译。
func TestTierEchoStreamEncode(t *testing.T) {
	for _, c := range []struct{ name, tier, want string }{
		{"anthropic", "priority", `"service_tier":"priority"`},
		{"openai-chat", "flex", `"service_tier":"flex"`},
		{"openai-responses", "ultrafast", `"service_tier":"ultrafast"`},
		{"openai-chat", "standard", `"service_tier":"default"`}, // anthropic 方言互译
		{"anthropic", "default", `"service_tier":"standard"`},
	} {
		enc := proto.MustInbound(c.name).NewStreamEncoder()
		frames, err := enc.Encode(ir.Event{Type: ir.EvMessageStart, MessageID: "m1", Model: "g", ServiceTier: c.tier})
		if err != nil {
			t.Fatalf("%s Encode: %v", c.name, err)
		}
		var joined string
		for _, f := range frames {
			joined += string(f)
		}
		if !strings.Contains(joined, c.want) {
			t.Errorf("%s 回显 %q 未写成 %q: %s", c.name, c.tier, c.want, joined)
		}
		if notes := enc.Notes(); len(notes) != 0 {
			t.Errorf("%s 集内回显误报：%v", c.name, notes)
		}
	}
}

// 越集丢弃 + 流式注记；gemini 无槽位恒报。
func TestTierEchoStreamEncodeDropped(t *testing.T) {
	for _, c := range []struct{ name, tier string }{
		{"openai-chat", "batch"},
		{"anthropic", "ultrafast"},
		{"anthropic", "flex"},
		{"openai-chat", "ultrafast"},
		{"gemini", "priority"},
	} {
		enc := proto.MustInbound(c.name).NewStreamEncoder()
		frames, err := enc.Encode(ir.Event{Type: ir.EvMessageStart, MessageID: "m1", Model: "g", ServiceTier: c.tier})
		if err != nil {
			t.Fatalf("%s Encode: %v", c.name, err)
		}
		for _, f := range frames {
			if strings.Contains(string(f), "service_tier") {
				t.Errorf("%s 越集回显 %q 仍写出: %s", c.name, c.tier, f)
			}
		}
		notes := strings.Join(enc.Notes(), "; ")
		if !strings.Contains(notes, `dropped service tier echo "`+c.tier+`"`) {
			t.Errorf("%s 越集回显 %q 未报：%q", c.name, c.tier, notes)
		}
	}
}

// 晚到补写与晚到无处写：chat 后续 chunk 还补得上；anthropic 的
// message_delta 没有槽位，晚到只能报出。
func TestTierEchoLateArrival(t *testing.T) {
	enc := proto.MustInbound("openai-chat").NewStreamEncoder()
	if _, err := enc.Encode(ir.Event{Type: ir.EvMessageStart, MessageID: "c1", Model: "g"}); err != nil {
		t.Fatal(err)
	}
	frames, err := enc.Encode(ir.Event{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn, ServiceTier: "flex"})
	if err != nil {
		t.Fatal(err)
	}
	var joined string
	for _, f := range frames {
		joined += string(f)
	}
	if !strings.Contains(joined, `"service_tier":"flex"`) {
		t.Errorf("chat 晚到回显未补写: %s", joined)
	}
	if notes := enc.Notes(); len(notes) != 0 {
		t.Errorf("chat 晚到已补写，误报：%v", notes)
	}

	enc = proto.MustInbound("anthropic").NewStreamEncoder()
	if _, err := enc.Encode(ir.Event{Type: ir.EvMessageStart, MessageID: "m1", Model: "c"}); err != nil {
		t.Fatal(err)
	}
	if _, err := enc.Encode(ir.Event{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn, ServiceTier: "priority"}); err != nil {
		t.Fatal(err)
	}
	notes := strings.Join(enc.Notes(), "; ")
	if !strings.Contains(notes, `dropped service tier echo "priority"`) {
		t.Errorf("anthropic 晚到回显未报：%q", notes)
	}
}

// 非流式双向：DecodeResponse 进 IR，EncodeResponse 按目标值集回写。
func TestTierEchoNonStreamRoundTrip(t *testing.T) {
	for _, c := range []struct{ name, body, tier string }{
		{"anthropic", `{"id":"m1","type":"message","role":"assistant","model":"c","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1},"service_tier":"batch"}`, "batch"},
		{"openai-chat", `{"id":"c1","object":"chat.completion","created":1,"model":"g","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"service_tier":"scale"}`, "scale"},
		{"openai-responses", `{"id":"r1","model":"g","status":"completed","output":[],"service_tier":"ultrafast"}`, "ultrafast"},
	} {
		resp, err := proto.MustOutbound(c.name).DecodeResponse([]byte(c.body))
		if err != nil {
			t.Fatalf("%s DecodeResponse: %v", c.name, err)
		}
		if resp.ServiceTier != c.tier {
			t.Errorf("%s 回显未进 IR：%q", c.name, resp.ServiceTier)
		}
		out, err := proto.MustInbound(c.name).EncodeResponse(resp)
		if err != nil {
			t.Fatalf("%s EncodeResponse: %v", c.name, err)
		}
		if !strings.Contains(string(out), `"service_tier":"`+c.tier+`"`) {
			t.Errorf("%s 同族回显未回写: %s", c.name, out)
		}
	}
	// 缺省不多键
	out, err := proto.MustInbound("anthropic").EncodeResponse(&ir.Response{ID: "m", Model: "c",
		Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "service_tier") {
		t.Errorf("缺省时发明回显键: %s", out)
	}
}

// 非流式越集由 ResponseNotes 报出（四家全覆盖：gemini 无槽位恒报）。
func TestTierEchoResponseNotes(t *testing.T) {
	resp := &ir.Response{ID: "m", Model: "g", ServiceTier: "batch",
		Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}
	if notes := proto.MustInbound("anthropic").ResponseNotes(resp); len(notes) != 0 {
		t.Errorf("batch 回本族误报：%v", notes)
	}
	for _, name := range []string{"openai-chat", "openai-responses", "gemini"} {
		notes := strings.Join(proto.MustInbound(name).ResponseNotes(resp), "; ")
		if !strings.Contains(notes, `dropped service tier echo "batch"`) {
			t.Errorf("%s 未报 batch 越集：%q", name, notes)
		}
	}
	resp.ServiceTier = "priority"
	for _, name := range []string{"anthropic", "openai-chat", "openai-responses"} {
		if notes := proto.MustInbound(name).ResponseNotes(resp); len(notes) != 0 {
			t.Errorf("%s 集内回显误报：%v", name, notes)
		}
	}
	if notes := proto.MustInbound("gemini").ResponseNotes(resp); len(notes) == 0 {
		t.Error("gemini 无槽位未报")
	}
	resp.ServiceTier = ""
	for _, name := range []string{"anthropic", "openai-chat", "openai-responses", "gemini"} {
		if notes := proto.MustInbound(name).ResponseNotes(resp); len(notes) != 0 {
			t.Errorf("%s 无回显误报：%v", name, notes)
		}
	}
}

// 重复到达先到先得：首帧已定档，晚到的不同值不覆盖（同一上游重复回显
// 同值是常态，异值是病态——以先到为准，与聚合器同判据）。
func TestTierEchoFirstWins(t *testing.T) {
	enc := proto.MustInbound("openai-chat").NewStreamEncoder()
	if _, err := enc.Encode(ir.Event{Type: ir.EvMessageStart, MessageID: "c1", Model: "g", ServiceTier: "flex"}); err != nil {
		t.Fatal(err)
	}
	frames, err := enc.Encode(ir.Event{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn, ServiceTier: "fast"})
	if err != nil {
		t.Fatal(err)
	}
	var joined string
	for _, f := range frames {
		joined += string(f)
	}
	if !strings.Contains(joined, `"service_tier":"flex"`) || strings.Contains(joined, "fast") {
		t.Errorf("先到档位被覆盖：%s", joined)
	}
}

// responses 终止帧也带回显：SDK 的 get_final_response 读的是 completed
// 那一帧的完整 response 对象，只在 created 上带等于没给。
func TestTierEchoResponsesCompletedFrame(t *testing.T) {
	enc := proto.MustInbound("openai-responses").NewStreamEncoder()
	if _, err := enc.Encode(ir.Event{Type: ir.EvMessageStart, MessageID: "r1", Model: "g", ServiceTier: "ultrafast"}); err != nil {
		t.Fatal(err)
	}
	frames, err := enc.Encode(ir.Event{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn})
	if err != nil {
		t.Fatal(err)
	}
	var joined string
	for _, f := range frames {
		joined += string(f)
	}
	if !strings.Contains(joined, "response.completed") || !strings.Contains(joined, `"service_tier":"ultrafast"`) {
		t.Errorf("终止帧未带回显：%s", joined)
	}
}
