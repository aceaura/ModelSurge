package relay

import (
	"context"
	"time"
)

type requestLogContextKey struct{}

type requestLogContext struct {
	id      string
	started time.Time
}

func withRequestLog(ctx context.Context, id string, started time.Time) context.Context {
	return context.WithValue(ctx, requestLogContextKey{}, requestLogContext{id: id, started: started})
}

func requestLogFrom(ctx context.Context) requestLogContext {
	value, _ := ctx.Value(requestLogContextKey{}).(requestLogContext)
	return value
}

func requestIDFrom(ctx context.Context) string { return requestLogFrom(ctx).id }

func requestLatencyFrom(ctx context.Context) time.Duration {
	started := requestLogFrom(ctx).started
	if started.IsZero() {
		return 0
	}
	return time.Since(started)
}
