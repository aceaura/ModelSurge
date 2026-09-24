package openaichat

import (
	"strings"
	"testing"
)

// R106-A2 响应侧 logprobs 探测：请求侧开关早已贯通，但上游算出来的逐 token
// 概率在 IR 响应模型里没有槽位——此前静默丢弃，现在计数报注记。

// 流式：携带 logprobs 的 chunk 逐帧计数；显式 null 不算（缺省语义）。
func TestStreamLogprobsCounted(t *testing.T) {
	dec := codec{}.NewStreamDecoder()
	frames := []string{
		`{"id":"c1","model":"g","choices":[{"index":0,"delta":{"content":"hi"},"logprobs":{"content":[{"token":"hi","logprob":-0.1}]}}]}`,
		`{"id":"c1","model":"g","choices":[{"index":0,"delta":{"content":"!"},"logprobs":{"content":[{"token":"!","logprob":-0.2}]}}]}`,
		`{"id":"c1","model":"g","choices":[{"index":0,"delta":{},"finish_reason":"stop","logprobs":null}]}`,
	}
	for _, f := range frames {
		if _, err := dec.Feed("", f); err != nil {
			t.Fatalf("Feed: %v", err)
		}
	}
	notes := dec.(interface{ Notes() []string }).Notes()
	if len(notes) != 1 || !strings.Contains(notes[0], "2 logprobs payload(s)") {
		t.Fatalf("logprobs 计数注记不对：%q", notes)
	}
}

// 非流式：choice.logprobs 进 notes，正文照旧解出。
func TestNonStreamLogprobsNoted(t *testing.T) {
	resp, notes, err := (codec{}).DecodeResponseWithNotes([]byte(
		`{"id":"c1","model":"g","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},` +
			`"finish_reason":"stop","logprobs":{"content":[{"token":"hi","logprob":-0.1}]}}]}`))
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

// 零注记基线：不带 logprobs 的正常响应不许误报。
func TestNoLogprobsNoNote(t *testing.T) {
	_, notes, err := (codec{}).DecodeResponseWithNotes([]byte(
		`{"id":"c1","model":"g","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 0 {
		t.Errorf("无 logprobs 误报：%q", notes)
	}
	dec := codec{}.NewStreamDecoder()
	if _, err := dec.Feed("", `{"id":"c1","model":"g","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":"stop"}]}`); err != nil {
		t.Fatal(err)
	}
	if n := dec.(interface{ Notes() []string }).Notes(); len(n) != 0 {
		t.Errorf("流式无 logprobs 误报：%q", n)
	}
}
