package relay

import (
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aceaura/ModelSurge/agent/config"
	"github.com/aceaura/ModelSurge/agent/ir"
	"github.com/aceaura/ModelSurge/agent/proto"
)

// R104 流式路径的响应侧损耗与终止纪律。

// poolForwardTo 同 poolForward，但客户端协议可选（terminalErr 用例需要
// responses 客户端才能触发编码器报错）。
func poolForwardTo(t *testing.T, rp *poolReplay, clientProto, model string, stream bool) *httptest.ResponseRecorder {
	t.Helper()
	f := NewForwarder(&config.Config{}, rp, nil)
	w := httptest.NewRecorder()
	f.Forward(t.Context(), w, proto.MustInbound(clientProto), &ir.Request{
		Model: model, MaxTokens: 64, Stream: stream,
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}},
	}, "client-key")
	return w
}

// 单帧畸形在流式路径同样跳帧续流：正文照发，损耗以 SSE 注释帧报出，
// 不得升级成错误帧终止整条流。
func TestStreamMalformedFrameSkippedWithNote(t *testing.T) {
	var hits atomic.Int32
	up := sseUpstream(t, []string{
		poolStart, poolText,
		`event: content_block_delta` + "\n" + `data: {截断的畸形 JSON`,
		`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"后半"}}`,
	}, &hits)
	rp := &poolReplay{baseURL: up.URL, limit: poolLimit}
	w := poolForward(t, rp, "m", true)

	body := w.Body.String()
	if !strings.Contains(body, "后半") {
		t.Errorf("坏帧之后的正文没续上：%s", body)
	}
	if !strings.Contains(body, ": modelsurge-note: skipped 1 malformed upstream SSE frame(s)") {
		t.Errorf("跳帧损耗没落 SSE 注释帧：%s", body)
	}
	if strings.Contains(body, `"type":"error"`) {
		t.Errorf("单帧畸形被升级成错误帧：%s", body)
	}
}

// 客户端编码器报错（responses 客户端收到未开块的 text delta）后：
// 渲染错误帧并置 terminalErr——后续事件一律不再下发，enc.Finish() 的
// completed 帧同样跳过，否则错误帧之后又漏出正文/正常收尾。
func TestStreamClientEncodeErrorTerminatesOutput(t *testing.T) {
	var hits atomic.Int32
	up := sseUpstream(t, []string{
		poolStart,
		// 没有 content_block_start 直接来 delta：anthropic 解码器照发
		// EvTextDelta，responses 客户端编码器对它必报错（unopened block）。
		`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"孤儿增量"}}`,
		poolText,
		`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"不该再下发"}}`,
	}, &hits)
	rp := &poolReplay{baseURL: up.URL, limit: poolLimit}
	w := poolForwardTo(t, rp, "openai-responses", "m", true)

	body := w.Body.String()
	if !strings.Contains(body, "event: error") {
		t.Fatalf("编码错误没渲染成错误帧：%s", body)
	}
	if strings.Contains(body, "不该再下发") {
		t.Errorf("terminalErr 之后还在下发事件：%s", body)
	}
	if strings.Contains(body, "response.completed") {
		t.Errorf("错误之后 Finish 补了 completed 帧：%s", body)
	}
}

// retention 跨族丢弃必须报出来：目标协议没有缓存留存槽位时，
// 客户端的 prompt_cache_retention 静默失效就是计费口径漂移。
func TestDiagnoseNotesDroppedPromptCacheRetention(t *testing.T) {
	req := &ir.Request{PromptCacheRetention: "24h"}
	notes := Diagnose(req, "anthropic", capsOf(t, "anthropic"))
	found := false
	for _, n := range notes {
		if strings.Contains(n, "dropped prompt cache retention") {
			found = true
		}
	}
	if !found {
		t.Fatalf("retention 丢弃没报：%v", notes)
	}
	// 同族（有槽位）不许误报。
	for _, n := range Diagnose(req, "openai-chat", capsOf(t, "openai-chat")) {
		if strings.Contains(n, "dropped prompt cache retention") {
			t.Fatalf("同族被误报：%q", n)
		}
	}
}
