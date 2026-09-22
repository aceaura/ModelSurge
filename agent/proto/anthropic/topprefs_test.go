package anthropic

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// R65：anthropic 顶层 cache_control 便捷糖与 inference_geo 双请求偏好贯通。
// 官方语义（SDK messages.ts）：顶层 cache_control 自动给最后一个可缓存块
// 打断点；inference_geo 指定推理地理区域，缺省按 workspace 默认。两维都是
// anthropic 一族独有，IR 原样保留，跨族由诊断报出（relay/diagnose.go）。

func TestTopPrefsDecode(t *testing.T) {
	r, err := codec{}.DecodeRequest([]byte(`{
		"model":"m","max_tokens":10,
		"messages":[{"role":"user","content":"hi"}],
		"cache_control":{"type":"ephemeral","ttl":"1h"},
		"inference_geo":"us"}`))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if r.TopCacheCtl != "ephemeral" || r.TopCacheTTL != "1h" {
		t.Errorf("TopCacheCtl/TTL = %q/%q", r.TopCacheCtl, r.TopCacheTTL)
	}
	if r.InferenceGeo != "us" {
		t.Errorf("InferenceGeo = %q", r.InferenceGeo)
	}
}

func TestTopPrefsDecodeAbsent(t *testing.T) {
	r, err := codec{}.DecodeRequest([]byte(
		`{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if r.TopCacheCtl != "" || r.TopCacheTTL != "" || r.InferenceGeo != "" {
		t.Errorf("缺席时应全空：%q/%q/%q", r.TopCacheCtl, r.TopCacheTTL, r.InferenceGeo)
	}
}

// ttl 缺省与显式 null：顶层糖只带 type 时 TTL 留空（官方默认 5m 由上游补）；
// 显式 null 与缺省同义，都解成空。
func TestTopPrefsDecodeTTLAbsentAndNull(t *testing.T) {
	for _, body := range []string{
		`{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hi"}],"cache_control":{"type":"ephemeral"}}`,
		`{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hi"}],"cache_control":null,"inference_geo":null}`,
	} {
		r, err := codec{}.DecodeRequest([]byte(body))
		if err != nil {
			t.Fatalf("DecodeRequest: %v", err)
		}
		if r.TopCacheTTL != "" {
			t.Errorf("TopCacheTTL = %q, want 空", r.TopCacheTTL)
		}
	}
}

func TestTopPrefsEncodeRoundTrip(t *testing.T) {
	req := &ir.Request{Model: "m", MaxTokens: 10,
		TopCacheCtl: "ephemeral", TopCacheTTL: "1h", InferenceGeo: "us",
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}}}
	out, err := codec{}.EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(out, &wire); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	cc, ok := wire["cache_control"].(map[string]any)
	if !ok {
		t.Fatalf("cache_control 缺席或类型错：%s", out)
	}
	if cc["type"] != "ephemeral" || cc["ttl"] != "1h" {
		t.Errorf("cache_control = %v", cc)
	}
	if wire["inference_geo"] != "us" {
		t.Errorf("inference_geo = %v", wire["inference_geo"])
	}
	// 同族往返：编出去再解回来三字段不变。
	back, err := codec{}.DecodeRequest(out)
	if err != nil {
		t.Fatalf("DecodeRequest(往返): %v", err)
	}
	if back.TopCacheCtl != "ephemeral" || back.TopCacheTTL != "1h" || back.InferenceGeo != "us" {
		t.Errorf("往返漂移：%q/%q/%q", back.TopCacheCtl, back.TopCacheTTL, back.InferenceGeo)
	}
}

func TestTopPrefsEncodeAbsent(t *testing.T) {
	req := &ir.Request{Model: "m", MaxTokens: 10,
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}}}
	out, err := codec{}.EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if strings.Contains(string(out), "cache_control") || strings.Contains(string(out), "inference_geo") {
		t.Errorf("零值不应出场：%s", out)
	}
}

// 顶层糖只有 type 没有 ttl 时，编码回写也应只有 type（omitempty 吃掉空 ttl）。
func TestTopPrefsEncodeTypeOnly(t *testing.T) {
	req := &ir.Request{Model: "m", MaxTokens: 10, TopCacheCtl: "ephemeral",
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}}}
	out, err := codec{}.EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, `"cache_control":{"type":"ephemeral"}`) {
		t.Errorf("cache_control 形态错：%s", s)
	}
	if strings.Contains(s, `"ttl"`) {
		t.Errorf("空 ttl 不应出场：%s", s)
	}
}
