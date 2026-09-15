package relaystore

import (
	"context"
	"github.com/aceaura/ModelSurge/upstream/dialect"
	"path/filepath"
	"testing"
)

func TestUserModelAuthenticationStoresOnlyHash(t *testing.T) {
	ctx := context.Background()
	s, err := Open(dialect.SQLite, filepath.Join(t.TempDir(), "relay.db"))
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
	s, err := Open(dialect.SQLite, filepath.Join(t.TempDir(), "relay.db"))
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
	s, err := Open(dialect.SQLite, filepath.Join(t.TempDir(), "replay.db"))
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

// 超限不污染 target_cache：原 target 与 last_result 保持不变
// （对照 abnormal 仍写 "abnormal"）。
func TestApplyReportContextExceededKeepsTargetCache(t *testing.T) {
	ctx := context.Background()
	s, err := Open(dialect.SQLite, filepath.Join(t.TempDir(), "replay.db"))
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
	if _, err := s.ApplyReport(ctx, "warm", "request", "g", "a/model", "normal"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyReport(ctx, "ctx", "request", "g", "b/model", "context_exceeded"); err != nil {
		t.Fatal(err)
	}
	group, err := s.GroupForModel(ctx, "m")
	if err != nil {
		t.Fatal(err)
	}
	if group.CachedTarget != "a/model" || group.LastResult != "normal" {
		t.Fatalf("target_cache polluted: target=%q last_result=%q, want a/model normal", group.CachedTarget, group.LastResult)
	}
	if _, err := s.ApplyReport(ctx, "abn", "request", "g", "c/model", "abnormal"); err != nil {
		t.Fatal(err)
	}
	group, err = s.GroupForModel(ctx, "m")
	if err != nil {
		t.Fatal(err)
	}
	if group.CachedTarget != "c/model" || group.LastResult != "abnormal" {
		t.Fatalf("abnormal should still write cache: target=%q last_result=%q", group.CachedTarget, group.LastResult)
	}
}
