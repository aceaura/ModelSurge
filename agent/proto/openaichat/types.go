// Package openaichat 实现 OpenAI Chat Completions 协议（/v1/chat/completions）的 codec。
package openaichat

import (
	"encoding/json"
	"fmt"
)

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
	ParallelToolCalls   *bool           `json:"parallel_tool_calls,omitempty"`
	ReasoningEffort     string          `json:"reasoning_effort,omitempty"`
	ResponseFormat      *responseFormat `json:"response_format,omitempty"`
	Metadata            json.RawMessage `json:"metadata,omitempty"`
	// User 终端用户标识（滥用追踪/计费归属）。与 anthropic 的 metadata.user_id
	// 同一维度，IR 里统一放 Metadata["user_id"]。
	User string `json:"user,omitempty"`
	// 调参维度全用指针：penalty 的 0 是「不惩罚」、seed 的 0 是一个具体种子、
	// logprobs 的 false 是「明确不要」，与「没给」语义不同。
	PresencePenalty  *float64           `json:"presence_penalty,omitempty"`
	FrequencyPenalty *float64           `json:"frequency_penalty,omitempty"`
	Seed             *int               `json:"seed,omitempty"`
	N                *int               `json:"n,omitempty"`
	LogProbs         *bool              `json:"logprobs,omitempty"`
	TopLogProbs      *int               `json:"top_logprobs,omitempty"`
	LogitBias        map[string]float64 `json:"logit_bias,omitempty"`
	// ServiceTier 服务质量档位（auto/default/flex/scale/priority/fast）。
	ServiceTier string `json:"service_tier,omitempty"`
	// PromptCacheKey 提示缓存路由键。
	PromptCacheKey string `json:"prompt_cache_key,omitempty"`
	// Verbosity 输出啰嗦程度档位（low/medium/high），顶层字段。
	Verbosity string `json:"verbosity,omitempty"`
	// SafetyIdentifier 滥用检测标识，user 字段的官方替代。与 user 同一维度，
	// IR 里独立成槽：跨族到 anthropic 时映进 metadata.user_id（该槽被 user
	// 占了才丢）。
	SafetyIdentifier string `json:"safety_identifier,omitempty"`
	// Moderation 请求级审核策略 {model, policy{input/output}}。结构属于上游
	// 产品语义，代理不解析，原文透传。
	Moderation json.RawMessage `json:"moderation,omitempty"`
	// PromptCacheOptions 显式缓存断点控制 {mode, ttl, ...}。同样原文透传。
	PromptCacheOptions json.RawMessage `json:"prompt_cache_options,omitempty"`
}

// responseFormat 结构化输出：type 为 text / json_object / json_schema。
type responseFormat struct {
	Type       string      `json:"type"`
	JSONSchema *jsonSchema `json:"json_schema,omitempty"`
}

type jsonSchema struct {
	Name   string          `json:"name,omitempty"`
	Strict *bool           `json:"strict,omitempty"`
	Schema json.RawMessage `json:"schema,omitempty"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// message content 为 string 或 []part，用自定义 Unmarshal 兼容。
type message struct {
	Role             string          `json:"role,omitempty"`
	Content          json.RawMessage `json:"content,omitempty"`
	ReasoningContent string          `json:"reasoning_content,omitempty"`
	// Refusal 模型拒绝作答的正文。与 Content 并列而非互斥：官方在拒绝时把
	// content 置 null、正文放这里，漏读会让拒绝变成一条空消息。
	Refusal string `json:"refusal,omitempty"`
	// Annotations 正文的来源标注（托管搜索开启时下发）。官方只有
	// type=url_citation 一种，索引口径是 content 内的字符下标。
	Annotations []annotation `json:"annotations,omitempty"`
	ToolCalls   []toolCall   `json:"tool_calls,omitempty"`
	ToolCallID  string       `json:"tool_call_id,omitempty"`
	Name        string       `json:"name,omitempty"`
	media       bool         // 出站内部标记：tool 结果抽出的图片块消息（不参与 JSON）
}

type part struct {
	Type       string      `json:"type"` // text / image_url / input_audio / file
	Text       string      `json:"text,omitempty"`
	ImageURL   *imageURL   `json:"image_url,omitempty"`
	InputAudio *inputAudio `json:"input_audio,omitempty"`
	File       *filePart   `json:"file,omitempty"`
}

// inputAudio Chat 的音频输入部分。只有 base64 一种形态，MIME 由 format 推出。
type inputAudio struct {
	Data   string `json:"data"`
	Format string `json:"format"` // "wav" / "mp3"
}

// filePart Chat 的文件输入部分。file_data 是 data URI，与 file_id 二选一。
type filePart struct {
	FileData string `json:"file_data,omitempty"`
	FileID   string `json:"file_id,omitempty"`
	Filename string `json:"filename,omitempty"`
}

type imageURL struct {
	URL string `json:"url"`
}

// annotation message.annotations 元素。官方把细节包在同名子对象里，
// 平铺形态（少数兼容上游）在解码时一并接受。
type annotation struct {
	Type        string       `json:"type"` // "url_citation"
	URLCitation *urlCitation `json:"url_citation,omitempty"`
}

type urlCitation struct {
	URL   string `json:"url"`
	Title string `json:"title,omitempty"`
	// StartIndex/EndIndex 是 content 内的字符下标（半开区间）。
	// 不可 omitempty：start_index=0 是合法值，去掉会让首字起始的引用丢失起点。
	StartIndex int    `json:"start_index"`
	EndIndex   int    `json:"end_index"`
	CitedText  string `json:"cited_text,omitempty"`
}

type toolCall struct {
	Index    int          `json:"index"` // 不可 omitempty：index=0 是合法值，严格客户端（Qoder）强校验该字段存在
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

// UnmarshalJSON 兼容 Cursor 扁平工具形态 {name, description, input_schema}
// （无 type/function 包装，KiroaaS converters_openai.py:261-302 同款）：
// 常规解析失败且能解出 name 时按扁平 DTO 解析，映射为 Type:"function"。
func (t *tool) UnmarshalJSON(data []byte) error {
	type plain tool
	var p plain
	if err := json.Unmarshal(data, &p); err == nil && p.Function.Name != "" {
		*t = tool(p)
		return nil
	}
	var flat struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		InputSchema json.RawMessage `json:"input_schema"`
		Parameters  json.RawMessage `json:"parameters"`
	}
	if err := json.Unmarshal(data, &flat); err != nil || flat.Name == "" {
		return fmt.Errorf("tool: neither standard {type,function} nor flat {name,...} form: %s", truncateJSON(data))
	}
	*t = tool{Type: "function", Function: toolFunc{
		Name:        flat.Name,
		Description: flat.Description,
		Parameters:  flat.InputSchema,
	}}
	return nil
}

// truncateJSON 截断原始 JSON 用于错误信息（与 codec 错误口径一致，防超长）。
func truncateJSON(data []byte) string {
	const max = 120
	if len(data) > max {
		data = data[:max]
	}
	return string(data)
}

type toolFunc struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	// Strict 严格 schema 校验开关（官方 FunctionDefinition.strict）。
	Strict *bool `json:"strict,omitempty"`
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
	// ServiceTier 实际服务档位回显（auto/default/flex/scale/priority/fast）。
	ServiceTier string `json:"service_tier,omitempty"`
}

type choice struct {
	Index        int      `json:"index"`
	Message      *message `json:"message,omitempty"` // 非流式
	Delta        *message `json:"delta,omitempty"`   // 流式
	FinishReason string   `json:"finish_reason,omitempty"`
}

type usage struct {
	PromptTokens            int                `json:"prompt_tokens"`
	CompletionTokens        int                `json:"completion_tokens"`
	TotalTokens             int                `json:"total_tokens"`
	PromptTokensDetails     *promptDetails     `json:"prompt_tokens_details,omitempty"`
	CompletionTokensDetails *completionDetails `json:"completion_tokens_details,omitempty"`
}

type promptDetails struct {
	CachedTokens int `json:"cached_tokens,omitempty"`
}

type completionDetails struct {
	ReasoningTokens int `json:"reasoning_tokens,omitempty"`
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
