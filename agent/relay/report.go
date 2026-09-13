package relay

import (
	"context"
	"log"
	"time"

	"github.com/aceaura/ModelSurge/agent/agentstore"
	"github.com/aceaura/ModelSurge/replay/contract/replayv1"
)

func (f *Forwarder) finishReport(protocol, model string, report replayv1.ResultReport) replayv1.ResultResponse {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := report.Outcome
	if f.store != nil {
		if err := f.store.LogRequest(ctx, agentstore.RequestLog{RequestID: report.RequestID, At: report.At, InboundProtocol: protocol, UserModel: model, TargetID: report.TargetID, Result: result, Usage: report.Usage}); err != nil {
			log.Printf("agent: request log %s failed: %v", report.RequestID, err)
		}
	}
	for attempt := 0; attempt < 3; attempt++ {
		response, err := f.replay.Report(ctx, report)
		if err == nil {
			return response
		}
		if ctx.Err() != nil {
			break
		}
	}
	if f.store != nil {
		if err := f.store.Enqueue(ctx, report); err != nil {
			log.Printf("agent: enqueue report %s failed: %v", report.ReportID, err)
		}
	}
	return replayv1.ResultResponse{}
}

func ReplayOutbox(ctx context.Context, store *agentstore.Store, replay Replay) {
	items, err := store.Due(ctx, 100)
	if err != nil {
		log.Printf("agent: load report outbox: %v", err)
		return
	}
	for _, item := range items {
		rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_, err := replay.Report(rctx, item.Report)
		cancel()
		if err == nil {
			_ = store.Ack(ctx, item.ID)
		} else {
			_ = store.Retry(ctx, item.ID, item.Attempts+1)
		}
	}
}

func RunOutboxWorker(ctx context.Context, store *agentstore.Store, replay Replay, interval time.Duration) {
	if interval <= 0 {
		interval = time.Second
	}
	ReplayOutbox(ctx, store, replay)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			ReplayOutbox(ctx, store, replay)
		}
	}
}
