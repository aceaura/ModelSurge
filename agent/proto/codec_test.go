package proto_test

import (
	"testing"

	"github.com/aceaura/ModelSurge/agent/proto"
	_ "github.com/aceaura/ModelSurge/agent/proto/anthropic"
	_ "github.com/aceaura/ModelSurge/agent/proto/gemini"
	_ "github.com/aceaura/ModelSurge/agent/proto/openaichat"
	_ "github.com/aceaura/ModelSurge/agent/proto/openairesponses"
)

func TestDirectionalRegistry(t *testing.T) {
	if _, err := proto.GetInbound("gemini"); err != nil {
		t.Fatalf("GetInbound(gemini): %v", err)
	}
	if _, err := proto.GetOutbound("gemini"); err == nil {
		t.Fatal("GetOutbound(gemini) succeeded, want inbound-only rejection")
	}

	for _, name := range []string{"anthropic", "openai-chat", "openai-responses", "codex"} {
		if _, err := proto.GetInbound(name); err != nil {
			t.Errorf("GetInbound(%s): %v", name, err)
		}
		if _, err := proto.GetOutbound(name); err != nil {
			t.Errorf("GetOutbound(%s): %v", name, err)
		}
	}
}

func TestNamesAreDirectional(t *testing.T) {
	if !contains(proto.Names(), "gemini") {
		t.Fatalf("Names() = %v, want Gemini inbound", proto.Names())
	}
	if contains(proto.OutboundNames(), "gemini") {
		t.Fatalf("OutboundNames() = %v, must not contain Gemini", proto.OutboundNames())
	}
}

func contains(names []string, want string) bool {
	for _, name := range names {
		if name == want {
			return true
		}
	}
	return false
}
