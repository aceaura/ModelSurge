// streamusageoptin_test.go 流式 usage 帧的客户端 opt-in 语义跨族校验。
//
// 四族里只有 Chat 把 usage 做成客户端可选（stream_options.include_usage）：
// 该帧的 choices 是空数组，OpenAI 契约规定只在 opt-in 时出现，没要的客户端按
// choices[0] 取增量会越界。Anthropic/Gemini/Responses 的 usage 是协议内建、
// 无条件回传，所以 opt-in 标志绝不能退化成全局的 usage 抑制开关。
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

func usageEvents(start, delta ir.Usage) []ir.Event {
	return []ir.Event{
		{Type: ir.EvMessageStart, MessageID: "m1", Model: "m", Usage: &start},
		{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockText}},
		{Type: ir.EvTextDelta, Index: 0, Text: "hi"},
		{Type: ir.EvBlockStop, Index: 0},
		{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn, Usage: &delta},
		{Type: ir.EvMessageStop},
	}
}

// encodeOptIn 按客户端意图建编码器并跑完整事件流，返回 wire 字节与损耗注记。
func encodeOptIn(t *testing.T, name string, events []ir.Event, includeUsage bool) (string, []string) {
	t.Helper()
	enc := proto.NewClientStreamEncoder(proto.MustInbound(name), &ir.Request{IncludeUsage: includeUsage})
	var out strings.Builder
	for _, ev := range events {
		frames, err := enc.Encode(ev)
		if err != nil {
			t.Fatalf("%s Encode(%s): %v", name, ev.Type, err)
		}
		for _, f := range frames {
			out.Write(f)
		}
	}
	for _, f := range enc.Finish() {
		out.Write(f)
	}
	return out.String(), enc.Notes()
}

// 各族 usage 在 wire 上的落地标记（start=100 输入 / delta=5 输出）。
func usageMarker(name string) string {
	switch name {
	case "openai-chat":
		return `"prompt_tokens":100`
	case "gemini":
		return `"promptTokenCount":100`
	default: // anthropic 与 openai-responses 都叫 input_tokens
		return `"input_tokens":100`
	}
}

func TestUsageOptInOnlyGatesChat(t *testing.T) {
	events := usageEvents(ir.Usage{InputTokens: 100}, ir.Usage{OutputTokens: 5})
	for _, name := range []string{"openai-chat", "anthropic", "openai-responses", "gemini"} {
		for _, optIn := range []bool{false, true} {
			t.Run(name+"/optIn="+boolName(optIn), func(t *testing.T) {
				out, _ := encodeOptIn(t, name, events, optIn)
				marker := usageMarker(name)
				// 只有 Chat 受 opt-in 门控；其余三族两种取值都必须带 usage。
				want := optIn || name != "openai-chat"
				if got := strings.Contains(out, marker); got != want {
					t.Fatalf("usage 出现=%v，want %v（marker=%s）\n%s", got, want, marker, out)
				}
			})
		}
	}
}

// Chat 未 opt-in 时，流里不得出现任何 choices 为空数组的帧——那正是 usage-only
// 帧的指纹，也是严格客户端越界的原因。
func TestChatWithoutOptInEmitsNoEmptyChoicesFrame(t *testing.T) {
	events := usageEvents(ir.Usage{InputTokens: 100}, ir.Usage{OutputTokens: 5})
	out, _ := encodeOptIn(t, "openai-chat", events, false)
	if strings.Contains(out, `"choices":[]`) {
		t.Fatalf("未 opt-in 却发了 choices 为空数组的帧：\n%s", out)
	}
	// 正文与终止帧不受影响：抑制的只是 usage 帧。
	for _, want := range []string{`"content":"hi"`, `"finish_reason":"stop"`, "data: [DONE]"} {
		if !strings.Contains(out, want) {
			t.Fatalf("流缺少 %s：\n%s", want, out)
		}
	}
}

// opt-in 时 usage 帧必须是独立的一帧（choices 空数组 + usage），且数值按 OpenAI
// 口径：prompt_tokens 是含缓存的总输入。
func TestChatWithOptInEmitsUsageOnlyFrame(t *testing.T) {
	events := usageEvents(
		ir.Usage{InputTokens: 100, CacheReadTokens: 40, CacheCreationTokens: 30},
		ir.Usage{OutputTokens: 5},
	)
	out, _ := encodeOptIn(t, "openai-chat", events, true)
	if !strings.Contains(out, `"choices":[],"usage":`) {
		t.Fatalf("opt-in 却没发 usage-only 帧：\n%s", out)
	}
	for _, want := range []string{`"prompt_tokens":170`, `"completion_tokens":5`, `"total_tokens":175`, `"cached_tokens":40`} {
		if !strings.Contains(out, want) {
			t.Fatalf("usage 帧缺少 %s：\n%s", want, out)
		}
	}
}

// 缓存 TTL 明细的损耗注记只在真的发了 usage 帧时才报：客户端没要 usage 时，
// 那个帧压根没发出去，报「TTL 明细被丢」是在说一件从未请求的载荷。
func TestCacheCreationNoteFollowsUsageFrame(t *testing.T) {
	events := usageEvents(
		ir.Usage{InputTokens: 100, CacheCreationTokens: 30, CacheCreation5mTokens: 30, CacheCreationDetailsKnown: true},
		ir.Usage{OutputTokens: 5},
	)
	const noteFragment = "cache-creation TTL details"
	for _, optIn := range []bool{false, true} {
		t.Run("optIn="+boolName(optIn), func(t *testing.T) {
			out, notes := encodeOptIn(t, "openai-chat", events, optIn)
			has := false
			for _, n := range notes {
				if strings.Contains(n, noteFragment) {
					has = true
				}
			}
			if has != optIn {
				t.Fatalf("TTL 损耗注记出现=%v，want %v（notes=%v）", has, optIn, notes)
			}
			if !optIn && strings.Contains(out, noteFragment) {
				t.Fatalf("未 opt-in 却把注记写进了流：\n%s", out)
			}
		})
	}
}

// nil 请求按默认意图（不发 usage 帧），不得 panic：writeResponse 的非流式路径
// 就传 nil。
func TestNewClientStreamEncoderToleratesNilRequest(t *testing.T) {
	enc := proto.NewClientStreamEncoder(proto.MustInbound("openai-chat"), nil)
	frames, err := enc.Encode(ir.Event{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn,
		Usage: &ir.Usage{InputTokens: 1, OutputTokens: 1}})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	for _, f := range frames {
		if strings.Contains(string(f), `"choices":[]`) {
			t.Fatalf("nil 请求下仍发了 usage 帧：%s", f)
		}
	}
}

// 没实现 UsageOptIn 的编码器必须被助手原样放行（否则三族会因为类型断言失败
// 而拿不到编码器）。
func TestNewClientStreamEncoderPassesThroughNonOptInCodecs(t *testing.T) {
	for _, name := range []string{"anthropic", "gemini", "openai-responses"} {
		enc := proto.NewClientStreamEncoder(proto.MustInbound(name), &ir.Request{IncludeUsage: true})
		if _, ok := enc.(proto.UsageOptIn); ok {
			t.Fatalf("%s 意外实现了 UsageOptIn：该协议的 usage 是内建的，不该有开关", name)
		}
	}
	if _, ok := proto.NewClientStreamEncoder(proto.MustInbound("openai-chat"),
		&ir.Request{IncludeUsage: true}).(proto.UsageOptIn); !ok {
		t.Fatalf("openai-chat 没实现 UsageOptIn，客户端意图无从下发")
	}
}

func boolName(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
