package schedule

import (
	"context"
	"errors"
	"github.com/aceaura/ModelSurge/upstream/dialect"
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

func newScheduler(t *testing.T, policy string, f Upstream) *Scheduler {
	t.Helper()
	ctx := context.Background()
	st, err := relaystore.Open(dialect.SQLite, filepath.Join(t.TempDir(), "relay.db"))
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
	sel, err := s.Select(ctx, "model", map[string]bool{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if sel.Target.ID != "a/model" || f.evals != 1 || f.resolves != 1 {
		t.Fatalf("first selection: %+v eval=%d resolve=%d", sel, f.evals, f.resolves)
	}
	_, err = s.Select(ctx, "model", map[string]bool{}, 0)
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
	sel, err := s.Select(context.Background(), "model", map[string]bool{"a/model": true}, 0)
	if err != nil || sel.Target.ID != "b/model" {
		t.Fatalf("selection=%+v err=%v", sel, err)
	}
}

func TestRoundRobinAndLeastUsed(t *testing.T) {
	ctx := context.Background()
	rr := newScheduler(t, "round_robin", &fakeUpstream{})
	first, _ := rr.Select(ctx, "model", map[string]bool{}, 0)
	second, _ := rr.Select(ctx, "model", map[string]bool{}, 0)
	if first.Target.ID != "a/model" || second.Target.ID != "b/model" {
		t.Fatalf("round robin: %s then %s", first.Target.ID, second.Target.ID)
	}
	f := &fakeUpstream{candidates: []upstreamv1.CandidateEvaluation{{ID: "a/model", Available: true, Score: 10}, {ID: "b/model", Available: true, Score: 2}}}
	least := newScheduler(t, "least_used", f)
	sel, err := least.Select(ctx, "model", map[string]bool{}, 0)
	if err != nil || sel.Target.ID != "b/model" {
		t.Fatalf("least used selection=%+v err=%v", sel, err)
	}
}

func TestDynamicPolicyExplicitlyUnsupported(t *testing.T) {
	s := newScheduler(t, "dynamic", &fakeUpstream{})
	_, err := s.Select(context.Background(), "model", map[string]bool{}, 0)
	if !errors.Is(err, ErrUnsupportedPolicy) {
		t.Fatalf("err=%v", err)
	}
}

// 10.4 评估路径窗口过滤：A(64k) B(200k)，est=100k 选 B；est=300k 报
// ErrContextTooLarge；窗口全 0 不过滤；被排除候选不进 Resolve。
func TestSelectFiltersByContextWindow(t *testing.T) {
	ctx := context.Background()
	f := &fakeUpstream{candidates: []upstreamv1.CandidateEvaluation{
		{ID: "a/model", Available: true, ContextWindow: 64000},
		{ID: "b/model", Available: true, ContextWindow: 200000},
	}}
	s := newScheduler(t, "failover", f)
	sel, err := s.Select(ctx, "model", map[string]bool{}, 100000)
	if err != nil || sel.Target.ID != "b/model" {
		t.Fatalf("selection=%+v err=%v", sel, err)
	}
	if f.resolves != 1 {
		t.Fatalf("resolves=%d, want 1（A 不应触发 Resolve）", f.resolves)
	}

	// est=300k：A B 全装不下 → ErrContextTooLarge（新 scheduler 避开上一步写入的缓存）
	_, err = newScheduler(t, "failover", f).Select(ctx, "model", map[string]bool{}, 300000)
	if !errors.Is(err, ErrContextTooLarge) {
		t.Fatalf("err=%v, want ErrContextTooLarge", err)
	}

	// 窗口全 0（未知）：不过滤
	f2 := &fakeUpstream{}
	s2 := newScheduler(t, "failover", f2)
	sel, err = s2.Select(ctx, "model", map[string]bool{}, 300000)
	if err != nil || sel.Target.ID != "a/model" {
		t.Fatalf("selection=%+v err=%v", sel, err)
	}

	// 不可用候选与窗口排除并存：无可用候选仍走 no available target，
	// 有可用但全被窗口排除才报 ErrContextTooLarge
	f3 := &fakeUpstream{candidates: []upstreamv1.CandidateEvaluation{
		{ID: "a/model", Available: false, ExclusionReason: "cooling_down"},
		{ID: "b/model", Available: true, ContextWindow: 64000},
	}}
	s3 := newScheduler(t, "failover", f3)
	_, err = s3.Select(ctx, "model", map[string]bool{}, 100000)
	if !errors.Is(err, ErrContextTooLarge) {
		t.Fatalf("err=%v, want ErrContextTooLarge", err)
	}
	f4 := &fakeUpstream{candidates: []upstreamv1.CandidateEvaluation{
		{ID: "a/model", Available: false, ExclusionReason: "cooling_down"},
		{ID: "b/model", Available: false, ExclusionReason: "cooling_down"},
	}}
	s4 := newScheduler(t, "failover", f4)
	_, err = s4.Select(ctx, "model", map[string]bool{}, 100000)
	if err == nil || errors.Is(err, ErrContextTooLarge) {
		t.Fatalf("err=%v, want no available target", err)
	}
}

// 10.4 缓存命中但窗口装不下：绕过缓存落评估选大窗口目标，缓存不清。
func TestSelectCacheBypassOnWindowTooSmall(t *testing.T) {
	ctx := context.Background()
	resolved := map[string]upstreamv1.ResolvedTarget{}
	f := &windowFakeUpstream{evals: map[string]upstreamv1.CandidateEvaluation{}, resolved: resolved}
	s := newScheduler(t, "sticky", f)
	f.resolved["a/model"] = upstreamv1.ResolvedTarget{ID: "a/model", Protocol: "openai-chat", NativeModel: "native", BaseURL: "https://example.test", Runtime: upstreamv1.RuntimeMetadata{MaxInputTokens: 64000}}
	f.resolved["b/model"] = upstreamv1.ResolvedTarget{ID: "b/model", Protocol: "openai-chat", NativeModel: "native", BaseURL: "https://example.test", Runtime: upstreamv1.RuntimeMetadata{MaxInputTokens: 200000}}
	f.evals["a/model"] = upstreamv1.CandidateEvaluation{ID: "a/model", Available: true, ContextWindow: 64000}
	f.evals["b/model"] = upstreamv1.CandidateEvaluation{ID: "b/model", Available: true, ContextWindow: 200000}

	// 第一次选 a/model 并写缓存
	sel, err := s.Select(ctx, "model", map[string]bool{}, 0)
	if err != nil || sel.Target.ID != "a/model" {
		t.Fatalf("first selection=%+v err=%v", sel, err)
	}
	// 第二次大请求：缓存目标 64k 装不下 → 绕过缓存落评估选 b/model
	// （bypass 分支不动缓存；评估路径选中后照常写缓存）
	sel, err = s.Select(ctx, "model", map[string]bool{}, 100000)
	if err != nil || sel.Target.ID != "b/model" {
		t.Fatalf("bypass selection=%+v err=%v", sel, err)
	}
	// 第三次小请求：缓存已被评估路径改写为 b/model，正常命中
	sel, err = s.Select(ctx, "model", map[string]bool{}, 0)
	if err != nil || sel.Target.ID != "b/model" {
		t.Fatalf("cache after bypass selection=%+v err=%v", sel, err)
	}
}

// windowFakeUpstream Resolve/Evaluate 按 id 返回预置窗口与目标。
type windowFakeUpstream struct {
	resolved map[string]upstreamv1.ResolvedTarget
	evals    map[string]upstreamv1.CandidateEvaluation
}

func (f *windowFakeUpstream) Models(context.Context) ([]upstreamv1.ModelSummary, error) {
	return []upstreamv1.ModelSummary{{ID: "a/model"}, {ID: "b/model"}}, nil
}
func (f *windowFakeUpstream) Resolve(_ context.Context, id string) (upstreamv1.ResolvedTarget, error) {
	return f.resolved[id], nil
}
func (f *windowFakeUpstream) Evaluate(_ context.Context, ids []string) ([]upstreamv1.CandidateEvaluation, error) {
	out := make([]upstreamv1.CandidateEvaluation, 0, len(ids))
	for _, id := range ids {
		out = append(out, f.evals[id])
	}
	return out, nil
}
func (f *windowFakeUpstream) Report(context.Context, upstreamv1.ResultReport) (upstreamv1.ResultResponse, error) {
	return upstreamv1.ResultResponse{}, nil
}
func (f *windowFakeUpstream) WebSearch(context.Context, upstreamv1.WebSearchRequest) (upstreamv1.WebSearchResponse, error) {
	return upstreamv1.WebSearchResponse{}, nil
}
