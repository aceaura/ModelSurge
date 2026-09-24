package openairesponses

import (
	"strings"
	"testing"
)

// R106-A2 响应侧 logprobs 探测：output_text part 的 logprobs 数组在 IR 里
// 没有槽位——此前静默丢弃，现在计数报注记。

// 流式：output_item.done 的 message content part 携带 logprobs 时计数；
// 正文回补不受影响。
func TestStreamLogprobsCounted(t *testing.T) {
	dec := New().NewStreamDecoder()
	evs := feedAll(t, dec,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"m1","role":"assistant",`+
			`"content":[{"type":"output_text","text":"hi","logprobs":[{"token":"hi","logprob":-0.1}]}]}}`,
		`{"type":"response.completed","response":{"id":"r1","status":"completed"}}`,
	)
	if len(evs) == 0 {
		t.Fatal("正文回补没产出事件")
	}
	notes := dec.(interface{ Notes() []string }).Notes()
	if len(notes) != 1 || !strings.Contains(notes[0], "1 logprobs payload(s)") {
		t.Fatalf("logprobs 计数注记不对：%q", notes)
	}
}

// 非流式：output 数组里带 logprobs 的 part 计数进 notes。
func TestNonStreamLogprobsNoted(t *testing.T) {
	resp, notes, err := (codec{}).DecodeResponseWithNotes([]byte(
		`{"id":"r1","model":"g","status":"completed","output":[` +
			`{"type":"message","id":"m1","role":"assistant","content":[` +
			`{"type":"output_text","text":"a","logprobs":[{"token":"a","logprob":-0.1}]},` +
			`{"type":"output_text","text":"b"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Content) == 0 {
		t.Fatal("正文没解出来")
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "1 logprobs payload(s)") {
		t.Fatalf("非流式 logprobs 注记不对：%q", notes)
	}
}

// 零注记基线：不带 logprobs 的响应不许误报。
func TestNoLogprobsNoNote(t *testing.T) {
	_, notes, err := (codec{}).DecodeResponseWithNotes([]byte(
		`{"id":"r1","model":"g","status":"completed","output":[` +
			`{"type":"message","id":"m1","role":"assistant","content":[{"type":"output_text","text":"a"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 0 {
		t.Errorf("无 logprobs 误报：%q", notes)
	}
}
