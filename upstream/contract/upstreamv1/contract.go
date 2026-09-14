// Package upstreamv1 defines the authenticated relay-to-upstream HTTP JSON contract.
package upstreamv1

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/aceaura/ModelSurge/upstream/ir"
)

const BasePath = "/internal/v1"

const (
	CodeUnauthorized      = "unauthorized"
	CodeNotFound          = "not_found"
	CodeTargetUnavailable = "target_unavailable"
	CodeInvalidRequest    = "invalid_request"
	CodeQuotaQueryFailed  = "quota_query_failed"
	CodeCredentialRefresh = "credential_refresh_failed"
	CodeInternal          = "internal_error"
)

type HealthResponse struct {
	Status   string `json:"status"`
	Database string `json:"database"`
}

type ModelSummary struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	Protocol    string `json:"protocol"`
	Available   bool   `json:"available"`
}

type ModelsResponse struct {
	Models []ModelSummary `json:"models"`
}

// ResolvedTarget is short-lived and secret-bearing. It must never be persisted by relay.
type ResolvedTarget struct {
	ID               string            `json:"id"`
	Account          string            `json:"account,omitempty"`
	Protocol         string            `json:"protocol"`
	NativeModel      string            `json:"native_model"`
	BaseURL          string            `json:"base_url"`
	APIKey           string            `json:"api_key,omitempty"`
	Headers          map[string]string `json:"headers,omitempty"`
	RequestOverrides *ir.Overrides     `json:"request_overrides,omitempty"`
	Runtime          RuntimeMetadata   `json:"runtime,omitempty"`
}

type RuntimeMetadata struct {
	AccountType      string            `json:"account_type,omitempty"`
	ProfileArn       string            `json:"profile_arn,omitempty"`
	MaxInputTokens   int               `json:"max_input_tokens,omitempty"`
	FakeReasoning    bool              `json:"fake_reasoning,omitempty"`
	WebSearch        bool              `json:"web_search,omitempty"`
	StreamingTimeout int64             `json:"streaming_timeout_ms,omitempty"`
	WebSearchRuntime *WebSearchRuntime `json:"web_search_runtime,omitempty"`
}

type WebSearchRuntime struct {
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
}

type WebSearchRequest struct {
	TargetID string `json:"target_id"`
	Query    string `json:"query"`
}

type WebSearchResult struct {
	Type    string `json:"type,omitempty"`
	Title   string `json:"title,omitempty"`
	URL     string `json:"url,omitempty"`
	Snippet string `json:"snippet,omitempty"`
}

type WebSearchResponse struct {
	ID      string            `json:"id"`
	Results []WebSearchResult `json:"results"`
}

func (r ResolvedTarget) String() string {
	return fmt.Sprintf("ResolvedTarget{ID:%q Protocol:%q NativeModel:%q BaseURL:%q APIKey:%q Headers:%v}",
		r.ID, r.Protocol, r.NativeModel, r.BaseURL, redact(r.APIKey), redactHeaders(r.Headers))
}

type EvaluateRequest struct {
	TargetIDs      []string `json:"target_ids"`
	Classification string   `json:"classification,omitempty"`
}

type CandidateEvaluation struct {
	ID              string  `json:"id"`
	Available       bool    `json:"available"`
	Score           float64 `json:"score"`
	QuotaClass      string  `json:"quota_class,omitempty"`
	ExclusionReason string  `json:"exclusion_reason,omitempty"`
}

type EvaluateResponse struct {
	Candidates []CandidateEvaluation `json:"candidates"`
}

type Usage struct {
	InputTokens   int64 `json:"input_tokens,omitempty"`
	OutputTokens  int64 `json:"output_tokens,omitempty"`
	CacheRead     int64 `json:"cache_read,omitempty"`
	CacheCreation int64 `json:"cache_creation,omitempty"`
}

type ResultReport struct {
	ReportID      string    `json:"report_id"`
	TargetID      string    `json:"target_id"`
	Outcome       string    `json:"outcome"`
	Status        int       `json:"status,omitempty"`
	Reason        string    `json:"reason,omitempty"`
	Attempt       int       `json:"attempt,omitempty"`
	Action        string    `json:"action,omitempty"`
	CooldownUntil time.Time `json:"cooldown_until,omitempty"`
	Message       string    `json:"message,omitempty"`
	Usage         Usage     `json:"usage,omitempty"`
	At            time.Time `json:"at"`
}

const (
	ActionStop         = "stop"
	ActionRetryTarget  = "retry_target"
	ActionSwitchTarget = "switch_target"
)

type ResultResponse struct {
	Applied bool   `json:"applied"`
	Action  string `json:"action,omitempty"`
}

type Error struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

type ErrorEnvelope struct {
	Error Error `json:"error"`
}

func (e Error) Error() string { return e.Code + ": " + e.Message }

func ValidateProtocol(p string) error {
	switch p {
	case "anthropic", "openai-chat", "openai-responses", "kiro", "codex":
		return nil
	default:
		return fmt.Errorf("unknown protocol %q", p)
	}
}

func ValidateResolvedTarget(t ResolvedTarget) error {
	if t.ID == "" || len(t.ID) > 512 || t.NativeModel == "" || len(t.NativeModel) > 512 {
		return fmt.Errorf("invalid target identity")
	}
	if err := ValidateProtocol(t.Protocol); err != nil {
		return err
	}
	if t.BaseURL == "" || len(t.BaseURL) > 4096 {
		return fmt.Errorf("invalid base_url")
	}
	return nil
}

func redact(s string) string {
	if s == "" {
		return ""
	}
	return "***"
}

func redactHeaders(h map[string]string) map[string]string {
	out := make(map[string]string, len(h))
	safe := map[string]bool{"content-type": true, "accept": true, "connection": true, "user-agent": true}
	for k, v := range h {
		if safe[strings.ToLower(k)] {
			out[k] = v
		} else {
			out[k] = "***"
		}
	}
	return out
}

// RedactedJSON is suitable for diagnostics; secret-bearing fields are replaced.
func (r ResolvedTarget) RedactedJSON() []byte {
	r.APIKey = redact(r.APIKey)
	r.Headers = redactHeaders(r.Headers)
	b, _ := json.Marshal(r)
	return b
}
