package replayv1

import (
	"encoding/json"
	"testing"
)

func TestRequestOverridesJSONRoundTripPreservesNilAndExplicitZero(t *testing.T) {
	zeroFloat := 0.0
	zeroInt := 0
	in := TargetLease{
		RequestID: "req", GroupID: "group", TargetID: "target",
		Protocol: "openai-chat", NativeModel: "native", BaseURL: "https://example.test",
		Runtime: RuntimeMetadata{ProfileArn: "arn:aws:codewhisperer:us-east-1:1:profile/test"},
		RequestOverrides: &RequestOverrides{
			Temperature: &zeroFloat,
			TopP:        nil,
			MaxTokens:   &zeroInt,
			Thinking:    &ThinkingOverride{Enabled: false},
		},
	}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out TargetLease
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	if out.Runtime.ProfileArn != in.Runtime.ProfileArn {
		t.Fatalf("profile_arn = %q, want %q", out.Runtime.ProfileArn, in.Runtime.ProfileArn)
	}
	if out.RequestOverrides == nil {
		t.Fatal("request_overrides lost")
	}
	if out.RequestOverrides.Temperature == nil || *out.RequestOverrides.Temperature != 0 {
		t.Fatalf("temperature = %v, want explicit zero", out.RequestOverrides.Temperature)
	}
	if out.RequestOverrides.TopP != nil {
		t.Fatalf("top_p = %v, want nil", out.RequestOverrides.TopP)
	}
	if out.RequestOverrides.MaxTokens == nil || *out.RequestOverrides.MaxTokens != 0 {
		t.Fatalf("max_tokens = %v, want explicit zero", out.RequestOverrides.MaxTokens)
	}
	if out.RequestOverrides.Thinking == nil || out.RequestOverrides.Thinking.Enabled {
		t.Fatalf("thinking = %+v, want explicit disabled", out.RequestOverrides.Thinking)
	}
}

func TestRequestOverridesJSONRoundTripPreservesNilContainer(t *testing.T) {
	in := TargetLease{RequestID: "req", GroupID: "group", TargetID: "target", Protocol: "openai-chat", NativeModel: "native", BaseURL: "https://example.test"}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out TargetLease
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	if out.RequestOverrides != nil {
		t.Fatalf("request_overrides = %+v, want nil", out.RequestOverrides)
	}
}
