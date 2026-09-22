package proto

import (
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// 编码侧 EvError 的终止性：错误帧就是终止帧。Finish() 仍要关掉开着的块（不给客户端
// 留永不结束的 item），但不得再补「正常收尾」。补了的话，一个上游限流会被翻译成
// stop_reason=max_tokens / finish_reason=length / incomplete_details=max_output_tokens
// ——客户端读到的是「你输出超长了，请加大 max_output_tokens」，照做之后再次失败，
// 死循环；紧随其后的 message_stop / [DONE] 还把失败伪装成正常结束。
//
// 四族的下落形态不同，故逐族给出两组子串：forbidden 锁本轮修复（错误之后禁止出现），
// abort 锁 R48 的中断档语义（真断流时必须出现）——修复只许在 EvError 之后生效。

func ecRun(t *testing.T, name string, evs []ir.Event) (stream, finish string) {
	t.Helper()
	enc := MustInbound(name).NewStreamEncoder()
	var sb, fb strings.Builder
	for _, ev := range evs {
		frames, err := enc.Encode(ev)
		if err != nil {
			t.Fatalf("%s: encode %q: %v", name, ev.Type, err)
		}
		for _, f := range frames {
			sb.Write(f)
		}
	}
	for _, f := range enc.Finish() {
		fb.Write(f)
	}
	return sb.String(), fb.String()
}

func ecErrEvent() ir.Event {
	return ir.Event{Type: ir.EvError, Err: &ir.Error{
		Type: ir.ErrTypeRateLimit, Code: "rate_limit_exceeded", Message: "boom", Retryable: true,
	}}
}

func ecTextStream(withErr bool) []ir.Event {
	evs := []ir.Event{
		{Type: ir.EvMessageStart, MessageID: "m1", Model: "mod"},
		{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockText}},
		{Type: ir.EvTextDelta, Index: 0, Text: "前半"},
	}
	if withErr {
		return append(evs, ecErrEvent())
	}
	return evs
}

func ecToolStream(withErr bool) []ir.Event {
	evs := []ir.Event{
		{Type: ir.EvMessageStart, MessageID: "m1", Model: "mod"},
		{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: "t1", Name: "get"}}},
		{Type: ir.EvToolInput, Index: 0, Text: `{"q":`},
	}
	if withErr {
		return append(evs, ecErrEvent())
	}
	return evs
}

var ecCases = []struct {
	name string
	// forbidden 错误帧之后 Finish() 不得产出的子串。
	forbidden []string
	// required 错误帧之后 Finish() 仍须产出的子串（关块，不留永不结束的 item）。
	required []string
	// abort 真断流（无 EvError）时 Finish() 必须产出的中断档标记。
	abort []string
}{
	{
		name:      "anthropic",
		forbidden: []string{"stop_reason", "message_stop"},
		required:  []string{"content_block_stop"},
		abort:     []string{`"stop_reason":"max_tokens"`, "message_stop"},
	},
	{
		name: "openai-chat",
		// RenderStreamError 自带 [DONE]；Finish() 再补一个就是流末两个 [DONE]。
		forbidden: []string{"finish_reason", "[DONE]"},
		abort:     []string{`"finish_reason":"length"`, "[DONE]"},
	},
	{
		name:      "openai-responses",
		forbidden: []string{"incomplete_details", "response.incomplete"},
		required:  []string{"output_item.done"},
		abort:     []string{"incomplete_details"},
	},
	{
		name: "gemini",
		// functionCall 在禁止列：Gemini 的工具调用攒到块结束才整块下发，错误到达时
		// 缓冲里的参数必然被截断，冲刷出去等于让客户端执行一个畸形调用。
		forbidden: []string{"finishReason", "functionCall"},
		abort:     []string{`"finishReason":"MAX_TOKENS"`},
	},
}

func TestEncodeErrorIsTerminalAcrossCodecs(t *testing.T) {
	for _, tc := range ecCases {
		for _, shape := range []struct {
			label string
			evs   func(bool) []ir.Event
		}{{"正文流", ecTextStream}, {"工具流", ecToolStream}} {
			t.Run(tc.name+"/"+shape.label, func(t *testing.T) {
				stream, finish := ecRun(t, tc.name, shape.evs(true))
				if !strings.Contains(stream, "rate_limit_error") {
					t.Errorf("错误帧没带上规范类型，stream=%q", stream)
				}
				for _, bad := range tc.forbidden {
					if strings.Contains(finish, bad) {
						t.Errorf("错误之后不得再补 %q，finish=%q", bad, finish)
					}
				}
				for _, want := range tc.required {
					if !strings.Contains(finish, want) {
						t.Errorf("开着的块仍须由 Finish() 关掉（缺 %q），finish=%q", want, finish)
					}
				}
			})
		}
	}
}

// 对照组：真断流时中断档收尾是正确行为（R48），不得被本轮修复一并抹掉。
// 只跑正文流——工具流的断流收尾各族还有参数残片冲刷，不在此不变量范围内。
func TestEncodeAbortStillFabricatesInterruptTerminal(t *testing.T) {
	for _, tc := range ecCases {
		t.Run(tc.name, func(t *testing.T) {
			_, finish := ecRun(t, tc.name, ecTextStream(false))
			for _, want := range tc.abort {
				if !strings.Contains(finish, want) {
					t.Errorf("真断流必须补中断档 %q，finish=%q", want, finish)
				}
			}
		})
	}
}

// 错误之后不得再吐新内容：Gemini 把 functionCall 攒到块结束才发，是唯一一家
// Finish() 会在错误帧之后产出**新内容**（而非仅关块）的编码器。
func TestGeminiDropsPendingToolCallAfterError(t *testing.T) {
	stream, finish := ecRun(t, "gemini", ecToolStream(true))
	if !strings.Contains(stream, `"error"`) {
		t.Fatalf("没有产出错误帧，stream=%q", stream)
	}
	if strings.Contains(finish, "functionCall") {
		t.Errorf("缓冲的工具调用不得在错误帧之后下发，finish=%q", finish)
	}
	if finish != "" {
		t.Errorf("错误之后 Finish() 应当无输出，实得 %q", finish)
	}
}

// 错误帧之后 Chat 的流末只许有一个 [DONE]：RenderStreamError 已经带了一个。
func TestChatSingleDoneAfterStreamError(t *testing.T) {
	stream, _ := ecRun(t, "openai-chat", ecTextStream(true))
	if n := strings.Count(stream, "[DONE]"); n != 1 {
		t.Errorf("流末 [DONE] 应恰好 1 个，实得 %d，stream=%q", n, stream)
	}
}
