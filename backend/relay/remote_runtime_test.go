package relay

import (
	"context"
	"path/filepath"
	"testing"

	"relayd/backend/config"
	"relayd/backend/contract/upstreamv1"
	"relayd/backend/ir"
	"relayd/backend/relaystore"
	"relayd/backend/schedule"
)

type runtimeDecoder struct {
	model string
	max   int
	fake  bool
}

func (d *runtimeDecoder) Feed(string, string) ([]ir.Event, error) { return nil, nil }
func (d *runtimeDecoder) Finish() []ir.Event                      { return nil }
func (d *runtimeDecoder) SetModel(v string)                       { d.model = v }
func (d *runtimeDecoder) SetMaxInputTokens(v int)                 { d.max = v }
func (d *runtimeDecoder) SetFakeReasoning(v bool)                 { d.fake = v }

func TestRemoteKiroRuntimeAppliedWithoutManager(t *testing.T) {
	f := NewRemoteForwarder(&config.Config{}, nil)
	d := &runtimeDecoder{}
	f.ApplyResolvedRuntimeForTest(d, upstreamv1.ResolvedTarget{ID: "k/m", Account: "k", Protocol: "kiro", NativeModel: "native", BaseURL: "https://example.test", Runtime: upstreamv1.RuntimeMetadata{AccountType: "kiro", MaxInputTokens: 123456, FakeReasoning: true}}, &ir.Request{Model: "client-model"})
	if d.model != "client-model" || d.max != 123456 || !d.fake {
		t.Fatalf("runtime not applied: %+v", d)
	}
}

type reportUpstream struct {
	called   bool
	canceled bool
}

func (u *reportUpstream) Models(context.Context) ([]upstreamv1.ModelSummary, error) { return nil, nil }
func (u *reportUpstream) Resolve(context.Context, string) (upstreamv1.ResolvedTarget, error) {
	return upstreamv1.ResolvedTarget{}, nil
}
func (u *reportUpstream) Evaluate(context.Context, []string) ([]upstreamv1.CandidateEvaluation, error) {
	return nil, nil
}
func (u *reportUpstream) Report(ctx context.Context, _ upstreamv1.ResultReport) error {
	u.called = true
	u.canceled = ctx.Err() != nil
	return nil
}

func TestReportSurvivesCanceledClientContext(t *testing.T) {
	store, err := relaystore.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.PutUserModel(ctx, relaystore.UserModel{Name: "m", Protocol: "auto", APIKey: "k", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutGroup(ctx, relaystore.Group{ID: "g", UserModel: "m", PolicyType: "sticky", PolicyConfig: "{}"}); err != nil {
		t.Fatal(err)
	}
	u := &reportUpstream{}
	s := &schedule.Scheduler{Store: store, Upstream: u}
	f := NewRemoteForwarder(&config.Config{}, s)
	clientCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if clientCtx.Err() == nil {
		t.Fatal("client context should be canceled")
	}
	f.reportRemote(schedule.Selection{GroupID: "g", Target: upstreamv1.ResolvedTarget{ID: "u/m"}}, upstreamv1.ResultReport{ReportID: "r", TargetID: "u/m", Outcome: "normal"})
	if !u.called || u.canceled {
		t.Fatalf("called=%v report context canceled=%v", u.called, u.canceled)
	}
}
