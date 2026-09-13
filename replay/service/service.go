// Package service implements replay scheduling and result orchestration.
package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aceaura/ModelSurge/replay/contract/replayv1"
	"github.com/aceaura/ModelSurge/replay/relaystore"
	"github.com/aceaura/ModelSurge/replay/schedule"
	"github.com/aceaura/ModelSurge/upstream/contract/upstreamv1"
)

type Service struct {
	Store     *relaystore.Store
	Scheduler *schedule.Scheduler
	Upstream  schedule.Upstream
}

func (s *Service) Models(ctx context.Context) ([]replayv1.ModelSummary, error) {
	models, err := s.Store.ListUserModels(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]replayv1.ModelSummary, 0, len(models))
	for _, model := range models {
		if model.Enabled {
			out = append(out, replayv1.ModelSummary{Name: model.Name, Protocol: model.Protocol, Enabled: true})
		}
	}
	return out, nil
}

func (s *Service) Dispatch(ctx context.Context, req replayv1.DispatchRequest) (replayv1.TargetLease, error) {
	if req.Model == "" || req.InboundProtocol == "" || req.ClientKey == "" || req.RequestID == "" {
		return replayv1.TargetLease{}, replayv1.Error{Code: replayv1.CodeInvalidRequest, Message: "model, inbound_protocol, client_key and request_id are required"}
	}
	configured, ok, err := s.Scheduler.Authenticate(ctx, req.Model, req.InboundProtocol, req.ClientKey)
	if err != nil {
		return replayv1.TargetLease{}, fmt.Errorf("authenticate user model: %w", err)
	}
	if !configured {
		return replayv1.TargetLease{}, replayv1.Error{Code: replayv1.CodeNotFound, Message: "user model not found"}
	}
	if !ok {
		return replayv1.TargetLease{}, replayv1.Error{Code: replayv1.CodeUnauthorized, Message: "user model authentication failed"}
	}
	tried := make(map[string]bool, len(req.TriedIDs))
	for _, id := range req.TriedIDs {
		tried[id] = true
	}
	selection, err := s.Scheduler.Select(ctx, req.Model, tried)
	if err != nil {
		if errors.Is(err, schedule.ErrUnsupportedPolicy) {
			return replayv1.TargetLease{}, replayv1.Error{Code: replayv1.CodeInvalidRequest, Message: err.Error()}
		}
		return replayv1.TargetLease{}, replayv1.Error{Code: replayv1.CodeTargetUnavailable, Message: err.Error(), Retryable: true}
	}
	return leaseFromTarget(req.RequestID, selection.GroupID, selection.Target), nil
}

func (s *Service) Report(ctx context.Context, report replayv1.ResultReport) (replayv1.ResultResponse, error) {
	if report.ReportID == "" || report.RequestID == "" || report.GroupID == "" || report.TargetID == "" || report.Outcome == "" {
		return replayv1.ResultResponse{}, replayv1.Error{Code: replayv1.CodeInvalidRequest, Message: "report_id, request_id, group_id, target_id and outcome are required"}
	}
	if report.At.IsZero() {
		report.At = time.Now().UTC()
	}
	storeOutcome := report.Outcome
	if report.Outcome == "abnormal" && report.Attempt == 0 && report.Status != 402 && report.Status != 429 && report.Reason != "INVALID_MODEL_ID" {
		storeOutcome = "normal"
	}
	if report.Reason == "INVALID_MODEL_ID" {
		storeOutcome = "normal"
	}
	applied, err := s.Store.ApplyReport(ctx, report.ReportID, report.RequestID, report.GroupID, report.TargetID, storeOutcome)
	if err != nil {
		return replayv1.ResultResponse{}, fmt.Errorf("apply replay result: %w", err)
	}
	upstreamReport := upstreamv1.ResultReport{
		ReportID: report.ReportID,
		TargetID: report.TargetID,
		Outcome:  report.Outcome,
		Status:   report.Status,
		Reason:   report.Reason,
		Attempt:  report.Attempt,
		Message:  report.Message,
		Usage: upstreamv1.Usage{
			InputTokens: report.Usage.InputTokens, OutputTokens: report.Usage.OutputTokens,
			CacheRead: report.Usage.CacheRead, CacheCreation: report.Usage.CacheCreation,
		},
		At: report.At,
	}
	upstreamResult, err := s.Upstream.Report(ctx, upstreamReport)
	if err != nil {
		return replayv1.ResultResponse{Applied: applied}, fmt.Errorf("report upstream result: %w", err)
	}
	return replayv1.ResultResponse{Applied: applied, Action: upstreamResult.Action}, nil
}

func (s *Service) WebSearch(ctx context.Context, req replayv1.WebSearchRequest) (replayv1.WebSearchResponse, error) {
	if req.TargetID == "" || req.Query == "" {
		return replayv1.WebSearchResponse{}, replayv1.Error{Code: replayv1.CodeInvalidRequest, Message: "target_id and query are required"}
	}
	out, err := s.Upstream.WebSearch(ctx, upstreamv1.WebSearchRequest{TargetID: req.TargetID, Query: req.Query})
	if err != nil {
		return replayv1.WebSearchResponse{}, err
	}
	results := make([]replayv1.WebSearchResult, len(out.Results))
	for i, r := range out.Results {
		results[i] = replayv1.WebSearchResult{Type: r.Type, Title: r.Title, URL: r.URL, Snippet: r.Snippet}
	}
	return replayv1.WebSearchResponse{ID: out.ID, Results: results}, nil
}

func leaseFromTarget(requestID, groupID string, target upstreamv1.ResolvedTarget) replayv1.TargetLease {
	lease := replayv1.TargetLease{
		RequestID: requestID, GroupID: groupID, TargetID: target.ID, Protocol: target.Protocol,
		NativeModel: target.NativeModel, BaseURL: target.BaseURL, Credential: target.APIKey,
		Headers: target.Headers,
		Runtime: replayv1.RuntimeMetadata{
			AccountType: target.Runtime.AccountType, MaxInputTokens: target.Runtime.MaxInputTokens,
			FakeReasoning: target.Runtime.FakeReasoning, WebSearch: target.Runtime.WebSearch,
			StreamingTimeout: target.Runtime.StreamingTimeout,
		},
	}
	if x := target.Runtime.WebSearchRuntime; x != nil {
		lease.Runtime.WebSearchRuntime = &replayv1.WebSearchRuntime{URL: x.URL, Headers: x.Headers}
	}
	if o := target.RequestOverrides; o != nil {
		lease.RequestOverrides = &replayv1.RequestOverrides{
			Temperature: o.Temperature, TopP: o.TopP, MaxTokens: o.MaxTokens,
		}
		if o.Thinking != nil {
			lease.RequestOverrides.Thinking = &replayv1.ThinkingOverride{Enabled: o.Thinking.Enabled, BudgetTokens: o.Thinking.BudgetTokens, Effort: o.Thinking.Effort}
		}
	}
	return lease
}
