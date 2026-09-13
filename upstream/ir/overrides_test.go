package ir

import (
	"encoding/json"
	"testing"
)

// 覆盖语义：nil 指针透传、已配置字段强制覆盖。
func TestOverrides_Apply(t *testing.T) {
	temp := 1.0
	topP := 0.95
	maxTok := 4096
	clientTemp := 0.3
	req := &Request{
		MaxTokens:   8192,
		Temperature: &clientTemp,
		Thinking:    &ThinkingConfig{Enabled: false},
	}

	var nilOv *Overrides
	nilOv.Apply(req)
	if req.MaxTokens != 8192 || *req.Temperature != 0.3 || req.Thinking.Enabled {
		t.Fatalf("nil overrides must be no-op: %+v", req)
	}

	ov := &Overrides{
		Thinking:    &ThinkingOverride{Enabled: true, BudgetTokens: 4096, Effort: "max"},
		Temperature: &temp,
		TopP:        &topP,
		MaxTokens:   &maxTok,
	}
	ov.Apply(req)
	if req.Thinking == nil || !req.Thinking.Enabled || req.Thinking.BudgetTokens != 4096 || req.Thinking.Effort != "max" {
		t.Errorf("thinking = %+v, want enabled/4096/max", req.Thinking)
	}
	if *req.Temperature != 1.0 || *req.TopP != 0.95 {
		t.Errorf("temp/top_p = %v/%v, want 1/0.95", *req.Temperature, *req.TopP)
	}
	if req.MaxTokens != 4096 {
		t.Errorf("max_tokens = %d, want 4096", req.MaxTokens)
	}
}

// force-off：Enabled=false 覆盖掉客户端已有的 thinking 配置。
func TestOverrides_ApplyForceOff(t *testing.T) {
	req := &Request{Thinking: &ThinkingConfig{Enabled: true, BudgetTokens: 2048}}
	(&Overrides{Thinking: &ThinkingOverride{Enabled: false}}).Apply(req)
	if req.Thinking == nil || req.Thinking.Enabled {
		t.Errorf("thinking = %+v, want present but disabled", req.Thinking)
	}
}

// Configured：全空配置返回 false。
func TestOverrides_Configured(t *testing.T) {
	var nilOv *Overrides
	if nilOv.Configured() {
		t.Error("nil overrides should not be configured")
	}
	if (&Overrides{}).Configured() {
		t.Error("empty overrides should not be configured")
	}
	if !(&Overrides{Temperature: new(float64)}).Configured() {
		t.Error("temperature-only overrides should be configured")
	}
}

// json 往返（账号存储与管理面 API 走 json 形态）。
func TestOverrides_JSONRoundtrip(t *testing.T) {
	in := `{"thinking":{"enabled":true,"budget_tokens":4096,"effort":"max"},"temperature":1,"top_p":0.95}`
	var ov Overrides
	if err := json.Unmarshal([]byte(in), &ov); err != nil {
		t.Fatal(err)
	}
	if ov.Thinking == nil || !ov.Thinking.Enabled || ov.Thinking.BudgetTokens != 4096 || ov.Thinking.Effort != "max" {
		t.Fatalf("thinking = %+v", ov.Thinking)
	}
	if ov.Temperature == nil || *ov.Temperature != 1 || ov.TopP == nil || *ov.TopP != 0.95 {
		t.Fatalf("scalar = %v %v", ov.Temperature, ov.TopP)
	}
	out, err := json.Marshal(&ov)
	if err != nil {
		t.Fatal(err)
	}
	var back Overrides
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatal(err)
	}
	if back.Thinking == nil || !back.Thinking.Enabled || back.Thinking.BudgetTokens != 4096 {
		t.Errorf("roundtrip lost thinking: %s", out)
	}
}
