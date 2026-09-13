package relaystore

import (
	"context"
	"path/filepath"
	"testing"
)

func TestUserModelAuthenticationStoresOnlyHash(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Bootstrap(ctx, "bootstrap-secret", []string{"compat"}); err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := s.DB.QueryRow(`SELECT api_key_hash FROM user_models WHERE name='compat'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored == "bootstrap-secret" || stored != HashAPIKey("bootstrap-secret") {
		t.Fatalf("unsafe stored credential %q", stored)
	}
	configured, ok, err := s.Authenticate(ctx, "compat", "anthropic", "bootstrap-secret")
	if err != nil || !configured || !ok {
		t.Fatalf("configured=%v ok=%v err=%v", configured, ok, err)
	}
	_, ok, _ = s.Authenticate(ctx, "compat", "anthropic", "wrong")
	if ok {
		t.Fatal("wrong key authenticated")
	}
}

func TestUserModelProtocolBinding(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.PutUserModel(ctx, UserModel{Name: "m", Protocol: "anthropic", APIKey: "model-key", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	configured, ok, err := s.Authenticate(ctx, "m", "openai-chat", "model-key")
	if err != nil || !configured || ok {
		t.Fatalf("protocol mismatch configured=%v ok=%v err=%v", configured, ok, err)
	}
}

func TestApplyReportIsIdempotent(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "replay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.PutUserModel(ctx, UserModel{Name: "m", Protocol: "auto", APIKey: "key", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutGroup(ctx, Group{ID: "g", UserModel: "m", PolicyType: "sticky", PolicyConfig: "{}"}); err != nil {
		t.Fatal(err)
	}
	applied, err := s.ApplyReport(ctx, "report", "request", "g", "a/model", "normal")
	if err != nil || !applied {
		t.Fatalf("first applied=%v err=%v", applied, err)
	}
	applied, err = s.ApplyReport(ctx, "report", "request", "g", "b/model", "abnormal")
	if err != nil || applied {
		t.Fatalf("duplicate applied=%v err=%v", applied, err)
	}
	group, err := s.GroupForModel(ctx, "m")
	if err != nil || group.CachedTarget != "a/model" || group.LastResult != "normal" {
		t.Fatalf("group=%+v err=%v", group, err)
	}
}
