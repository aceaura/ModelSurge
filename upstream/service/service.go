package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/aceaura/ModelSurge/upstream/account"
	"github.com/aceaura/ModelSurge/upstream/contract/upstreamv1"
	"github.com/aceaura/ModelSurge/upstream/ir"
	"github.com/aceaura/ModelSurge/upstream/redisx"
	"github.com/aceaura/ModelSurge/upstream/upstreamstore"
)

type Service struct {
	Store                    *upstreamstore.Store
	Manager                  *account.Manager
	KiroHTTPClient           *http.Client
	KiroFirstTokenTimeout    time.Duration
	KiroStreamingReadTimeout time.Duration
	KiroWebSearchInject      bool
	AccessLogEnabled         bool
	// Redis 可选热态层（Evaluate 读旁路 + Half-Open 试探锁）；
	// nil = 纯 DB 路径（模式一现行为）。
	Redis *redisx.Client
}

func NewService(store *upstreamstore.Store, manager *account.Manager) *Service {
	return &Service{Store: store, Manager: manager, AccessLogEnabled: true}
}

func (s *Service) Models(ctx context.Context) ([]upstreamv1.ModelSummary, error) {
	models, err := s.Store.ListModels(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	out := make([]upstreamv1.ModelSummary, 0, len(models))
	for _, m := range models {
		if upstreamv1.ValidateProtocol(m.Protocol) != nil {
			continue
		}
		out = append(out, upstreamv1.ModelSummary{ID: m.ID, DisplayName: m.DisplayName, Protocol: m.Protocol, Available: m.Enabled && !m.CooldownUntil.After(now)})
	}
	return out, nil
}

func (s *Service) Resolve(ctx context.Context, id string) (upstreamv1.ResolvedTarget, *upstreamv1.Error) {
	m, err := s.Store.GetModel(ctx, id)
	if err != nil {
		return upstreamv1.ResolvedTarget{}, internalError()
	}
	if m == nil {
		return upstreamv1.ResolvedTarget{}, &upstreamv1.Error{Code: upstreamv1.CodeNotFound, Message: "target not found"}
	}
	if err := upstreamv1.ValidateProtocol(m.Protocol); err != nil {
		return upstreamv1.ResolvedTarget{}, &upstreamv1.Error{Code: upstreamv1.CodeTargetUnavailable, Message: "unsupported outbound protocol"}
	}
	if !m.Enabled || (m.CooldownUntil.After(time.Now()) && !s.probeGranted(ctx, m.ID, entryFromModel(m))) {
		return upstreamv1.ResolvedTarget{}, &upstreamv1.Error{Code: upstreamv1.CodeTargetUnavailable, Message: "target unavailable", Retryable: true}
	}
	var acc *account.Account
	for _, a := range s.Manager.Status() {
		if a.Name == m.Account {
			aa := a
			acc = &aa
			break
		}
	}
	if acc == nil || !acc.Enabled || acc.Disabled {
		return upstreamv1.ResolvedTarget{}, &upstreamv1.Error{Code: upstreamv1.CodeTargetUnavailable, Message: "target unavailable", Retryable: true}
	}
	native := m.NativeModel
	if acc.Type == account.TypeKiro {
		t := upstreamv1.ResolvedTarget{ID: m.ID, Protocol: m.Protocol}
		if err := upstreamv1.ValidateResolvedTarget(t); err != nil {
			return upstreamv1.ResolvedTarget{}, &upstreamv1.Error{Code: upstreamv1.CodeInternal, Message: "invalid target configuration"}
		}
		return t, nil
	}
	var ov *ir.Overrides
	if len(m.RequestOverrides) > 0 && string(m.RequestOverrides) != "null" {
		_ = json.Unmarshal(m.RequestOverrides, &ov)
	}
	t := upstreamv1.ResolvedTarget{ID: m.ID, Account: m.Account, Protocol: m.Protocol, NativeModel: native, BaseURL: m.BaseURL, APIKey: acc.APIKey, Headers: cloneHeaders(m.Headers), RequestOverrides: ov, Runtime: upstreamv1.RuntimeMetadata{AccountType: acc.Type, MaxInputTokens: m.ContextWindow}}
	if err := upstreamv1.ValidateResolvedTarget(t); err != nil {
		return upstreamv1.ResolvedTarget{}, &upstreamv1.Error{Code: upstreamv1.CodeInternal, Message: "invalid target configuration"}
	}
	return t, nil
}

// windowOf 候选上下文窗口：物化列（model_limits 配置）优先；0=未知时 kiro
// 账号回落动态模型缓存（非 kiro 或缓存未命中仍为 0，调度不过滤）。
func (s *Service) windowOf(e stateCacheEntry) int {
	if e.Window > 0 {
		return e.Window
	}
	rt := s.Manager.KiroRuntimeOf(e.Account)
	if rt == nil || rt.Models == nil || e.Native == "" {
		return 0
	}
	if w := rt.Models.LookupMaxInputTokens(e.Native); w > 0 {
		return int(w)
	}
	return 0
}

func (s *Service) Evaluate(ctx context.Context, ids []string) ([]upstreamv1.CandidateEvaluation, error) {
	entries, err := s.modelStates(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make([]upstreamv1.CandidateEvaluation, 0, len(ids))
	now := time.Now().Unix()
	accountRequests := map[string]int64{}
	for _, a := range s.Manager.Status() {
		accountRequests[a.Name] = a.Stats.Requests
	}
	for i, id := range ids {
		e := entries[i]
		window := s.windowOf(e)
		c := upstreamv1.CandidateEvaluation{ID: id, ContextWindow: window}
		switch {
		case !e.Found:
			c.ExclusionReason = "not_found"
		case !e.Enabled:
			c.ExclusionReason = "disabled"
		case e.Cooldown > now:
			c.ExclusionReason = "cooling_down"
			// Half-Open：熔断退避类冷却经全局试探锁收敛放行（设计 2.4/2.5）。
			if probeEligible(e) && s.acquireProbe(ctx, id) {
				c = upstreamv1.CandidateEvaluation{ID: id, Available: true, Score: float64(accountRequests[e.Account]), ContextWindow: window}
			}
		default:
			c.Available = true
			c.Score = float64(accountRequests[e.Account])
		}
		out = append(out, c)
	}
	return out, nil
}

func (s *Service) WebSearch(ctx context.Context, req upstreamv1.WebSearchRequest) (upstreamv1.WebSearchResponse, error) {
	m, err := s.Store.GetModel(ctx, req.TargetID)
	if err != nil || m == nil || req.Query == "" {
		return upstreamv1.WebSearchResponse{}, fmt.Errorf("invalid web search request")
	}
	rt := s.Manager.KiroRuntimeOf(m.Account)
	if rt == nil {
		return upstreamv1.WebSearchResponse{}, fmt.Errorf("kiro runtime unavailable")
	}
	id, results, err := rt.Client.CallWebSearch(ctx, req.Query)
	if err != nil {
		return upstreamv1.WebSearchResponse{}, err
	}
	out := make([]upstreamv1.WebSearchResult, len(results))
	for i, r := range results {
		out[i] = upstreamv1.WebSearchResult{Title: r.Title, URL: r.URL, Snippet: r.Snippet}
	}
	return upstreamv1.WebSearchResponse{ID: id, Results: out}, nil
}

func (s *Service) Report(ctx context.Context, r upstreamv1.ResultReport) (upstreamv1.ResultResponse, error) {
	if r.ReportID == "" || r.TargetID == "" || r.Outcome == "" {
		return upstreamv1.ResultResponse{}, fmt.Errorf("missing report fields")
	}
	m, _ := s.Store.GetModel(ctx, r.TargetID)
	action := upstreamv1.ActionStop
	if m != nil && r.Outcome != "normal" {
		isKiro := m.Protocol == "kiro"
		switch {
		case r.Outcome == "context_exceeded":
			// 超限是请求侧问题，目标无恙：不重试不换目标，也不进熔断计数。
			r.Action = upstreamv1.ActionStop
			action = r.Action
		case isKiro && (r.Status == 401 || r.Status == 403) && r.Attempt == 0:
			if rt := s.Manager.KiroRuntimeOf(m.Account); rt != nil && rt.Auth.ForceRefresh(ctx) == nil {
				action = upstreamv1.ActionRetryTarget
				r.Action = action
				r.Outcome = "retrying"
			}
		case isKiro && r.Status == 402:
			until := time.Now().Add(time.Hour)
			if rt := s.Manager.KiroRuntimeOf(m.Account); rt != nil {
				if reset := rt.QuotaCooldownUntil(ctx); reset.After(time.Now()) {
					until = reset
				}
			}
			r.Action = upstreamv1.ActionSwitchTarget
			r.CooldownUntil = until
			action = r.Action
		case isKiro && r.Reason == account.ReasonInvalidModelID:
			r.Action = upstreamv1.ActionSwitchTarget
			r.Outcome = "invalid_model"
			action = r.Action
		case r.Status == 429:
			r.Action = upstreamv1.ActionSwitchTarget
			action = r.Action
		case r.Attempt == 0:
			r.Action = upstreamv1.ActionRetryTarget
			r.Outcome = "retrying"
			action = r.Action
		default:
			r.Action = upstreamv1.ActionSwitchTarget
			action = r.Action
		}
	}
	applied, err := s.Store.ApplyReport(ctx, r)
	if err != nil {
		return upstreamv1.ResultResponse{}, err
	}
	if applied {
		// PG 权威写后失效读旁路缓存（设计 2.4）。
		s.invalidateState(ctx, r.TargetID)
	}
	if applied && m != nil {
		switch {
		case r.Outcome == "normal":
			s.Manager.ReportSuccess(m.Account)
		case r.Outcome == "retrying" || r.Outcome == "invalid_model":
			// No account penalty while retrying the same target or switching subscriptions.
		case r.Status == 402:
			s.Manager.ReportRecoverable(m.Account, r.CooldownUntil)
		case r.Status == 401 || r.Status == 403:
			s.Manager.ReportAuthFailure(m.Account)
		case r.Status == 429:
			s.Manager.ReportLimit(m.Account, r.Message)
		default:
			s.Manager.ReportTransientFailure(m.Account)
		}
		if r.Usage != (upstreamv1.Usage{}) {
			s.Manager.ReportUsage(m.Account, account.Usage{InputTokens: r.Usage.InputTokens, OutputTokens: r.Usage.OutputTokens, CacheRead: r.Usage.CacheRead, CacheCreation: r.Usage.CacheCreation})
		}
	}
	return upstreamv1.ResultResponse{Applied: applied, Action: action}, nil
}

func internalError() *upstreamv1.Error {
	return &upstreamv1.Error{Code: upstreamv1.CodeInternal, Message: "internal error", Retryable: true}
}
func cloneHeaders(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
