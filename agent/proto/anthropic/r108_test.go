package anthropic

import (
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// R108-甲1 model_context_window_exceeded 双向映射：解码进 IR 独立档位，
// 同族编码原值带回（不并进 max_tokens——两者客户端补救动作相反）。
func TestR108ContextWindowStopRoundTrip(t *testing.T) {
	if got := MapStopReason("model_context_window_exceeded"); got != ir.StopContextWindow {
		t.Fatalf("MapStopReason = %q", got)
	}
	if got := UnmapStopReason(ir.StopContextWindow); got != "model_context_window_exceeded" {
		t.Fatalf("UnmapStopReason = %q", got)
	}
	if !ir.StopContextWindow.Incomplete() {
		t.Fatal("context_window_exceeded 是不完整终态，Incomplete() 必须为真")
	}
}

// R108-乙5 output_config.task_budget 同族原文往返：beta 结构原文透传，
// 一个字节不动；没给不造键。
func TestR108TaskBudgetRoundTrip(t *testing.T) {
	body := []byte(`{"model":"m","max_tokens":1,"messages":[{"role":"user","content":"hi"}],` +
		`"output_config":{"task_budget":{"type":"tokens","total":50000,"remaining":32000}}}`)
	req, err := New().DecodeRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if string(req.TaskBudget) != `{"type":"tokens","total":50000,"remaining":32000}` {
		t.Fatalf("TaskBudget = %s", req.TaskBudget)
	}
	out, err := New().EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"task_budget":{"type":"tokens","total":50000,"remaining":32000}`) {
		t.Fatalf("task_budget 没原样回写：%s", out)
	}

	plain, err := New().DecodeRequest([]byte(`{"model":"m","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	out2, err := New().EncodeRequest(plain)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out2), "task_budget") {
		t.Fatalf("没给被伪造：%s", out2)
	}
}

// R108-乙5 显式 null 按没给处理：Clone 的 JSON 往返不会把 null 当真值带回。
func TestR108TaskBudgetNullSilent(t *testing.T) {
	req, err := New().DecodeRequest([]byte(`{"model":"m","max_tokens":1,"messages":[{"role":"user","content":"hi"}],"output_config":{"task_budget":null}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(req.TaskBudget) != 0 {
		t.Fatalf("null 被当真值：%s", req.TaskBudget)
	}
}

// R108-乙8 beta usage.speed 回显双向：解码进 IR，同族编码写回；
// 没给不造键。
func TestR108UsageSpeedRoundTrip(t *testing.T) {
	resp := []byte(`{"id":"msg_1","type":"message","role":"assistant","model":"m",` +
		`"content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn",` +
		`"usage":{"input_tokens":10,"output_tokens":5,"speed":"fast"}}`)
	r, err := New().DecodeResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	if r.Usage.Speed != "fast" {
		t.Fatalf("Speed = %q", r.Usage.Speed)
	}
	out, err := New().EncodeResponse(r)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"speed":"fast"`) {
		t.Fatalf("speed 没回写：%s", out)
	}

	r2, err := New().DecodeResponse([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"m",` +
		`"content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	if err != nil {
		t.Fatal(err)
	}
	out2, err := New().EncodeResponse(r2)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out2), "speed") {
		t.Fatalf("没给被伪造：%s", out2)
	}
}
