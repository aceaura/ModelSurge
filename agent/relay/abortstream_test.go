package relay

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/config"
	"github.com/aceaura/ModelSurge/agent/ir"
	"github.com/aceaura/ModelSurge/agent/proto"
)

// abortFrames 上游吐到一半就关流：有 message_start / block_start / text_delta，
// 没有 message_delta 与 message_stop。这是代理超时、上游崩溃的常见形态。
func abortFrames() []string {
	return sseFrames("半截")[:3]
}

// forwardSSE 把给定上游帧跑一遍完整转发，返回客户端（anthropic 入站）实际
// 收到的字节。
func forwardSSE(t *testing.T, frames []string) string {
	t.Helper()
	up := sseUpstream(t, frames, nil)
	replay := &thinkNotesReplay{lease: sseLease(up.URL)}
	f := NewForwarder(&config.Config{}, replay, nil)
	w := httptest.NewRecorder()
	f.Forward(t.Context(), w, proto.MustInbound("anthropic"), &ir.Request{
		Model: "m", MaxTokens: 64, Stream: true,
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}},
	}, "client-key")
	return w.Body.String()
}

// 上游吐了半截正文就断了，客户端拿到的流必须带「输出不完整」的收尾档。
// 报 end_turn 时 agent harness 会把半截代码当成模型的最终答复提交，
// 而正确动作是重试。收尾帧的 usage 还必须来自解码器——它才拿得到上游
// message_start 里的 input_tokens，否则断流那一轮的计费直接丢掉。
func TestAbortedUpstreamStreamNotReportedAsNormalCompletion(t *testing.T) {
	s := forwardSSE(t, abortFrames())
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
	for _, want := range []string{`"type":"content_block_stop"`, `"type":"message_stop"`} {
		if !strings.Contains(s, want) {
			t.Fatalf("骨架不完整，缺 %s：\n%s", want, s)
		}
	}
	// 断言必须落在 message_delta 这一帧上：input_tokens 也出现在 message_start
	// 里，整体查串会把「解码器兜底被跳过、收尾帧由编码器凭空造」放过去。
	i := strings.Index(s, `"type":"message_delta"`)
	if i < 0 {
		t.Fatalf("没有收尾帧：\n%s", s)
	}
	if tail := s[i:]; !strings.Contains(tail, `"input_tokens":7`) {
		t.Fatalf("收尾帧没带上游 usage（解码器兜底被跳过）：\n%s", tail)
	}
}

// 上游正常收尾时不得反过来被当成中断：这条守住上面那条修复的另一侧，
// 否则每一轮正常对话都会被客户端判定为需要重试。
func TestCompletedUpstreamStreamKeepsNormalStop(t *testing.T) {
	s := forwardSSE(t, sseFrames("完整"))
	if !strings.Contains(s, `"stop_reason":"end_turn"`) {
		t.Fatalf("正常完成被报成了中断：\n%s", s)
	}
	if strings.Contains(s, `"stop_reason":"max_tokens"`) {
		t.Fatalf("正常流里混进了中断档：\n%s", s)
	}
}

var errClientGone = errors.New("client connection is gone")

// brokenWriter 从第 failAt 次 Write 起报错，模拟客户端在流中途断开。
type brokenWriter struct {
	http.ResponseWriter
	writes int
	failAt int
}

func (w *brokenWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.writes >= w.failAt {
		return 0, errClientGone
	}
	return w.ResponseWriter.Write(p)
}

// 客户端断开是断流保真的第三种触发源（另两种是上游断、上游报错）。转发循环
// 必须在第一次写失败时停下：继续读上游等于替一个已经不存在的读者烧配额，
// 而写出的每一帧都只会再失败一次。
func TestClientDisconnectStopsUpstreamConsumption(t *testing.T) {
	up := sseUpstream(t, sseFrames("正文"), nil)
	replay := &thinkNotesReplay{lease: sseLease(up.URL)}
	f := NewForwarder(&config.Config{}, replay, nil)
	rec := httptest.NewRecorder()
	w := &brokenWriter{ResponseWriter: rec, failAt: 2}
	f.Forward(t.Context(), w, proto.MustInbound("anthropic"), &ir.Request{
		Model: "m", MaxTokens: 64, Stream: true,
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}},
	}, "client-key")
	if w.writes != 2 {
		t.Errorf("写失败后仍在继续消费上游：writes=%d, want 2", w.writes)
	}
	if strings.Contains(rec.Body.String(), `"stop_reason":"end_turn"`) {
		t.Errorf("客户端断开被当成了正常完成：%s", rec.Body.String())
	}
}
