package relaystore

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/aceaura/ModelSurge/upstream/dialect"
	"github.com/aceaura/ModelSurge/upstream/dialect/pgtest"
)

// pgUnique 每次调用唯一短后缀：gated 测试对同一 PG 库重复执行不撞行。
var pgUnique = time.Now().UnixNano()

// postgres 门控：SQLite→PG 迁移 + 迁后鉴权/调度/报表全链路。
func TestMigrateSQLiteToPG(t *testing.T) {
	ctx := context.Background()
	sfx := fmt.Sprintf("-%d", pgUnique)
	source := createReplaySource(t, sfx)
	s, err := Open(dialect.Postgres, pgtest.ModuleDSN(t, "replay"))
	if err != nil {
		t.Fatalf("open pg: %v", err)
	}
	defer s.Close()

	res, err := s.Migrate(ctx, source)
	if err != nil {
		t.Fatalf("migrate to pg: %v", err)
	}
	if res.UserModels != 2 || res.Groups != 2 || res.Members != 2 || res.Reports != 1 {
		t.Fatalf("migrate result = %+v", res)
	}

	model := "m" + sfx
	configured, ok, _, err := s.Authenticate(ctx, model, "anthropic", "model-key", "")
	if err != nil || !configured || !ok {
		t.Fatalf("auth after pg migrate: configured=%v ok=%v err=%v", configured, ok, err)
	}
	g, err := s.GroupForModel(ctx, model)
	if err != nil || g == nil {
		t.Fatalf("group for %s: %+v %v", model, g, err)
	}
	if len(g.Members) != 2 {
		t.Fatalf("group members = %+v", g.Members)
	}

	// 迁后 ApplyReport 幂等链路（rebind 后的 SQL 全走一遍）；
	// 新 report_id（rep1+sfx 已随迁移入库，撞唯一约束）
	newReport := "post-migrate" + sfx
	applied, err := s.ApplyReport(ctx, newReport, "req2"+sfx, g.ID, "u1", "normal")
	if err != nil || !applied {
		t.Fatalf("apply report: applied=%v err=%v", applied, err)
	}
	applied, err = s.ApplyReport(ctx, newReport, "req2"+sfx, g.ID, "u1", "normal")
	if err != nil || applied {
		t.Fatalf("replay apply not idempotent: applied=%v err=%v", applied, err)
	}

	// 重跑迁移：行数不变
	if _, err := s.Migrate(ctx, source); err != nil {
		t.Fatalf("re-migrate: %v", err)
	}
	var members int
	if err := s.DB.QueryRow(s.q(`SELECT COUNT(*) FROM schedule_group_members WHERE group_id=?`), g.ID).Scan(&members); err != nil {
		t.Fatal(err)
	}
	if members != 2 {
		t.Fatalf("members after re-migrate = %d", members)
	}
}
