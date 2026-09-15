// Package replayv1 defines the agent-to-replay HTTP JSON contract.
package replayv1

import (
	"context"
	"encoding/json"
	"time"
)

const BasePath = "/internal/v1"

type requestIDContextKey struct{}

func WithRequestID(ctx context.Context, requestID string) context.Context {
	return context.WithValue(ctx, requestIDContextKey{}, requestID)
}

func RequestIDFromContext(ctx context.Context) string {
	requestID, _ := ctx.Value(requestIDContextKey{}).(string)
	return requestID
}

const (
	CodeUnauthorized      = "unauthorized"
	CodeNotFound          = "not_found"
	CodeInvalidRequest    = "invalid_request"
	CodeTargetUnavailable = "target_unavailable"
	CodeConflict          = "conflict"
	CodeInternal          = "internal_error"
	// CodeContextTooLarge 估算 token 超过全部可用候选的上下文窗口
	// （调度层前置过滤，非上游错误）。
	CodeContextTooLarge = "context_too_large"
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
	// EstTokens 估算输入+输出预算合计（Agent 估算；0=未送，调度不过滤）。
	EstTokens int `json:"est_tokens,omitempty"`
	// CompressOf 非空 = Agent 内部压缩调用（原模型超限后的回退重发），
	// 值为原 user model 名。Replay 见非空跳过 key 校验（信任 Agent 已鉴权
	// 原请求），调度与评估不豁免。仅内部契约，外部客户端无法注入。
	CompressOf string `json:"compress_of,omitempty"`
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

type KiroExecuteRequest struct {
	RequestID string          `json:"request_id"`
	TargetID  string          `json:"target_id"`
	Request   json.RawMessage `json:"request"`
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
	BaseURL          string            `json:"base_url,omitempty"`
	Credential       string            `json:"credential,omitempty"`
	Headers          map[string]string `json:"headers,omitempty"`
	RequestOverrides *RequestOverrides `json:"request_overrides,omitempty"`
	Runtime          RuntimeMetadata   `json:"runtime,omitempty"`
	// CompressModel 本 user model 配置的压缩备用模型名（空=未配置压缩回退）。
	// Agent 据此在超限/不可用失败时换模型重发。非机密（仅模型名）。
	CompressModel string `json:"compress_model,omitempty"`
}

func (l TargetLease) MarshalJSON() ([]byte, error) {
	if l.Protocol == "kiro" {
		return json.Marshal(struct {
			RequestID     string `json:"request_id"`
			GroupID       string `json:"group_id"`
			TargetID      string `json:"target_id"`
			Protocol      string `json:"protocol"`
			CompressModel string `json:"compress_model,omitempty"`
		}{RequestID: l.RequestID, GroupID: l.GroupID, TargetID: l.TargetID, Protocol: l.Protocol, CompressModel: l.CompressModel})
	}
	type alias TargetLease
	if l.Runtime == (RuntimeMetadata{}) {
		return json.Marshal(struct {
			alias
			Runtime *RuntimeMetadata `json:"runtime,omitempty"`
		}{alias: alias(l)})
	}
	return json.Marshal(alias(l))
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
	Status    int    `json:"status,omitempty"`
	Reason    string `json:"reason,omitempty"`
	// CompressModel dispatch 失败时携带（鉴权通过后的失败）：本 user model
	// 配置的压缩备用模型名，供 Agent 显式压缩请求换模型重发。鉴权类失败
	// 不携带（未鉴权请求不触发压缩）。
	CompressModel string `json:"compress_model,omitempty"`
}

func (e Error) Error() string { return e.Code + ": " + e.Message }

type ErrorEnvelope struct {
	Error Error `json:"error"`
}
