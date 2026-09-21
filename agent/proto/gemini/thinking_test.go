package gemini

import "testing"

// thinkingBudget 的三种官方语义：0 关闭、-1 动态、正数固定预算。
// 此前一律 Enabled=true，会把「显式关闭思考」翻成「开启」送往上游。
func TestDecodeRequest_ThinkingBudgetSemantics(t *testing.T) {
	cases := []struct {
		name        string
		body        string
		wantPresent bool
		wantEnabled bool
		wantBudget  int
	}{
		{"off", `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"thinkingConfig":{"thinkingBudget":0}}}`,
			true, false, 0},
		{"dynamic", `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"thinkingConfig":{"thinkingBudget":-1}}}`,
			true, true, -1},
		{"fixed", `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"thinkingConfig":{"thinkingBudget":8192}}}`,
			true, true, 8192},
		{"absent", `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"maxOutputTokens":64}}`,
			false, false, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req, err := codec{}.DecodeRequest([]byte(c.body))
			if err != nil {
				t.Fatalf("DecodeRequest: %v", err)
			}
			if !c.wantPresent {
				if req.Thinking != nil {
					t.Fatalf("无 thinkingConfig 时不应构造 Thinking：%+v", req.Thinking)
				}
				return
			}
			if req.Thinking == nil {
				t.Fatal("缺 Thinking")
			}
			if req.Thinking.Enabled != c.wantEnabled {
				t.Errorf("Enabled = %v, want %v", req.Thinking.Enabled, c.wantEnabled)
			}
			if req.Thinking.BudgetTokens != c.wantBudget {
				t.Errorf("BudgetTokens = %d, want %d", req.Thinking.BudgetTokens, c.wantBudget)
			}
		})
	}
}
