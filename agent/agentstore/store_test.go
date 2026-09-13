package agentstore

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/aceaura/ModelSurge/replay/contract/replayv1"
)

func TestDeleteJournalAndOutboxReplayData(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var mode string
	if err := s.DB.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "delete" {
		t.Fatalf("journal_mode=%q", mode)
	}
	r := replayv1.ResultReport{ReportID: "rep-1", RequestID: "req-1", GroupID: "g", TargetID: "t", Outcome: "abnormal", At: time.Now()}
	if err := s.Enqueue(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	items, err := s.Due(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Report.TargetID != "t" {
		t.Fatalf("items=%+v", items)
	}
	if err := s.Ack(context.Background(), items[0].ID); err != nil {
		t.Fatal(err)
	}
	items, err = s.Due(context.Background(), 10)
	if err != nil || len(items) != 0 {
		t.Fatalf("after ack=%+v err=%v", items, err)
	}
}

func TestRequestLogContainsNoSecretColumns(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rows, err := s.DB.Query(`PRAGMA table_info(request_log)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull, pk int
		var def any
		if err := rows.Scan(&cid, &name, &typ, &notnull, &def, &pk); err != nil {
			t.Fatal(err)
		}
		switch name {
		case "client_key", "credential", "headers", "token":
			t.Fatalf("secret column %q", name)
		}
	}
}
