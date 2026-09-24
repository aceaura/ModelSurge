package proto_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
	"github.com/aceaura/ModelSurge/agent/proto"
	"github.com/aceaura/ModelSurge/agent/proto/anthropic"
	"github.com/aceaura/ModelSurge/agent/proto/gemini"
	"github.com/aceaura/ModelSurge/agent/proto/openaichat"
)

// R110-D1 responses 的 incomplete_details.reason="steered"（用户中途转向在安全
// 边界处截断）必须成独立档，且与 max_tokens 分开：那是输出配额耗尽要加大预算
// 重试，steered 加大预算毫无意义。塌成 max_tokens 会让客户端误判补救动作。
func TestR110SteeredIsDistinctIncompleteReason(t *testing.T) {
	body := []byte(`{"id":"resp_1","model":"m","status":"incomplete","output":[],` +
		`"incomplete_details":{"reason":"steered"}}`)
	resp, err := proto.MustOutbound("openai-responses").DecodeResponse(body)
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if resp.StopReason != ir.StopSteered {
		t.Fatalf("StopReason = %q, want steered（被塌成了别的档）", resp.StopReason)
	}
	if !resp.StopReason.Incomplete() {
		t.Errorf("steered 应判输出不完整，客户端不该把半截正文当最终答案")
	}
}

// steered 同族回写原值，不塌成 max_output_tokens。
func TestR110SteeredEncodesBackVerbatim(t *testing.T) {
	out, err := proto.MustInbound("openai-responses").EncodeResponse(&ir.Response{
		ID: "resp_1", Model: "m", StopReason: ir.StopSteered,
		Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}},
	})
	if err != nil {
		t.Fatalf("EncodeResponse: %v", err)
	}
	var got struct {
		Status            string `json:"status"`
		IncompleteDetails *struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	if got.Status != "incomplete" || got.IncompleteDetails == nil || got.IncompleteDetails.Reason != "steered" {
		t.Errorf("steered 回写丢档：status=%q details=%+v\n%s", got.Status, got.IncompleteDetails, out)
	}
}

// steered 跨族出站：三族都没有「用户转向截断」语义，但必须落到「输出不完整」
// 那一档（chat length / anthropic max_tokens / gemini MAX_TOKENS），塌成
// stop/STOP/end_turn 会让客户端把半截结果当成说完了。
func TestR110SteeredDegradesToIncompleteCrossFamily(t *testing.T) {
	if got := openaichat.UnmapFinishReason(ir.StopSteered); got != "length" {
		t.Errorf("chat finish_reason = %q, want length", got)
	}
	if got := anthropic.UnmapStopReason(ir.StopSteered); got != "max_tokens" {
		t.Errorf("anthropic stop_reason = %q, want max_tokens", got)
	}
	if got := gemini.UnmapFinishReason(ir.StopSteered); got != "MAX_TOKENS" {
		t.Errorf("gemini finishReason = %q, want MAX_TOKENS", got)
	}
}

// R110-C6 anthropic beta usage.iterations（按 message/compaction/advisor 迭代
// 阶段细分的用量）只有 anthropic 有槽位：跨族出站要被 UsageDropDims 报出来，
// 原生形态（anthropic）必须闭嘴——报了就是谎报。
func TestR110IterationsUsageDropDims(t *testing.T) {
	u := &ir.Usage{InputTokens: 1, OutputTokens: 1,
		Iterations: json.RawMessage(`{"message":{"input_tokens":1}}`)}
	for _, name := range []string{"openai-chat", "openai-responses", "gemini"} {
		dims := proto.UsageDropDims(u, name)
		if !strings.Contains(strings.Join(dims, ","), "usage iterations breakdown") {
			t.Errorf("%s 跨族出站没报 iterations 细分丢弃：%v", name, dims)
		}
	}
	// 原生形态闭嘴。
	if dims := proto.UsageDropDims(u, "anthropic"); len(dims) != 0 {
		t.Errorf("anthropic 是 iterations 的原生槽位，不该报丢弃：%v", dims)
	}
	// 没给 iterations 时谁都不报。
	quiet := &ir.Usage{InputTokens: 1, OutputTokens: 1}
	for _, name := range []string{"openai-chat", "openai-responses", "gemini", "anthropic"} {
		for _, d := range proto.UsageDropDims(quiet, name) {
			if strings.Contains(d, "iterations") {
				t.Errorf("%s 缺席误报 iterations：%v", name, d)
			}
		}
	}
}
