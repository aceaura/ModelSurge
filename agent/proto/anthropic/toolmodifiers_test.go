package anthropic

import (
	"strings"
	"testing"
)

// R62：anthropic 工具定义 2026 修饰四维（defer_loading /
// eager_input_streaming / input_examples / allowed_callers）同族贯通。
// 官方 SDK Tool 接口核对（2026-09-22）；其余协议没有这些槽位。

func TestToolModifiersRoundTrip(t *testing.T) {
	body := `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hi"}],` +
		`"tools":[{"name":"ping","input_schema":{"type":"object"},` +
		`"defer_loading":true,"eager_input_streaming":false,` +
		`"input_examples":[{"name":"x"}],"allowed_callers":["direct"]},` +
		`{"name":"plain","input_schema":{"type":"object"}}]}`
	r, err := (codec{}).DecodeRequest([]byte(body))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	tl := r.Tools[0]
	if !tl.DeferLoading {
		t.Error("defer_loading 未解出")
	}
	if tl.EagerInputStreaming == nil || *tl.EagerInputStreaming {
		t.Errorf("eager_input_streaming 显式 false 应保真：%+v", tl.EagerInputStreaming)
	}
	if len(tl.InputExamples) != 1 || string(tl.InputExamples[0]) != `{"name":"x"}` {
		t.Errorf("input_examples = %s", tl.InputExamples)
	}
	if len(tl.AllowedCallers) != 1 || tl.AllowedCallers[0] != "direct" {
		t.Errorf("allowed_callers = %v", tl.AllowedCallers)
	}
	// 第二件工具全缺省
	if r.Tools[1].DeferLoading || r.Tools[1].EagerInputStreaming != nil ||
		len(r.Tools[1].InputExamples) > 0 || len(r.Tools[1].AllowedCallers) > 0 {
		t.Errorf("缺省工具解出了东西：%+v", r.Tools[1])
	}
	out, err := (codec{}).EncodeRequest(r)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	s := string(out)
	for _, probe := range []string{`"defer_loading":true`, `"eager_input_streaming":false`,
		`"input_examples":[{"name":"x"}]`, `"allowed_callers":["direct"]`} {
		if !strings.Contains(s, probe) {
			t.Errorf("缺 %q: %s", probe, s)
		}
	}
	// plain 工具不得发明修饰键
	if strings.Count(s, `"defer_loading"`) != 1 || strings.Count(s, `"eager_input_streaming"`) != 1 {
		t.Errorf("缺省工具发明了修饰键: %s", s)
	}
}
