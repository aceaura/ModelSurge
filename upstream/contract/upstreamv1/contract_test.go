package upstreamv1

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/upstream/ir"
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

func TestKiroResolvedTargetIsOpaque(t *testing.T) {
	in := ResolvedTarget{
		ID: "kiro-2/gpt-5.6-sol", Protocol: "kiro", NativeModel: "gpt-5.6-sol",
		BaseURL: "https://secret.example", APIKey: "secret", Headers: map[string]string{"Authorization": "Bearer secret"},
		RequestOverrides: &ir.Overrides{}, Runtime: RuntimeMetadata{AccountType: "kiro"},
	}
	if err := ValidateResolvedTarget(ResolvedTarget{ID: in.ID, Protocol: in.Protocol}); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, forbidden := range []string{"profile_arn", "native_model", "base_url", "api_key", "headers", "request_overrides", "runtime"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("opaque Kiro target contains %q: %s", forbidden, text)
		}
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
