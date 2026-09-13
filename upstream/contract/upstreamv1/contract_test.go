package upstreamv1

import (
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
