package agentstore

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/aceaura/ModelSurge/replay/contract/replayv1"
	"github.com/aceaura/ModelSurge/upstream/dialect"
	"github.com/aceaura/ModelSurge/upstream/dialect/pgtest"
)

// pgUnique 每次调用唯一短后缀：gated 测试对同一 PG 库重复执行不撞行。
var pgUnique = time.Now().UnixNano()

// postgres 门控：请求日志 + outbox 全链路（IDENTITY 主键、rebind 后 SQL）。
func TestAgentStorePostgres(t *testing.T) {
	ctx := context.Background()
	s, err := Open(dialect.Postgres, pgtest.ModuleDSN(t, "agent"))
	if err != nil {
		t.Fatalf("open pg: %v", err)
	}
	defer s.Close()

	sfx := fmt.Sprintf("-%d", pgUnique)
	reqID := "req" + sfx
	if err := s.LogRequest(ctx, RequestLog{
		RequestID: reqID, At: time.Now(), InboundProtocol: "anthropic",
		UserModel: "m" + sfx, TargetID: "acct/m", Result: "normal",
		Usage: replayv1.Usage{InputTokens: 3, OutputTokens: 5},
	}); err != nil {
		t.Fatal(err)
	}
	// upsert：同 request_id 重写不撞约束
	if err := s.LogRequest(ctx, RequestLog{
		RequestID: reqID, At: time.Now(), InboundProtocol: "anthropic",
		UserModel: "m" + sfx, TargetID: "acct/m", Result: "error",
		Usage: replayv1.Usage{InputTokens: 4, OutputTokens: 6},
	}); err != nil {
		t.Fatal(err)
	}

	report := replayv1.ResultReport{
		ReportID: "rep" + sfx, RequestID: reqID, Outcome: "normal",
		Usage: replayv1.Usage{InputTokens: 3, OutputTokens: 5},
	}
	if err := s.Enqueue(ctx, report); err != nil {
		t.Fatal(err)
	}
	// 幂等：同 report_id 重复入队不新增行
	if err := s.Enqueue(ctx, report); err != nil {
		t.Fatal(err)
	}

	due, err := s.Due(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	var item *OutboxItem
	for i := range due {
		if due[i].Report.ReportID == report.ReportID {
			item = &due[i]
		}
	}
	if item == nil {
		t.Fatalf("due outbox missing %s: %+v", report.ReportID, due)
	}
	if item.Attempts != 0 {
		t.Fatalf("attempts = %d, want 0", item.Attempts)
	}
	id := item.ID

	// Retry 推迟后再取出不到，Ack 清行
	if err := s.Retry(ctx, id, 1); err != nil {
		t.Fatal(err)
	}
	due, err = s.Due(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	for i := range due {
		if due[i].ID == id {
			t.Fatalf("retry still due immediately: %+v", due[i])
		}
	}
	if err := s.Ack(ctx, id); err != nil {
		t.Fatal(err)
	}
	due, err = s.Due(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	for i := range due {
		if due[i].ID == id {
			t.Fatal("ack did not remove outbox item")
		}
	}
}
