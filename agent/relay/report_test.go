package relay

import (
	"context"
	"errors"
	"github.com/aceaura/ModelSurge/upstream/dialect"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/aceaura/ModelSurge/agent/agentstore"
	"github.com/aceaura/ModelSurge/replay/contract/replayv1"
)

type outboxReplay struct {
	mu       sync.Mutex
	failures int
	calls    int
}

func (r *outboxReplay) Dispatch(context.Context, replayv1.DispatchRequest) (replayv1.TargetLease, error) {
	return replayv1.TargetLease{}, errors.New("unused")
}

func (r *outboxReplay) Report(context.Context, replayv1.ResultReport) (replayv1.ResultResponse, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	if r.failures > 0 {
		r.failures--
		return replayv1.ResultResponse{}, errors.New("temporary")
	}
	return replayv1.ResultResponse{}, nil
}

func (r *outboxReplay) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func TestRunOutboxWorkerReplaysDuringRuntimeAndStops(t *testing.T) {
	store, err := agentstore.Open(dialect.SQLite, filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	report := replayv1.ResultReport{ReportID: "report-1", RequestID: "request-1", GroupID: "group", TargetID: "target", Outcome: "abnormal", At: time.Now()}
	if err := store.Enqueue(context.Background(), report); err != nil {
		t.Fatal(err)
	}
	replay := &outboxReplay{failures: 1}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		RunOutboxWorker(ctx, store, replay, 10*time.Millisecond)
		close(done)
	}()

	deadline := time.Now().Add(4 * time.Second)
	for {
		var count int
		if err := store.DB.QueryRow(`SELECT COUNT(*) FROM report_outbox`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("outbox was not replayed; calls=%d", replay.callCount())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if replay.callCount() < 2 {
		t.Fatalf("calls=%d, want failed attempt and runtime replay", replay.callCount())
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("outbox worker did not stop after cancellation")
	}
}
