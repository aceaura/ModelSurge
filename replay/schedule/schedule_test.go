package schedule

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/aceaura/ModelSurge/replay/relaystore"
	"github.com/aceaura/ModelSurge/upstream/contract/upstreamv1"
)

type fakeUpstream struct {
	evals, resolves int
	candidates      []upstreamv1.CandidateEvaluation
	reportErr       error
}

func (f *fakeUpstream) Models(context.Context) ([]upstreamv1.ModelSummary, error) {
	return []upstreamv1.ModelSummary{{ID: "a/model"}, {ID: "b/model"}}, nil
}
func (f *fakeUpstream) Resolve(_ context.Context, id string) (upstreamv1.ResolvedTarget, error) {
	f.resolves++
	return upstreamv1.ResolvedTarget{ID: id, Protocol: "openai-chat", NativeModel: "native", BaseURL: "https://example.test"}, nil
}
func (f *fakeUpstream) Evaluate(_ context.Context, ids []string) ([]upstreamv1.CandidateEvaluation, error) {
	f.evals++
	if f.candidates != nil {
		return f.candidates, nil
	}
	out := make([]upstreamv1.CandidateEvaluation, len(ids))
	for i, id := range ids {
		out[i] = upstreamv1.CandidateEvaluation{ID: id, Available: true}
	}
	return out, nil
}
func (f *fakeUpstream) Report(context.Context, upstreamv1.ResultReport) (upstreamv1.ResultResponse, error) {
	return upstreamv1.ResultResponse{}, f.reportErr
}
func (f *fakeUpstream) WebSearch(context.Context, upstreamv1.WebSearchRequest) (upstreamv1.WebSearchResponse, error) {
	return upstreamv1.WebSearchResponse{}, nil
}

func newScheduler(t *testing.T, policy string, f *fakeUpstream) *Scheduler {
	t.Helper()
	ctx := context.Background()
	st, err := relaystore.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err = st.PutUserModel(ctx, relaystore.UserModel{Name: "model", Protocol: "auto", APIKey: "key", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err = st.PutGroup(ctx, relaystore.Group{ID: "g", UserModel: "model", PolicyType: policy, PolicyConfig: "{}"}); err != nil {
		t.Fatal(err)
	}
	if err = st.AddMembers(ctx, "g", []string{"a/model", "b/model"}); err != nil {
		t.Fatal(err)
	}
	return &Scheduler{Store: st, Upstream: f}
}

func TestCacheHitStillResolvesAndSkipsEvaluate(t *testing.T) {
	ctx := context.Background()
	f := &fakeUpstream{}
	s := newScheduler(t, "sticky", f)
	sel, err := s.Select(ctx, "model", map[string]bool{})
	if err != nil {
		t.Fatal(err)
	}
	if sel.Target.ID != "a/model" || f.evals != 1 || f.resolves != 1 {
		t.Fatalf("first selection: %+v eval=%d resolve=%d", sel, f.evals, f.resolves)
	}
	_, err = s.Select(ctx, "model", map[string]bool{})
	if err != nil {
		t.Fatal(err)
	}
	if f.evals != 1 || f.resolves != 2 {
		t.Fatalf("cache path eval=%d resolve=%d", f.evals, f.resolves)
	}
}

func TestFailoverUsesNextUntriedMember(t *testing.T) {
	f := &fakeUpstream{}
	s := newScheduler(t, "failover", f)
	sel, err := s.Select(context.Background(), "model", map[string]bool{"a/model": true})
	if err != nil || sel.Target.ID != "b/model" {
		t.Fatalf("selection=%+v err=%v", sel, err)
	}
}

func TestRoundRobinAndLeastUsed(t *testing.T) {
	ctx := context.Background()
	rr := newScheduler(t, "round_robin", &fakeUpstream{})
	first, _ := rr.Select(ctx, "model", map[string]bool{})
	second, _ := rr.Select(ctx, "model", map[string]bool{})
	if first.Target.ID != "a/model" || second.Target.ID != "b/model" {
		t.Fatalf("round robin: %s then %s", first.Target.ID, second.Target.ID)
	}
	f := &fakeUpstream{candidates: []upstreamv1.CandidateEvaluation{{ID: "a/model", Available: true, Score: 10}, {ID: "b/model", Available: true, Score: 2}}}
	least := newScheduler(t, "least_used", f)
	sel, err := least.Select(ctx, "model", map[string]bool{})
	if err != nil || sel.Target.ID != "b/model" {
		t.Fatalf("least used selection=%+v err=%v", sel, err)
	}
}

func TestDynamicPolicyExplicitlyUnsupported(t *testing.T) {
	s := newScheduler(t, "dynamic", &fakeUpstream{})
	_, err := s.Select(context.Background(), "model", map[string]bool{})
	if !errors.Is(err, ErrUnsupportedPolicy) {
		t.Fatalf("err=%v", err)
	}
}
