package relay

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/config"
	"github.com/aceaura/ModelSurge/agent/ir"
	"github.com/aceaura/ModelSurge/agent/proto"
)

// R103-2 单个畸形 SSE 帧的处置。旧行为：dec.Feed 报错就包装成 EvError 发给
// 客户端，随后循环继续转发后续帧——客户端先读到错误帧（终止语义，chat 族
// 还自带 [DONE]）又读到正文，拿到一条「[DONE] 之后还有 data」的非法流。
// 新口径（sub2api 同款）：跳过坏帧、流不断，损耗计数在收尾注记里报出。
func TestMalformedUpstreamFrameSkippedStreamContinues(t *testing.T) {
	readLogs := captureLogs(t)
	frames := []string{
		"event: message_start\ndata: " + `{"type":"message_start","message":{"id":"m1","model":"native","usage":{"input_tokens":7}}}`,
		"event: content_block_start\ndata: " + `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		"event: content_block_delta\ndata: " + `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"前段"}}`,
		"event: content_block_delta\ndata: {garbage",
		"event: content_block_delta\ndata: " + `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"后段"}}`,
		"event: content_block_stop\ndata: " + `{"type":"content_block_stop","index":0}`,
		"event: message_delta\ndata: " + `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`,
		"event: message_stop\ndata: " + `{"type":"message_stop"}`,
	}
	up := sseUpstream(t, frames, nil)
	forwarder := NewForwarder(&config.Config{}, &logProbeReplay{lease: sseLease(up.URL)}, nil)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarder.Forward(r.Context(), w, proto.MustInbound("anthropic"), &ir.Request{
			Model: "m", MaxTokens: 64, Stream: true,
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
	body := string(raw)

	if strings.Contains(body, `"type":"error"`) {
		t.Errorf("畸形帧被升级成错误帧发给客户端：\n%s", body)
	}
	for _, want := range []string{"前段", "后段"} {
		if !strings.Contains(body, want) {
			t.Errorf("坏帧之后的正文没续传（缺 %q）：\n%s", want, body)
		}
	}
	if !strings.Contains(body, "message_stop") {
		t.Errorf("流被提前掐断（缺 message_stop）：\n%s", body)
	}
	// 损耗必须报出来（R95 判据）：收尾 SSE 注释帧与日志都要有。
	if !strings.Contains(body, "skipped 1 malformed upstream SSE frame") {
		t.Errorf("跳帧损耗没落到 SSE 注记帧：\n%s", body)
	}
	if !strings.Contains(readLogs(), "skipped 1 malformed upstream SSE frame") {
		t.Errorf("跳帧损耗没落日志：%s", readLogs())
	}
}
