package upstreamv1

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestResolvedTargetRedactionAndValidation(t *testing.T) {
	r := ResolvedTarget{ID: "a/model", Protocol: "openai-chat", NativeModel: "model", BaseURL: "https://example.test", APIKey: "super-secret", Headers: map[string]string{"Authorization": "Bearer token", "X-Trace": "ok"}}
	if err := ValidateResolvedTarget(r); err != nil {
		t.Fatal(err)
	}
	text := r.String() + string(r.RedactedJSON())
	for _, secret := range []string{"super-secret", "Bearer token"} {
		if strings.Contains(text, secret) {
			t.Fatalf("secret leaked: %s", text)
		}
	}
	if !strings.Contains(text, "X-Trace") || strings.Contains(text, `X-Trace:ok`) || strings.Contains(text, `"X-Trace":"ok"`) {
		t.Fatal("custom header name should remain but its value must be redacted")
	}
}
func TestValidateResolvedTargetRejectsUnknownProtocol(t *testing.T) {
	if ValidateResolvedTarget(ResolvedTarget{ID: "x", Protocol: "unknown", NativeModel: "m", BaseURL: "https://x"}) == nil {
		t.Fatal("expected validation error")
	}
}

func TestRuntimeMetadataProfileArnJSONRoundTrip(t *testing.T) {
	in := ResolvedTarget{
		ID: "kiro-2/gpt-5.6-sol", Protocol: "kiro", NativeModel: "gpt-5.6-sol", BaseURL: "https://example.test",
		Runtime: RuntimeMetadata{AccountType: "kiro", ProfileArn: "arn:aws:codewhisperer:us-east-1:1:profile/test"},
	}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out ResolvedTarget
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	if out.Runtime.ProfileArn != in.Runtime.ProfileArn {
		t.Fatalf("profile_arn=%q, want %q", out.Runtime.ProfileArn, in.Runtime.ProfileArn)
	}
}

func TestValidateProtocolRejectsGeminiAndKeepsKiro(t *testing.T) {
	if ValidateProtocol("gemini") == nil {
		t.Fatal("gemini must not be a valid upstream protocol")
	}
	if err := ValidateProtocol("kiro"); err != nil {
		t.Fatalf("kiro should remain valid: %v", err)
	}
}
