// Package openairesponses 实现 OpenAI Responses 协议（/v1/responses）的 codec。
package openairesponses

import "encoding/json"

// Name 协议标识。
const Name = "openai-responses"

// ---- 请求 DTO ----

type request struct {
	Model string `json:"model"`
	// instructions 无 omitempty：订阅端点（Codex 形态）要求字段存在，
	// 无 system 时输出空串（官方 API 同样接受）。
	Instructions       string          `json:"instructions"`
	Input              json.RawMessage `json:"input,omitempty"` // string 或 []inputItem（官方两种形态均支持）
	MaxOutputTokens    int             `json:"max_output_tokens,omitempty"`
	Temperature        *float64        `json:"temperature,omitempty"`
	TopP               *float64        `json:"top_p,omitempty"`
	Stream             bool            `json:"stream,omitempty"`
	Store              *bool           `json:"store,omitempty"`
	Tools              []tool          `json:"tools,omitempty"`
	ToolChoice         any             `json:"tool_choice,omitempty"`
	ParallelToolCalls  *bool           `json:"parallel_tool_calls,omitempty"`
	Reasoning          *reasoning      `json:"reasoning,omitempty"`
	Include            []string        `json:"include,omitempty"`
	Text               *textConfig     `json:"text,omitempty"`
	Metadata           json.RawMessage `json:"metadata,omitempty"`
	PreviousResponseID string          `json:"previous_response_id,omitempty"`
}

type reasoning struct {
	Effort  string `json:"effort,omitempty"`
	Summary string `json:"summary,omitempty"`
}

// textConfig text.format 结构化输出：Responses 把 Chat 的 response_format
// 挪进了 text 下，并把 json_schema 的三个字段平铺（没有嵌套的 json_schema 层）。
type textConfig struct {
	Format *textFormat `json:"format,omitempty"`
}

type textFormat struct {
	Type   string          `json:"type"`
	Name   string          `json:"name,omitempty"`
	Strict *bool           `json:"strict,omitempty"`
	Schema json.RawMessage `json:"schema,omitempty"`
}

// inputItem 输入项：message / function_call / function_call_output / reasoning。
type inputItem struct {
	Type string `json:"type"`
	Role string `json:"role,omitempty"` // message
	// message content：[]contentPart
	Content json.RawMessage `json:"content,omitempty"`
	// function_call
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	// function_call_output
	Output string `json:"output,omitempty"`
	// reasoning
	ID               string          `json:"id,omitempty"`
	Summary          json.RawMessage `json:"summary,omitempty"` // []summaryPart
	EncryptedContent string          `json:"encrypted_content,omitempty"`
}

type contentPart struct {
	Type     string `json:"type"` // input_text / input_image / input_file / input_audio / output_text
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
	// input_file：三者取一。file_data 是 data URI，file_url 是远程地址。
	FileData string `json:"file_data,omitempty"`
	FileURL  string `json:"file_url,omitempty"`
	FileID   string `json:"file_id,omitempty"`
	Filename string `json:"filename,omitempty"`
	// InputAudio input_audio 的 {data, format}，与 Chat 同形。
	InputAudio *inputAudio `json:"input_audio,omitempty"`
	// Refusal type=refusal 的正文。官方用独立字段而非 text，故不能并入上面。
	Refusal string `json:"refusal,omitempty"`
}

type inputAudio struct {
	Data   string `json:"data"`
	Format string `json:"format"` // "wav" / "mp3"
}

type summaryPart struct {
	Type string `json:"type"` // "summary_text"
	Text string `json:"text"`
}

// tool Responses 的 function 工具是扁平结构（与 Chat Completions 不同）；
// 托管工具（web_search 等）只有 type，没有 name/parameters。
type tool struct {
	Type        string          `json:"type"` // "function" / "web_search" / "code_interpreter" ...
	Name        string          `json:"name,omitempty"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type toolChoiceNamed struct {
	Type string `json:"type"` // "function"
	Name string `json:"name"`
}

// ---- 响应 / 流式事件 DTO ----

// streamEvent 统一解析流式事件载荷，按 Type 分派。
type streamEvent struct {
	Type         string       `json:"type"`
	OutputIndex  int          `json:"output_index,omitempty"`
	ContentIndex int          `json:"content_index,omitempty"`
	SummaryIndex int          `json:"summary_index,omitempty"`
	Item         *inputItem   `json:"item,omitempty"`     // output_item.added / done
	Part         *contentPart `json:"part,omitempty"`     // content_part.added
	Delta        string       `json:"delta,omitempty"`    // *.delta
	Response     *responseObj `json:"response,omitempty"` // response.created / completed / incomplete / failed
}

type responseObj struct {
	ID                string             `json:"id"`
	Object            string             `json:"object,omitempty"`
	CreatedAt         int64              `json:"created_at,omitempty"`
	Model             string             `json:"model"`
	Status            string             `json:"status,omitempty"` // completed / incomplete / failed / in_progress
	Output            []inputItem        `json:"output,omitempty"`
	Usage             *usage             `json:"usage,omitempty"`
	Error             *errorBody         `json:"error,omitempty"`
	IncompleteDetails *incompleteDetails `json:"incomplete_details,omitempty"`
}

// incompleteDetails status=incomplete 时的具体原因：
// max_output_tokens（输出超长）或 content_filter（风控拦截）。
type incompleteDetails struct {
	Reason string `json:"reason"`
}

type usage struct {
	InputTokens        int `json:"input_tokens"`
	OutputTokens       int `json:"output_tokens"`
	TotalTokens        int `json:"total_tokens"`
	InputTokensDetails *struct {
		CachedTokens int `json:"cached_tokens,omitempty"`
	} `json:"input_tokens_details,omitempty"`
	OutputTokensDetails *struct {
		ReasoningTokens int `json:"reasoning_tokens,omitempty"`
	} `json:"output_tokens_details,omitempty"`
}

// errorResponse Responses 错误外形：{"error":{"code":...,"message":...}}。
type errorResponse struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code    string `json:"code,omitempty"`
	Type    string `json:"type,omitempty"`
	Message string `json:"message"`
}

func marshal(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
