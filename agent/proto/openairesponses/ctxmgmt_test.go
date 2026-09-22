package openairesponses

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// R67：responses context_management 请求侧贯通。官方 SDK 核对
// （openai-sdk-typescript responses.ts:11017/11329）：请求侧
// Array<{type, compact_threshold?}>，目前唯一 type 是 "compaction"；
// 响应对象不回显（Response 接口无此字段，8970 是 WS 的 ResponseCreate
// 客户端事件）。纯请求侧维度，同族（含 codex 同形别名）回写。

func TestContextMgmtDecode(t *testing.T) {
	r, err := codec{}.DecodeRequest([]byte(`{"model":"m","input":"hi",
		"context_management":[{"type":"compaction","compact_threshold":200000},{"type":"compaction"}]}`))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if len(r.ContextMgmt) != 2 {
		t.Fatalf("ContextMgmt = %+v", r.ContextMgmt)
	}
	if r.ContextMgmt[0].Type != "compaction" || r.ContextMgmt[0].CompactThreshold == nil ||
		*r.ContextMgmt[0].CompactThreshold != 200000 {
		t.Errorf("entry[0] = %+v", r.ContextMgmt[0])
	}
	// threshold 缺省（上游默认）与显式值要分开：nil ≠ 0。
	if r.ContextMgmt[1].CompactThreshold != nil {
		t.Errorf("entry[1] threshold 应为 nil：%+v", r.ContextMgmt[1])
	}
}

func TestContextMgmtDecodeAbsent(t *testing.T) {
	r, err := codec{}.DecodeRequest([]byte(`{"model":"m","input":"hi"}`))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if len(r.ContextMgmt) != 0 {
		t.Errorf("缺席应为空：%+v", r.ContextMgmt)
	}
}

func TestContextMgmtEncodeRoundTrip(t *testing.T) {
	th := 200000
	req := &ir.Request{Model: "m",
		ContextMgmt: []ir.ContextMgmtEntry{
			{Type: "compaction", CompactThreshold: &th},
			{Type: "compaction"},
		},
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}}}
	out, err := codec{}.EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(out, &wire); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	cm, ok := wire["context_management"].([]any)
	if !ok || len(cm) != 2 {
		t.Fatalf("context_management = %v", wire["context_management"])
	}
	e0 := cm[0].(map[string]any)
	if e0["type"] != "compaction" || e0["compact_threshold"] != float64(200000) {
		t.Errorf("entry[0] = %v", e0)
	}
	e1 := cm[1].(map[string]any)
	if _, has := e1["compact_threshold"]; has {
		t.Errorf("nil threshold 不应出键：%v", e1)
	}
	// 同族往返不漂移。
	back, err := codec{}.DecodeRequest(out)
	if err != nil {
		t.Fatalf("DecodeRequest(往返): %v", err)
	}
	if len(back.ContextMgmt) != 2 || back.ContextMgmt[0].CompactThreshold == nil ||
		*back.ContextMgmt[0].CompactThreshold != 200000 || back.ContextMgmt[1].CompactThreshold != nil {
		t.Errorf("往返漂移：%+v", back.ContextMgmt)
	}
	// 空值不出键。
	out2, _ := codec{}.EncodeRequest(&ir.Request{Model: "m",
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}}})
	if strings.Contains(string(out2), "context_management") {
		t.Errorf("空值不应出键：%s", out2)
	}
}
