package config

import "testing"

func TestParseKiroAppliesFakeReasoningDefaults(t *testing.T) {
	kiro := &Kiro{}
	if err := ParseKiro(kiro); err != nil {
		t.Fatal(err)
	}
	if kiro.FakeReasoningMaxTokens != 4000 {
		t.Fatalf("fake_reasoning_max_tokens = %d, want 4000", kiro.FakeReasoningMaxTokens)
	}
	if kiro.FakeReasoningBudgetCap != 10000 {
		t.Fatalf("fake_reasoning_budget_cap = %d, want 10000", kiro.FakeReasoningBudgetCap)
	}
}

func TestParseKiroRejectsNegativeFakeReasoningLimits(t *testing.T) {
	for name, kiro := range map[string]*Kiro{
		"max tokens": {FakeReasoningMaxTokens: -1},
		"budget cap": {FakeReasoningBudgetCap: -1},
	} {
		t.Run(name, func(t *testing.T) {
			if err := ParseKiro(kiro); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}
