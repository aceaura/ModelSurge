package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"relayd/backend/account"
	"relayd/backend/contract/upstreamv1"
	"relayd/backend/ir"
	"relayd/backend/upstreamstore"
)

type Service struct {
	Store   *upstreamstore.Store
	Manager *account.Manager
}

func NewService(store *upstreamstore.Store, manager *account.Manager) *Service {
	return &Service{Store: store, Manager: manager}
}

func (s *Service) Models(ctx context.Context) ([]upstreamv1.ModelSummary, error) {
	models, err := s.Store.ListModels(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	out := make([]upstreamv1.ModelSummary, 0, len(models))
	for _, m := range models {
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
	if !m.Enabled || m.CooldownUntil.After(time.Now()) {
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
	baseURL := m.BaseURL
	headers := cloneHeaders(m.Headers)
	apiKey := acc.APIKey
	runtime := upstreamv1.RuntimeMetadata{AccountType: acc.Type}
	if acc.Type == account.TypeKiro {
		rt := s.Manager.KiroRuntimeOf(acc.Name)
		if rt == nil {
			return upstreamv1.ResolvedTarget{}, &upstreamv1.Error{Code: upstreamv1.CodeCredentialRefresh, Message: "credential runtime unavailable", Retryable: true}
		}
		if m.DisplayName != "*" {
			native, _ = rt.Resolve(m.DisplayName)
		}
		token, _, err := rt.Auth.GetAccessToken(ctx)
		if err != nil {
			return upstreamv1.ResolvedTarget{}, &upstreamv1.Error{Code: upstreamv1.CodeCredentialRefresh, Message: "credential refresh failed", Retryable: true}
		}
		baseURL = rt.Auth.ChatHost()
		headers = account.KiroHeaders(rt.Auth.Fingerprint(), token, account.TargetGenerateAssistantResponse)
		headers["Connection"] = "close"
		apiKey = ""
		runtime.FakeReasoning = acc.Kiro != nil && acc.Kiro.FakeReasoning
		runtime.WebSearch = acc.Kiro != nil && acc.Kiro.WebSearch
		runtime.MaxInputTokens = int(rt.Models.MaxInputTokens(native))
	}
	var ov *ir.Overrides
	if len(m.RequestOverrides) > 0 && string(m.RequestOverrides) != "null" {
		_ = json.Unmarshal(m.RequestOverrides, &ov)
	}
	t := upstreamv1.ResolvedTarget{ID: m.ID, Account: m.Account, Protocol: m.Protocol, NativeModel: native, BaseURL: baseURL, APIKey: apiKey, Headers: headers, RequestOverrides: ov, Runtime: runtime}
	if err := upstreamv1.ValidateResolvedTarget(t); err != nil {
		return upstreamv1.ResolvedTarget{}, &upstreamv1.Error{Code: upstreamv1.CodeInternal, Message: "invalid target configuration"}
	}
	return t, nil
}

func (s *Service) Evaluate(ctx context.Context, ids []string) ([]upstreamv1.CandidateEvaluation, error) {
	models, err := s.Store.ListModels(ctx)
	if err != nil {
		return nil, err
	}
	byID := map[string]upstreamstore.Model{}
	for _, m := range models {
		byID[m.ID] = m
	}
	out := make([]upstreamv1.CandidateEvaluation, 0, len(ids))
	now := time.Now()
	accountRequests := map[string]int64{}
	for _, a := range s.Manager.Status() {
		accountRequests[a.Name] = a.Stats.Requests
	}
	for _, id := range ids {
		m, ok := byID[id]
		c := upstreamv1.CandidateEvaluation{ID: id}
		if !ok {
			c.ExclusionReason = "not_found"
		} else if !m.Enabled {
			c.ExclusionReason = "disabled"
		} else if m.CooldownUntil.After(now) {
			c.ExclusionReason = "cooling_down"
		} else {
			c.Available = true
			c.Score = float64(accountRequests[m.Account])
		}
		out = append(out, c)
	}
	return out, nil
}

func (s *Service) Report(ctx context.Context, r upstreamv1.ResultReport) (bool, error) {
	if r.ReportID == "" || r.TargetID == "" || r.Outcome == "" {
		return false, fmt.Errorf("missing report fields")
	}
	applied, err := s.Store.ApplyReport(ctx, r)
	if err != nil {
		return false, err
	}
	m, _ := s.Store.GetModel(ctx, r.TargetID)
	if applied && m != nil {
		switch {
		case r.Outcome == "normal":
			s.Manager.ReportSuccess(m.Account)
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
	return applied, nil
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
