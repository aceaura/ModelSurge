package relay

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/config"
	"github.com/aceaura/ModelSurge/agent/ir"
	"github.com/aceaura/ModelSurge/agent/proto"
	"github.com/aceaura/ModelSurge/replay/contract/replayv1"
)

// forwardKiroEvents 把给定 IR 事件当成上游 ndjson 流跑一遍完整转发，
// 返回客户端（anthropic 入站）实际收到的字节。
func forwardKiroEvents(t *testing.T, events []ir.Event) string {
	t.Helper()
	var body bytes.Buffer
	for _, ev := range events {
		if err := json.NewEncoder(&body).Encode(ev); err != nil {
			t.Fatal(err)
		}
	}
	replay := &kiroExecuteReplay{
		lease: replayv1.TargetLease{RequestID: "req", GroupID: "group", TargetID: "kiro/public", Protocol: "kiro"},
		body:  io.NopCloser(bytes.NewReader(body.Bytes())),
	}
	forwarder := NewForwarder(&config.Config{}, replay, nil)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarder.Forward(r.Context(), w, proto.MustInbound("anthropic"), &ir.Request{
			Model: "public", Stream: true,
			Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}},
		}, "client-key")
	}))
	defer server.Close()

	resp, err := http.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// 上游吐了半截正文就断了（代理超时、上游崩溃的常见形态），客户端拿到的流
// 必须带「输出不完整」的收尾档。报 end_turn 时 agent harness 会把半截代码
// 当成模型的最终答复提交，而正确动作是重试。
func TestAbortedUpstreamStreamNotReportedAsNormalCompletion(t *testing.T) {
	s := forwardKiroEvents(t, []ir.Event{
		{Type: ir.EvMessageStart, MessageID: "msg", Model: "public"},
		{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockText}},
		{Type: ir.EvTextDelta, Index: 0, Text: "半截"},
		// 无 EvMessageDelta / EvMessageStop：上游直接断了
	})
	if !strings.Contains(s, "半截") {
		t.Fatalf("已下发的正文丢了：\n%s", s)
	}
	if strings.Contains(s, `"stop_reason":"end_turn"`) {
		t.Fatalf("断流被伪装成正常完成：\n%s", s)
	}
	if !strings.Contains(s, `"stop_reason":"max_tokens"`) {
		t.Fatalf("收尾帧没带输出不完整档：\n%s", s)
	}
	// 未闭合的块仍要关，否则客户端解析器一直等 content_block_stop
	if !strings.Contains(s, `"type":"content_block_stop"`) {
		t.Fatalf("未闭块没关：\n%s", s)
	}
}

// 真 SSE 上游中途断流（kiro 走的是 ndjson 旁路，这条覆盖 HTTP/SSE 主路）。
// 上游给过 message_start 的 usage，收尾帧必须把它带上：这既证明收尾档来自
// 解码器（它才有 usage），也保证断流那一轮的计费不丢。
func TestAbortedSSEUpstreamFinishesFromDecoder(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, fr := range []string{
			`event: message_start` + "\n" + `data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"native","content":[],"usage":{"input_tokens":11}}}`,
			`event: content_block_start` + "\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"半截"}}`,
		} {
			_, _ = w.Write([]byte(fr + "\n\n"))
			w.(http.Flusher).Flush()
		}
		// 这里直接返回 = 上游关流，没有 message_delta / message_stop
	}))
	defer up.Close()

	replay := &thinkNotesReplay{lease: replayv1.TargetLease{
		RequestID: "req", GroupID: "g", TargetID: "anthropic-1", Protocol: "anthropic",
		NativeModel: "native", BaseURL: up.URL, Credential: "sk-up",
	}}
	f := NewForwarder(&config.Config{}, replay, nil)
	w := httptest.NewRecorder()
	f.Forward(t.Context(), w, proto.MustInbound("anthropic"), &ir.Request{
		Model: "m", MaxTokens: 64, Stream: true,
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}},
	}, "client-key")

	s := w.Body.String()
	if !strings.Contains(s, "半截") {
		t.Fatalf("已下发的正文丢了：\n%s", s)
	}
	if strings.Contains(s, `"stop_reason":"end_turn"`) {
		t.Fatalf("断流被伪装成正常完成：\n%s", s)
	}
	if !strings.Contains(s, `"stop_reason":"max_tokens"`) {
		t.Fatalf("收尾帧没带输出不完整档：\n%s", s)
	}
	// 断言必须落在 message_delta 这一帧上：input_tokens 也出现在 message_start
	// 里，整体查串会把「解码器兜底被跳过、收尾帧由编码器凭空造」放过去。
	i := strings.Index(s, `"type":"message_delta"`)
	if i < 0 {
		t.Fatalf("没有收尾帧：\n%s", s)
	}
	if tail := s[i:]; !strings.Contains(tail, `"input_tokens":11`) {
		t.Fatalf("收尾帧没带上游 usage（解码器兜底被跳过）：\n%s", tail)
	}
	for _, want := range []string{`"type":"content_block_stop"`, `"type":"message_stop"`} {
		if !strings.Contains(s, want) {
			t.Fatalf("骨架不完整，缺 %s：\n%s", want, s)
		}
	}
}

// 上游正常收尾时不得反过来被当成中断：这条守住上面那条修复的另一侧，
// 否则每一轮正常对话都会被客户端判定为需要重试。
func TestCompletedUpstreamStreamKeepsNormalStop(t *testing.T) {
	s := forwardKiroEvents(t, []ir.Event{
		{Type: ir.EvMessageStart, MessageID: "msg", Model: "public"},
		{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockText}},
		{Type: ir.EvTextDelta, Index: 0, Text: "完整"},
		{Type: ir.EvBlockStop, Index: 0},
		{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn, Usage: &ir.Usage{OutputTokens: 3}},
		{Type: ir.EvMessageStop},
	})
	if !strings.Contains(s, `"stop_reason":"end_turn"`) {
		t.Fatalf("正常完成被报成了中断：\n%s", s)
	}
	if strings.Contains(s, `"stop_reason":"max_tokens"`) {
		t.Fatalf("正常流里混进了中断档：\n%s", s)
	}
}
