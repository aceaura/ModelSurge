package replayv1

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRequestOverridesJSONRoundTripPreservesNilAndExplicitZero(t *testing.T) {
	zeroFloat := 0.0
	zeroInt := 0
	in := TargetLease{
		RequestID: "req", GroupID: "group", TargetID: "target",
		Protocol: "openai-chat", NativeModel: "native", BaseURL: "https://example.test",
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

func TestKiroLeaseJSONIsOpaque(t *testing.T) {
	data, err := json.Marshal(TargetLease{
		RequestID: "req", GroupID: "group", TargetID: "target", Protocol: "kiro", NativeModel: "native",
		BaseURL: "https://secret.example", Credential: "secret", Headers: map[string]string{"Authorization": "Bearer secret"},
		RequestOverrides: &RequestOverrides{}, Runtime: RuntimeMetadata{AccountType: "kiro"},
	})
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, forbidden := range []string{"profile_arn", "native_model", "base_url", "credential", "headers", "request_overrides", "runtime"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("opaque Kiro lease contains %q: %s", forbidden, text)
		}
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
