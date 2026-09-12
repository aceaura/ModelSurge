// Package openairesponses 实现 OpenAI Responses 协议（/v1/responses）的 codec。
package openairesponses

import "encoding/json"

// Name 协议标识。
const Name = "openai-responses"

// ---- 请求 DTO ----

type request struct {
	Model              string          `json:"model"`
	Instructions       string          `json:"instructions,omitempty"`
	Input              json.RawMessage `json:"input,omitempty"` // []inputItem（string 形态不支持，网关恒用数组）
	MaxOutputTokens    int             `json:"max_output_tokens,omitempty"`
	Temperature        *float64        `json:"temperature,omitempty"`
	TopP               *float64        `json:"top_p,omitempty"`
	Stream             bool            `json:"stream,omitempty"`
	Store              *bool           `json:"store,omitempty"`
	Tools              []tool          `json:"tools,omitempty"`
	ToolChoice         any             `json:"tool_choice,omitempty"`
	Reasoning          *reasoning      `json:"reasoning,omitempty"`
	Include            []string        `json:"include,omitempty"`
	Metadata           json.RawMessage `json:"metadata,omitempty"`
	PreviousResponseID string          `json:"previous_response_id,omitempty"`
}

type reasoning struct {
	Effort  string `json:"effort,omitempty"`
	Summary string `json:"summary,omitempty"`
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
	Type     string `json:"type"` // input_text / input_image / output_text
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
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
	ID                string      `json:"id"`
	Object            string      `json:"object,omitempty"`
	CreatedAt         int64       `json:"created_at,omitempty"`
	Model             string      `json:"model"`
	Status            string      `json:"status,omitempty"` // completed / incomplete / failed / in_progress
	Output            []inputItem `json:"output,omitempty"`
	Usage             *usage      `json:"usage,omitempty"`
	Error             *errorBody  `json:"error,omitempty"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details,omitempty"`
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
