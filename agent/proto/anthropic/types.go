// Package anthropic 实现 Anthropic Messages 协议（/v1/messages）的 codec。
// Anthropic 是 IR 事件词汇的来源协议，转换最接近恒等映射。
package anthropic

import "encoding/json"

// ---- 请求 DTO ----

type request struct {
	Model         string          `json:"model"`
	Messages      []message       `json:"messages"`
	System        json.RawMessage `json:"system,omitempty"` // string 或 []block
	MaxTokens     int             `json:"max_tokens"`
	Temperature   *float64        `json:"temperature,omitempty"`
	TopP          *float64        `json:"top_p,omitempty"`
	TopK          *int            `json:"top_k,omitempty"`
	StopSequences []string        `json:"stop_sequences,omitempty"`
	Stream        bool            `json:"stream,omitempty"`
	Tools         []tool          `json:"tools,omitempty"`
	ToolChoice    *toolChoice     `json:"tool_choice,omitempty"`
	Thinking      *thinkingCfg    `json:"thinking,omitempty"`
	Metadata      *metadata       `json:"metadata,omitempty"`
}

type metadata struct {
	UserID string `json:"user_id,omitempty"`
}

type thinkingCfg struct {
	Type         string `json:"type"` // "enabled" / "disabled"
	BudgetTokens int    `json:"budget_tokens,omitempty"`
}

type message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"` // string 或 []block
}

// block 是 Anthropic content block 的万能结构：
// 同一结构承载 text/image/tool_use/tool_result/thinking 与流式 delta；
// server_tool_use 复用 ID/Name/Input，web_search_tool_result 复用
// ToolUseID/Content（Content 为 web_search_result 子块数组）。
type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Source    *mediaSource    `json:"source,omitempty"` // image / document
	Title     string          `json:"title,omitempty"`  // document 文件名
	ID        string          `json:"id,omitempty"`     // tool_use / server_tool_use
	Name      string          `json:"name,omitempty"`   // tool_use / server_tool_use
	Input     json.RawMessage `json:"input,omitempty"`  // tool_use / server_tool_use
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"` // tool_result / web_search_tool_result
	IsError   bool            `json:"is_error,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	Signature string          `json:"signature,omitempty"`
	// Citations text 块的来源标注（托管搜索开启时下发）。
	Citations []citation    `json:"citations,omitempty"`
	CacheCtl  *cacheControl `json:"cache_control,omitempty"`
}

// citation text 块的 citations 元素（web_search_result_location 形态）。
// CitedText 与 [start, end) 索引都可能只给一半，另一半由 IR 侧反推。
type citation struct {
	Type           string `json:"type"`
	URL            string `json:"url"`
	Title          string `json:"title,omitempty"`
	CitedText      string `json:"cited_text,omitempty"`
	EncryptedIndex string `json:"encrypted_index,omitempty"`
	// 不可 omitempty：start_char_index=0 是合法值（引用从正文首字起），
	// 去掉会让客户端把起点当成缺省而落到错误位置。
	StartCharIndex int `json:"start_char_index"`
	EndCharIndex   int `json:"end_char_index"`
}

// webSearchResultBlock web_search_tool_result.content 的子块形态。
type webSearchResultBlock struct {
	Type             string `json:"type"` // "web_search_result"
	Title            string `json:"title"`
	URL              string `json:"url"`
	EncryptedContent string `json:"encrypted_content"` // 原文摘要（KiroaaS 语义）
}

// mediaSource image 与 document 共用的 source 外形。
// document 多出 file 与 text 两种 Type：前者引用已上传文件，后者直接内联纯文本。
type mediaSource struct {
	Type      string `json:"type"` // "base64" / "url" / "file" / "text"
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
	FileID    string `json:"file_id,omitempty"`
}

type cacheControl struct {
	Type string `json:"type"` // "ephemeral"
}

type tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
	Type        string          `json:"type,omitempty"` // 服务端托管工具，如 "web_search_20250305"
}

type toolChoice struct {
	Type                   string `json:"type"` // auto/any/none/tool
	Name                   string `json:"name,omitempty"`
	DisableParallelToolUse bool   `json:"disable_parallel_tool_use,omitempty"`
}

// ---- 流式事件 DTO ----

// streamEvent 统一解析所有 SSE 事件的 data 载荷，按 Type 分派。
type streamEvent struct {
	Type         string        `json:"type"`
	Index        int           `json:"index,omitempty"`
	Message      *eventMessage `json:"message,omitempty"`       // message_start
	ContentBlock *block        `json:"content_block,omitempty"` // content_block_start
	Delta        *delta        `json:"delta,omitempty"`         // content_block_delta / message_delta
	Usage        *usage        `json:"usage,omitempty"`         // message_delta
	Error        *errorBody    `json:"error,omitempty"`         // error
}

type eventMessage struct {
	ID    string `json:"id"`
	Model string `json:"model"`
	Usage *usage `json:"usage,omitempty"`
}

type delta struct {
	Type        string `json:"type"` // text_delta / input_json_delta / thinking_delta / signature_delta / (message_delta 时为空)
	Text        string `json:"text,omitempty"`
	PartialJSON string `json:"partial_json,omitempty"`
	Thinking    string `json:"thinking,omitempty"`
	Signature   string `json:"signature,omitempty"`
	StopReason  string `json:"stop_reason,omitempty"` // message_delta
	// StopSequence stop_reason 为 stop_sequence 时命中的那条序列原文。
	StopSequence string `json:"stop_sequence,omitempty"` // message_delta
	// Citation citations_delta 携带的单条引用。官方一帧一条，故不是数组。
	Citation *citation `json:"citation,omitempty"`
}

type usage struct {
	InputTokens              int `json:"input_tokens,omitempty"`
	OutputTokens             int `json:"output_tokens,omitempty"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
}

type errorBody struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// ---- 非流式响应 DTO ----

type response struct {
	ID           string  `json:"id"`
	Type         string  `json:"type"`
	Role         string  `json:"role"`
	Model        string  `json:"model"`
	Content      []block `json:"content"`
	StopReason   string  `json:"stop_reason"`
	StopSequence string  `json:"stop_sequence,omitempty"`
	Usage        usage   `json:"usage"`
}

// errorResponse 是 Anthropic 错误外形：{"type":"error","error":{...}}。
type errorResponse struct {
	Type  string    `json:"type"` // 恒 "error"
	Error errorBody `json:"error"`
}

func marshal(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
