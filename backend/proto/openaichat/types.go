// Package openaichat 实现 OpenAI Chat Completions 协议（/v1/chat/completions）的 codec。
package openaichat

import "encoding/json"

// Name 协议标识。
const Name = "openai-chat"

// ---- 请求 DTO ----

type request struct {
	Model               string          `json:"model"`
	Messages            []message       `json:"messages"`
	MaxTokens           int             `json:"max_tokens,omitempty"`
	MaxCompletionTokens int             `json:"max_completion_tokens,omitempty"`
	Temperature         *float64        `json:"temperature,omitempty"`
	TopP                *float64        `json:"top_p,omitempty"`
	Stop                any             `json:"stop,omitempty"` // string 或 []string
	Stream              bool            `json:"stream,omitempty"`
	StreamOptions       *streamOptions  `json:"stream_options,omitempty"`
	Tools               []tool          `json:"tools,omitempty"`
	ToolChoice          any             `json:"tool_choice,omitempty"` // string 或 object
	ReasoningEffort     string          `json:"reasoning_effort,omitempty"`
	Metadata            json.RawMessage `json:"metadata,omitempty"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// message content 为 string 或 []part，用自定义 Unmarshal 兼容。
type message struct {
	Role             string          `json:"role,omitempty"`
	Content          json.RawMessage `json:"content,omitempty"`
	ReasoningContent string          `json:"reasoning_content,omitempty"`
	ToolCalls        []toolCall      `json:"tool_calls,omitempty"`
	ToolCallID       string          `json:"tool_call_id,omitempty"`
	Name             string          `json:"name,omitempty"`
}

type part struct {
	Type     string    `json:"type"` // text / image_url
	Text     string    `json:"text,omitempty"`
	ImageURL *imageURL `json:"image_url,omitempty"`
}

type imageURL struct {
	URL string `json:"url"`
}

type toolCall struct {
	Index    int          `json:"index,omitempty"`
	ID       string       `json:"id,omitempty"`
	Type     string       `json:"type"` // "function"
	Function functionCall `json:"function"`
}

type functionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON 字符串
}

type tool struct {
	Type     string   `json:"type"` // "function"
	Function toolFunc `json:"function"`
}

type toolFunc struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type toolChoiceNamed struct {
	Type     string `json:"type"` // "function"
	Function struct {
		Name string `json:"name"`
	} `json:"function"`
}

// ---- 响应 / chunk DTO ----

type response struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []choice `json:"choices"`
	Usage   *usage   `json:"usage,omitempty"`
}

type choice struct {
	Index        int      `json:"index"`
	Message      *message `json:"message,omitempty"`  // 非流式
	Delta        *message `json:"delta,omitempty"`    // 流式
	FinishReason string   `json:"finish_reason,omitempty"`
}

type usage struct {
	PromptTokens        int             `json:"prompt_tokens"`
	CompletionTokens    int             `json:"completion_tokens"`
	TotalTokens         int             `json:"total_tokens"`
	PromptTokensDetails *promptDetails  `json:"prompt_tokens_details,omitempty"`
}

type promptDetails struct {
	CachedTokens int `json:"cached_tokens,omitempty"`
}

// errorResponse OpenAI 错误外形：{"error":{...}}。
type errorResponse struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message"`
}

func marshal(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
