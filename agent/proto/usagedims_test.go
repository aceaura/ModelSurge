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

// R106-A3+B6：usage 细分维度跨族丢弃注记。细分维度的原生槽位是单家的
// （服务端工具执行次数/推理区域只在 anthropic；音频/预测 token 只在
// openai-chat），跨族转发时聚合 token 总量还在，细分静默蒸发——此前只有
// cache creation TTL 明细一个先例（droppedCacheDetails），其余全丢不报。

// anthropic 原生细分（两个不对称计数 + 推理区域）。
func anthropicDimsUsage() *ir.Usage {
	return &ir.Usage{InputTokens: 10, OutputTokens: 5,
		WebSearchRequests: 2, WebFetchRequests: 1, InferenceGeo: "us"}
}

// chat 原生细分（音频输入输出 + 预测加速，计数不对称防夹具串味）。
func chatDimsUsage() *ir.Usage {
	return &ir.Usage{InputTokens: 10, OutputTokens: 5,
		PromptAudioTokens: 3, CompletionAudioTokens: 7,
		AcceptedPredictionTokens: 11, RejectedPredictionTokens: 13}
}

func streamNotes(t *testing.T, name string, u *ir.Usage, includeUsage bool) string {
	t.Helper()
	enc := proto.MustInbound(name).NewStreamEncoder()
	if su, ok := enc.(interface{ SetIncludeUsage(bool) }); ok {
		su.SetIncludeUsage(includeUsage)
	}
	if _, err := enc.Encode(ir.Event{Type: ir.EvMessageStart, MessageID: "m1", Model: "g", Usage: u}); err != nil {
		t.Fatalf("%s message_start: %v", name, err)
	}
	if _, err := enc.Encode(ir.Event{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn, Usage: u}); err != nil {
		t.Fatalf("%s message_delta: %v", name, err)
	}
	return strings.Join(enc.Notes(), "; ")
}

// 外族目标报出本族装不下的细分；本族目标闭嘴。
func TestUsageDimsStreamNotes(t *testing.T) {
	// anthropic 细分 -> 三个外族：报 web search/fetch/geo，不报音频。
	for _, name := range []string{"openai-chat", "openai-responses", "gemini"} {
		notes := streamNotes(t, name, anthropicDimsUsage(), true)
		if !strings.Contains(notes, "dropped usage detail(s)") ||
			!strings.Contains(notes, "web search request count") ||
			!strings.Contains(notes, "web fetch request count") ||
			!strings.Contains(notes, "inference geo") {
			t.Errorf("%s 未报 anthropic 细分丢弃：%q", name, notes)
		}
		if strings.Contains(notes, "audio tokens") {
			t.Errorf("%s 误报不存在的音频细分：%q", name, notes)
		}
	}
	// chat 细分 -> anthropic/responses/gemini：报音频+预测，不报服务端工具项。
	for _, name := range []string{"anthropic", "openai-responses", "gemini"} {
		notes := streamNotes(t, name, chatDimsUsage(), true)
		if !strings.Contains(notes, "prompt audio tokens") ||
			!strings.Contains(notes, "completion audio tokens") ||
			!strings.Contains(notes, "prediction tokens") {
			t.Errorf("%s 未报 chat 细分丢弃：%q", name, notes)
		}
		if strings.Contains(notes, "web search") {
			t.Errorf("%s 误报不存在的服务端工具细分：%q", name, notes)
		}
	}
	// 原生形态闭嘴：回本族一维不丢，一条注记都不该有。
	if notes := streamNotes(t, "anthropic", anthropicDimsUsage(), true); notes != "" {
		t.Errorf("anthropic 本族细分误报：%q", notes)
	}
	if notes := streamNotes(t, "openai-chat", chatDimsUsage(), true); notes != "" {
		t.Errorf("chat 本族细分误报：%q", notes)
	}
}

// chat 客户端没 opt-in usage 帧时：usage 帧压根没发，细分注记同口径闭嘴
// （与 droppedCacheDetails 的门控一致）。
func TestUsageDimsGatedByIncludeUsage(t *testing.T) {
	if notes := streamNotes(t, "openai-chat", anthropicDimsUsage(), false); notes != "" {
		t.Errorf("未 opt-in usage 仍报细分丢弃：%q", notes)
	}
}

// 非流式 ResponseNotes 同判据：同一响应 stream=true/false 报出的损耗一致。
func TestUsageDimsResponseNotes(t *testing.T) {
	resp := &ir.Response{ID: "m", Model: "g",
		Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}},
		Usage:   *anthropicDimsUsage()}
	if notes := proto.MustInbound("anthropic").ResponseNotes(resp); len(notes) != 0 {
		t.Errorf("anthropic 本族细分误报：%q", notes)
	}
	for _, name := range []string{"openai-chat", "openai-responses", "gemini"} {
		notes := strings.Join(proto.MustInbound(name).ResponseNotes(resp), "; ")
		if !strings.Contains(notes, "dropped usage detail(s)") || !strings.Contains(notes, "inference geo") {
			t.Errorf("%s 非流式未报细分丢弃：%q", name, notes)
		}
	}
	resp.Usage = *chatDimsUsage()
	if notes := proto.MustInbound("openai-chat").ResponseNotes(resp); len(notes) != 0 {
		t.Errorf("chat 本族细分误报：%q", notes)
	}
	notes := strings.Join(proto.MustInbound("gemini").ResponseNotes(resp), "; ")
	if !strings.Contains(notes, "prediction tokens") {
		t.Errorf("gemini 非流式未报预测 token 丢弃：%q", notes)
	}
}

// 聚合总量不含细分时不报：零值 usage 不许误报。
func TestUsageDimsZeroSilent(t *testing.T) {
	zero := &ir.Usage{InputTokens: 10, OutputTokens: 5}
	for _, name := range []string{"anthropic", "openai-chat", "openai-responses", "gemini"} {
		if notes := streamNotes(t, name, zero, true); strings.Contains(notes, "usage detail") {
			t.Errorf("%s 零细分误报：%q", name, notes)
		}
	}
}
