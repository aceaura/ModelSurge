package gemini

import (
	"strings"
	"testing"
)

// B5：generationConfig.serviceTier 与 responseJsonSchema 解码即丢的修复。
// responseJsonSchema 是 responseSchema 的替代槽位（完整 JSON Schema），
// serviceTier 原值进 IR 由出站按目标值集映射。
func TestServiceTierAndResponseJsonSchemaDecode(t *testing.T) {
	body := `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],` +
		`"generationConfig":{"serviceTier":"flex",` +
		`"responseJsonSchema":{"type":"object","properties":{"n":{"type":"string"}},"additionalProperties":false}}}`
	req, err := New().DecodeRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if req.ServiceTier != "flex" {
		t.Fatalf("serviceTier = %q", req.ServiceTier)
	}
	if req.ResponseFormat == nil || !strings.Contains(string(req.ResponseFormat.Schema), `"additionalProperties":false`) {
		t.Fatalf("responseJsonSchema not carried: %+v", req.ResponseFormat)
	}
	if !req.ResponseFormat.Strict {
		t.Errorf("gemini schema 恒严格语义: %+v", req.ResponseFormat)
	}

	// "unspecified" 是显式缺省，与不给同义，不得进 IR 让出站发明档位。
	body2 := `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],` +
		`"generationConfig":{"serviceTier":"unspecified"}}`
	req2, err := New().DecodeRequest([]byte(body2))
	if err != nil {
		t.Fatal(err)
	}
	if req2.ServiceTier != "" {
		t.Errorf("unspecified must be treated as absent: %q", req2.ServiceTier)
	}
	if req2.ResponseFormat != nil {
		t.Errorf("no schema given must not invent ResponseFormat: %+v", req2.ResponseFormat)
	}

	// 两槽同给时取 responseSchema（老槽位优先，互斥属客户端错误）。
	body3 := `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],` +
		`"generationConfig":{"responseSchema":{"type":"string"},"responseJsonSchema":{"type":"integer"}}}`
	req3, err := New().DecodeRequest([]byte(body3))
	if err != nil {
		t.Fatal(err)
	}
	if req3.ResponseFormat == nil || string(req3.ResponseFormat.Schema) != `{"type":"string"}` {
		t.Errorf("responseSchema must win over responseJsonSchema: %+v", req3.ResponseFormat)
	}
}
