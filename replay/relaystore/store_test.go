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
	configured, ok, _, err := s.Authenticate(ctx, "compat", "anthropic", "bootstrap-secret", "")
	if err != nil || !configured || !ok {
		t.Fatalf("configured=%v ok=%v err=%v", configured, ok, err)
	}
	_, ok, _, _ = s.Authenticate(ctx, "compat", "anthropic", "wrong", "")
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
	configured, ok, _, err := s.Authenticate(ctx, "m", "openai-chat", "model-key", "")
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

// compress_model 列：PutUserModel 持久化、ListUserModels 回显、Authenticate
// 随行返回；CompressOf 非空跳过 key/协议校验放行（4.2），但禁用模型仍拒绝。
func TestCompressModelRoundTripAndCompressOfBypass(t *testing.T) {
	ctx := context.Background()
	s, err := Open(dialect.SQLite, filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.PutUserModel(ctx, UserModel{Name: "orig", Protocol: "anthropic", APIKey: "orig-key", Enabled: true, CompressModel: "comp"}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutUserModel(ctx, UserModel{Name: "comp", Protocol: "openai-chat", APIKey: "comp-key", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	models, err := s.ListUserModels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var orig, comp UserModel
	for _, m := range models {
		switch m.Name {
		case "orig":
			orig = m
		case "comp":
			comp = m
		}
	}
	if orig.CompressModel != "comp" || comp.CompressModel != "" {
		t.Fatalf("compress_model round trip: orig=%q comp=%q", orig.CompressModel, comp.CompressModel)
	}

	// Authenticate 返回 compress_model（1.3 触发条件数据源）
	if _, _, cm, err := s.Authenticate(ctx, "orig", "anthropic", "orig-key", ""); err != nil || cm != "comp" {
		t.Fatalf("authenticate compress_model=%q err=%v", cm, err)
	}

	// CompressOf 非空：错误 key + 协议不匹配仍放行（信任 Agent 已鉴权原请求）
	if configured, ok, _, err := s.Authenticate(ctx, "comp", "anthropic", "orig-key-not-comp", "orig"); err != nil || !configured || !ok {
		t.Fatalf("compress_of bypass: configured=%v ok=%v err=%v", configured, ok, err)
	}
	// CompressOf 置空时 key 校验照常（comp 的 key 不匹配 orig 的 key）
	if _, ok, _, err := s.Authenticate(ctx, "comp", "anthropic", "orig-key", ""); err != nil || ok {
		t.Fatalf("cross-model key must fail without compress_of: ok=%v err=%v", ok, err)
	}

	// 禁用模型：CompressOf 也拒绝（1.5：视为未配置）
	if err := s.PutUserModel(ctx, UserModel{Name: "comp", Protocol: "openai-chat", APIKey: "comp-key", Enabled: false}); err != nil {
		t.Fatal(err)
	}
	if configured, _, _, err := s.Authenticate(ctx, "comp", "openai-chat", "x", "orig"); err != nil || configured {
		t.Fatalf("disabled compress_model must be unconfigured: configured=%v err=%v", configured, err)
	}
}

// 存量库（无 compress_model 列）Open 自动补列，旧行默认空串（3.1 存量兼容）。
func TestOpenMigratesCompressModelColumn(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "relay.db")
	db, err := dialect.Open(dialect.SQLite, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE user_models(name TEXT PRIMARY KEY,protocol TEXT NOT NULL,api_key_hash TEXT NOT NULL,enabled INTEGER NOT NULL DEFAULT 1,updated_at BIGINT NOT NULL);
		INSERT INTO user_models(name,protocol,api_key_hash,enabled,updated_at) VALUES('legacy','auto','x',1,0)`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	s, err := Open(dialect.SQLite, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	models, err := s.ListUserModels(ctx)
	if err != nil || len(models) != 1 || models[0].Name != "legacy" {
		t.Fatalf("legacy rows: models=%+v err=%v", models, err)
	}
	if models[0].CompressModel != "" {
		t.Fatalf("legacy compress_model = %q, want empty", models[0].CompressModel)
	}
	if err := s.PutUserModel(ctx, UserModel{Name: "legacy", Protocol: "auto", APIKey: "k", Enabled: true, CompressModel: "other"}); err != nil {
		t.Fatal(err)
	}
	models, _ = s.ListUserModels(ctx)
	if models[0].CompressModel != "other" {
		t.Fatalf("updated compress_model = %q, want other", models[0].CompressModel)
	}
}
