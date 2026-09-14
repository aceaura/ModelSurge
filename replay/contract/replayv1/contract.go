// Package replayv1 defines the agent-to-replay HTTP JSON contract.
package replayv1

import "time"

const BasePath = "/internal/v1"

const (
	CodeUnauthorized      = "unauthorized"
	CodeNotFound          = "not_found"
	CodeInvalidRequest    = "invalid_request"
	CodeTargetUnavailable = "target_unavailable"
	CodeConflict          = "conflict"
	CodeInternal          = "internal_error"
)

type HealthResponse struct {
	Status   string `json:"status"`
	Database string `json:"database"`
	Upstream string `json:"upstream,omitempty"`
}

type ModelSummary struct {
	Name     string `json:"name"`
	Protocol string `json:"protocol"`
	Enabled  bool   `json:"enabled"`
}

type ModelsResponse struct {
	Models []ModelSummary `json:"models"`
}

type DispatchRequest struct {
	Model           string   `json:"model"`
	InboundProtocol string   `json:"inbound_protocol"`
	ClientKey       string   `json:"client_key"`
	RequestID       string   `json:"request_id"`
	TriedIDs        []string `json:"tried_ids,omitempty"`
}

type ThinkingOverride struct {
	Enabled      bool   `json:"enabled"`
	BudgetTokens int    `json:"budget_tokens,omitempty"`
	Effort       string `json:"effort,omitempty"`
}

type RequestOverrides struct {
	Thinking    *ThinkingOverride `json:"thinking,omitempty"`
	Temperature *float64          `json:"temperature,omitempty"`
	TopP        *float64          `json:"top_p,omitempty"`
	MaxTokens   *int              `json:"max_tokens,omitempty"`
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

// TargetLease is secret-bearing and must not be persisted by agent or replay.
type TargetLease struct {
	RequestID        string            `json:"request_id"`
	GroupID          string            `json:"group_id"`
	TargetID         string            `json:"target_id"`
	Protocol         string            `json:"protocol"`
	NativeModel      string            `json:"native_model"`
	BaseURL          string            `json:"base_url"`
	Credential       string            `json:"credential,omitempty"`
	Headers          map[string]string `json:"headers,omitempty"`
	RequestOverrides *RequestOverrides `json:"request_overrides,omitempty"`
	Runtime          RuntimeMetadata   `json:"runtime,omitempty"`
}

type Usage struct {
	InputTokens   int64 `json:"input_tokens,omitempty"`
	OutputTokens  int64 `json:"output_tokens,omitempty"`
	CacheRead     int64 `json:"cache_read,omitempty"`
	CacheCreation int64 `json:"cache_creation,omitempty"`
}

type ResultReport struct {
	ReportID  string    `json:"report_id"`
	RequestID string    `json:"request_id"`
	GroupID   string    `json:"group_id"`
	TargetID  string    `json:"target_id"`
	Outcome   string    `json:"outcome"`
	Status    int       `json:"status,omitempty"`
	Reason    string    `json:"reason,omitempty"`
	Attempt   int       `json:"attempt,omitempty"`
	Message   string    `json:"message,omitempty"`
	Usage     Usage     `json:"usage,omitempty"`
	At        time.Time `json:"at"`
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
	Field     string `json:"field,omitempty"`
}

func (e Error) Error() string { return e.Code + ": " + e.Message }

type ErrorEnvelope struct {
	Error Error `json:"error"`
}
