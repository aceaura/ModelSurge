package relaystore

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/aceaura/ModelSurge/upstream/dialect"
)

// createReplaySource 构造一个模式一形态的 SQLite replay.db。
// suffix 非空时拼进用户模型名（PG gated 用例对同一库重复执行不撞行）。
func createReplaySource(t *testing.T, suffix ...string) string {
	t.Helper()
	ctx := context.Background()
	sfx := ""
	if len(suffix) > 0 {
		sfx = suffix[0]
	}
	model := "m" + sfx
	path := filepath.Join(t.TempDir(), "source-replay.db")
	s, err := Open(dialect.SQLite, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Bootstrap(ctx, "bootstrap-secret", []string{"compat"}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutUserModel(ctx, UserModel{Name: model, Protocol: "anthropic", APIKey: "model-key", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutGroup(ctx, Group{ID: "g1" + sfx, UserModel: model, PolicyType: "round_robin", PolicyConfig: "{}", Members: []string{"u1", "u2"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddMembers(ctx, "g1"+sfx, []string{"u1", "u2"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyReport(ctx, "rep1"+sfx, "req1"+sfx, "g1"+sfx, "u1", "normal"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetCache(ctx, "g1"+sfx, "u1", "ok"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestMigrateFreshIdempotent(t *testing.T) {
	ctx := context.Background()
	source := createReplaySource(t)
	dst, err := Open(dialect.SQLite, filepath.Join(t.TempDir(), "target-replay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()

	res, err := dst.Migrate(ctx, source)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if res.UserModels != 2 || res.Groups != 2 || res.Members != 2 || res.Reports != 1 {
		t.Fatalf("migrate result = %+v", res)
	}

	// 迁移后鉴权/调度数据可用（bootstrap key 与 model-key 均能过）
	configured, ok, _, err := dst.Authenticate(ctx, "compat", "auto", "bootstrap-secret", "")
	if err != nil || !configured || !ok {
		t.Fatalf("compat auth: configured=%v ok=%v err=%v", configured, ok, err)
	}
	g, err := dst.GroupForModel(ctx, "m")
	if err != nil || g == nil {
		t.Fatalf("group for m: %+v %v", g, err)
	}
	if g.ID != "g1" || len(g.Members) != 2 {
		t.Fatalf("group = %+v", g)
	}

	// 重跑幂等：无新行、成员不重复（DO UPDATE 会重写行，行数不变）
	res2, err := dst.Migrate(ctx, source)
	if err != nil {
		t.Fatalf("re-migrate: %v", err)
	}
	if res2.Members != 0 || res2.Reports != 0 {
		t.Fatalf("re-migrate duplicated: %+v", res2)
	}
	var umCount, groupCount, memberCount, reportCount int
	if err := dst.DB.QueryRow(`SELECT COUNT(*) FROM user_models`).Scan(&umCount); err != nil {
		t.Fatal(err)
	}
	if err := dst.DB.QueryRow(`SELECT COUNT(*) FROM schedule_groups`).Scan(&groupCount); err != nil {
		t.Fatal(err)
	}
	if err := dst.DB.QueryRow(`SELECT COUNT(*) FROM schedule_group_members`).Scan(&memberCount); err != nil {
		t.Fatal(err)
	}
	if err := dst.DB.QueryRow(`SELECT COUNT(*) FROM result_reports`).Scan(&reportCount); err != nil {
		t.Fatal(err)
	}
	if umCount != 2 || groupCount != 2 || memberCount != 2 || reportCount != 1 {
		t.Fatalf("row counts after re-run: um=%d groups=%d members=%d reports=%d", umCount, groupCount, memberCount, reportCount)
	}
	g2, err := dst.GroupForModel(ctx, "m")
	if err != nil || g2 == nil || len(g2.Members) != 2 {
		t.Fatalf("group after re-run = %+v %v", g2, err)
	}
}

func TestMigrateRejectsSameFile(t *testing.T) {
	source := createReplaySource(t)
	s, err := Open(dialect.SQLite, source)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.Migrate(context.Background(), source); err == nil {
		t.Fatal("migrating into the same file should fail")
	}
}
