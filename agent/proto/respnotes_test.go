package proto_test

import (
	"encoding/json"
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

// R55 响应侧诊断通道：三类编码损耗（外族签名门控、畸形工具参数挪键、
// 思考抑制）此前只发生在编码器分支里，客户端与运维都看不见。现在
// ResponseNotes（非流式扫描）与 StreamEncoder.Notes（流式计数）把它们报出来。
// 判据与编码分支同源（SignatureGenuineFor / NormalizeToolInput），
// 扫描结果即实编结果。

const r55SigClue = "sig-r55-clue-4q2"

func r55Resp(sigFrom string, args json.RawMessage) *ir.Response {
	return &ir.Response{Content: []ir.Block{
		{Type: ir.BlockThinking, Thinking: &ir.Thinking{Text: "t", Signature: r55SigClue, SignatureFrom: sigFrom}},
		{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: "call-1", Name: "f", Input: args}},
	}}
}

func anyNoteContains(notes []string, sub string) bool {
	for _, n := range notes {
		if strings.Contains(n, sub) {
			return true
		}
	}
	return false
}

// 外族签名在四个有门控的入站都要报丢；kiro 透传签名不报；
// 无槽位的 chat 措辞与「签错家族」不同——读者要改的地方不同。
func TestResponseNotesForeignSignature(t *testing.T) {
	cases := []struct {
		protoName string
		sigFrom   string
		wantSub   string // 空串表示不应出现签名注记
	}{
		{"anthropic", "gemini", "different protocol family"},
		{"openai-chat", "anthropic", "no signature slot"},
		{"openai-responses", "anthropic", "different protocol family"},
		{"codex", "anthropic", "different protocol family"},
		{"gemini", "anthropic", "different protocol family"},
		{"kiro", "anthropic", ""}, // kiro 透传签名不门控
	}
	for _, c := range cases {
		notes := proto.MustInbound(c.protoName).ResponseNotes(r55Resp(c.sigFrom, json.RawMessage(`{"a":1}`)))
		if c.wantSub == "" {
			if anyNoteContains(notes, "signature") {
				t.Errorf("%s: 透传签名却报丢失：%v", c.protoName, notes)
			}
			continue
		}
		if !anyNoteContains(notes, "dropped 1 thought signature(s)") || !anyNoteContains(notes, c.wantSub) {
			t.Errorf("%s: 外族签名未报或措辞错：%v", c.protoName, notes)
		}
	}
}

// 同族真签名不丢，报了就是假阳性（会把运维引向不存在的损耗）。
// chat 无签名槽位，任何签名都丢——包括不可能出现的「本族」签名。
func TestResponseNotesGenuineSignatureSilent(t *testing.T) {
	for _, name := range []string{"anthropic", "gemini", "openai-responses"} {
		notes := proto.MustInbound(name).ResponseNotes(r55Resp(name, json.RawMessage(`{"a":1}`)))
		if anyNoteContains(notes, "signature") {
			t.Errorf("%s: 同族真签名误报丢失：%v", name, notes)
		}
	}
	notes := proto.MustInbound("openai-chat").ResponseNotes(r55Resp("openai-chat", json.RawMessage(`{"a":1}`)))
	if !anyNoteContains(notes, "no signature slot") {
		t.Errorf("openai-chat: 无槽位丢弃未报：%v", notes)
	}
}

// 畸形工具参数：对象槽位（anthropic/gemini/kiro）挪键必报；
// 字符串槽位（chat/responses）原样透传无损，不报。
func TestResponseNotesMalformedArgs(t *testing.T) {
	bad := json.RawMessage(`{"a": 1`) // max_tokens 截断的典型形态
	for _, name := range []string{"anthropic", "gemini", "kiro"} {
		notes := proto.MustInbound(name).ResponseNotes(r55Resp(name, bad))
		if !anyNoteContains(notes, "rewrapped 1 malformed tool call argument(s)") || !anyNoteContains(notes, ir.RawArgsKey) {
			t.Errorf("%s: 挪键未报：%v", name, notes)
		}
	}
	for _, name := range []string{"openai-chat", "openai-responses"} {
		notes := proto.MustInbound(name).ResponseNotes(r55Resp(name, bad))
		if anyNoteContains(notes, "rewrapped") {
			t.Errorf("%s: 字符串槽位透传却报挪键：%v", name, notes)
		}
	}
}

// 合法对象参数 + 无签名：五个入站都不该产出任何注记（静默是默认态）。
func TestResponseNotesCleanResponseSilent(t *testing.T) {
	resp := &ir.Response{Content: []ir.Block{
		{Type: ir.BlockText, Text: "hi"},
		{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: "c", Name: "f", Input: json.RawMessage(`{"a":1}`)}},
	}}
	for _, name := range []string{"anthropic", "gemini", "kiro", "openai-chat", "openai-responses"} {
		if notes := proto.MustInbound(name).ResponseNotes(resp); len(notes) != 0 {
			t.Errorf("%s: 干净响应误报：%v", name, notes)
		}
	}
}

// 流式编码器实编计数：外族签名事件被门控时进 Notes；排干后第二次调用为空。
func TestStreamEncoderNotesForeignSig(t *testing.T) {
	seq := func(sigFrom string) []ir.Event {
		return []ir.Event{
			{Type: ir.EvMessageStart, MessageID: "m1", Model: "m"},
			{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockThinking, Thinking: &ir.Thinking{}}},
			{Type: ir.EvSigDelta, Index: 0, Text: r55SigClue, SignatureFrom: sigFrom},
			{Type: ir.EvBlockStop, Index: 0},
			{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn},
			{Type: ir.EvMessageStop},
		}
	}
	// 外族签名：四个真实编码器都要报（chat 无槽位，其余门控）。
	for _, name := range []string{"anthropic", "openai-chat", "openai-responses", "gemini"} {
		sigFrom := "anthropic"
		if name == "anthropic" {
			sigFrom = "gemini"
		}
		enc := proto.MustInbound(name).NewStreamEncoder()
		for _, ev := range seq(sigFrom) {
			if _, err := enc.Encode(ev); err != nil {
				t.Fatalf("%s: Encode err=%v", name, err)
			}
		}
		enc.Finish()
		notes := enc.Notes()
		if !anyNoteContains(notes, "dropped 1 thought signature(s)") {
			t.Errorf("%s: 门控丢弃未进 Notes：%v", name, notes)
		}
		if again := enc.Notes(); len(again) != 0 {
			t.Errorf("%s: Notes 未排干，第二次还有：%v", name, again)
		}
	}
	// 同族真签名：不丢，不报。
	enc := proto.MustInbound("anthropic").NewStreamEncoder()
	for _, ev := range seq("anthropic") {
		if _, err := enc.Encode(ev); err != nil {
			t.Fatalf("Encode err=%v", err)
		}
	}
	enc.Finish()
	if notes := enc.Notes(); len(notes) != 0 {
		t.Errorf("anthropic 同族签名误报：%v", notes)
	}
}

// gemini 流式编码在 block_stop 处规整工具参数，截断参数挪键要进 Notes。
func TestGeminiStreamEncoderNotesRewrap(t *testing.T) {
	enc := proto.MustInbound("gemini").NewStreamEncoder()
	seq := []ir.Event{
		{Type: ir.EvMessageStart, MessageID: "m1", Model: "m"},
		{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: "c1", Name: "f"}}},
		{Type: ir.EvToolInput, Index: 0, Text: `{"a": 1`},
		{Type: ir.EvBlockStop, Index: 0},
		{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn},
		{Type: ir.EvMessageStop},
	}
	for _, ev := range seq {
		if _, err := enc.Encode(ev); err != nil {
			t.Fatalf("Encode err=%v", err)
		}
	}
	enc.Finish()
	if notes := enc.Notes(); !anyNoteContains(notes, "rewrapped 1 malformed tool call argument(s)") {
		t.Errorf("gemini 挪键未进 Notes：%v", notes)
	}
	// 断流由 Finish 冲刷未闭工具块，同一条规整路径也要记账。
	enc2 := proto.MustInbound("gemini").NewStreamEncoder()
	for _, ev := range seq[:3] {
		if _, err := enc2.Encode(ev); err != nil {
			t.Fatalf("Encode err=%v", err)
		}
	}
	enc2.Finish()
	if notes := enc2.Notes(); !anyNoteContains(notes, "rewrapped 1 malformed tool call argument(s)") {
		t.Errorf("gemini Finish 冲刷路径挪键未进 Notes：%v", notes)
	}
}

// SSE 注释帧是流式方向唯一的注记通道：换行必须洗掉（否则帧格式被注记内容
// 撕开），空注记不产帧。
func TestSSENoteFramesSanitize(t *testing.T) {
	frames := proto.SSENoteFrames([]string{"line1\nline2\r\nline3"})
	if len(frames) != 1 {
		t.Fatalf("帧数 = %d, want 1", len(frames))
	}
	got := string(frames[0])
	if !strings.HasPrefix(got, ": modelsurge-note: ") || !strings.HasSuffix(got, "\n\n") {
		t.Errorf("帧形态错：%q", got)
	}
	payload := strings.TrimSuffix(strings.TrimPrefix(got, ": modelsurge-note: "), "\n\n")
	if strings.ContainsAny(payload, "\r\n") {
		t.Errorf("换行未洗掉：%q", payload)
	}
	if proto.SSENoteFrames(nil) != nil {
		t.Error("空注记不应产帧")
	}
}
