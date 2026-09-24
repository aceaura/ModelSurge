package relay

import (
	"net/http/httptest"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
	"github.com/aceaura/ModelSurge/agent/proto"
)

// 合成事件流（上游给整份 JSON、客户端要 SSE）必须与真流式出口发同一套
// SSE 头：缺 Cache-Control/Connection 时，中间代理与客户端会把这条流当成
// 可缓存的普通响应处理。
func TestWriteResponseSynthStreamSSEHeaders(t *testing.T) {
	w := httptest.NewRecorder()
	resp := &ir.Response{ID: "m1", Model: "m", StopReason: ir.StopEndTurn,
		Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}
	writeResponse(w, proto.MustInbound("anthropic"), &ir.Request{}, resp, true, nil)
	for k, want := range map[string]string{
		"Content-Type":      "text/event-stream",
		"Cache-Control":     "no-cache",
		"Connection":        "keep-alive",
		"X-Accel-Buffering": "no",
	} {
		if got := w.Header().Get(k); got != want {
			t.Errorf("%s = %q，要 %q", k, got, want)
		}
	}
}
