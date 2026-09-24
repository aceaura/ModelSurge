package gemini

import "encoding/json"

// Name 协议标识。
const Name = "gemini"

// ---- 请求 ----

type generateRequest struct {
	Contents          []content         `json:"contents"`
	SystemInstruction *content          `json:"systemInstruction,omitempty"`
	GenerationConfig  *generationConfig `json:"generationConfig,omitempty"`
	Tools             []toolDef         `json:"tools,omitempty"`
	ToolConfig        *toolConfig       `json:"toolConfig,omitempty"`
	// SafetySettings / CachedContent 是 Gemini 独有维度，其他协议都没有
	// 对应槽位。收进来只为诊断可见（「给了但装不下」），不作映射。
	SafetySettings []safetySetting `json:"safetySettings,omitempty"`
	CachedContent  string          `json:"cachedContent,omitempty"`
}

type safetySetting struct {
	Category  string `json:"category"`
	Threshold string `json:"threshold"`
}

type content struct {
	Role  string `json:"role,omitempty"`
	Parts []part `json:"parts"`
}

// part 万能内容部件：text / inlineData / fileData / functionCall / functionResponse。
// thought+thoughtSignature 是 part 级字段（Gemini 2.5/3 思考模型）。
type part struct {
	Text             string            `json:"text,omitempty"`
	Thought          bool              `json:"thought,omitempty"`
	ThoughtSignature string            `json:"thoughtSignature,omitempty"`
	InlineData       *blob             `json:"inlineData,omitempty"`
	FileData         *fileData         `json:"fileData,omitempty"`
	FunctionCall     *functionCall     `json:"functionCall,omitempty"`
	FunctionResponse *functionResponse `json:"functionResponse,omitempty"`
}

type blob struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"` // base64
}

type fileData struct {
	MimeType string `json:"mimeType"`
	FileURI  string `json:"fileUri"`
}

type functionCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args,omitempty"`
	ID   string          `json:"id,omitempty"`
}

type functionResponse struct {
	Name     string          `json:"name"`
	Response json.RawMessage `json:"response"`
	ID       string          `json:"id,omitempty"`
}

type generationConfig struct {
	Temperature     *float64        `json:"temperature,omitempty"`
	TopP            *float64        `json:"topP,omitempty"`
	TopK            *int            `json:"topK,omitempty"`
	MaxOutputTokens int             `json:"maxOutputTokens,omitempty"`
	StopSequences   []string        `json:"stopSequences,omitempty"`
	ThinkingConfig  *thinkingConfig `json:"thinkingConfig,omitempty"`
	// ResponseMimeType "application/json" 即要求结构化输出；
	// ResponseSchema 是可选的 schema 约束（Gemini 的 schema 恒为严格语义）。
	ResponseMimeType string          `json:"responseMimeType,omitempty"`
	ResponseSchema   json.RawMessage `json:"responseSchema,omitempty"`
	// ResponseModalities 输出模态（TEXT/AUDIO/IMAGE）。OpenAI 值集是它的真子集
	// （text/audio），IMAGE 没有任何出站接得住——收下只为跨族传递与诊断可见。
	ResponseModalities []string `json:"responseModalities,omitempty"`
	// 调参维度全用指针，理由同 IR：零值与「没给」语义不同。
	// ResponseLogprobs 是 Gemini 的开关，Logprobs 是档位（对应 top_logprobs）。
	PresencePenalty  *float64 `json:"presencePenalty,omitempty"`
	FrequencyPenalty *float64 `json:"frequencyPenalty,omitempty"`
	Seed             *int     `json:"seed,omitempty"`
	CandidateCount   *int     `json:"candidateCount,omitempty"`
	ResponseLogprobs *bool    `json:"responseLogprobs,omitempty"`
	Logprobs         *int     `json:"logprobs,omitempty"`
}

type thinkingConfig struct {
	ThinkingBudget int `json:"thinkingBudget,omitempty"`
	// 指针：未给 / true / false 三态语义不同，只有显式 false 要动作。
	IncludeThoughts *bool `json:"includeThoughts,omitempty"`
}

type toolDef struct {
	FunctionDeclarations []functionDecl `json:"functionDeclarations,omitempty"`
	GoogleSearch         *struct{}      `json:"googleSearch,omitempty"`
	CodeExecution        *struct{}      `json:"codeExecution,omitempty"`
}

type functionDecl struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type toolConfig struct {
	FunctionCallingConfig *functionCallingConfig `json:"functionCallingConfig,omitempty"`
}

type functionCallingConfig struct {
	Mode                 string   `json:"mode,omitempty"` // AUTO / ANY / NONE
	AllowedFunctionNames []string `json:"allowedFunctionNames,omitempty"`
}

// ---- 响应 ----

// generateResponse Gemini 客户端非流式响应体，也是流式响应的 chunk 外形。
type generateResponse struct {
	Candidates    []candidate    `json:"candidates,omitempty"`
	UsageMetadata *usageMetadata `json:"usageMetadata,omitempty"`
	ModelVersion  string         `json:"modelVersion,omitempty"`
	ResponseID    string         `json:"responseId,omitempty"`
}

type candidate struct {
	Content      *content `json:"content,omitempty"`
	FinishReason string   `json:"finishReason,omitempty"`
	// GroundingMetadata 托管搜索的来源与正文对应关系。Gemini 不把引用挂在
	// part 上，而是用 chunk 数组 + support 数组按字节区间指回 part 正文。
	GroundingMetadata *groundingMetadata `json:"groundingMetadata,omitempty"`
}

type groundingMetadata struct {
	GroundingChunks   []groundingChunk   `json:"groundingChunks,omitempty"`
	GroundingSupports []groundingSupport `json:"groundingSupports,omitempty"`
}

type groundingChunk struct {
	Web *groundingWeb `json:"web,omitempty"`
}

type groundingWeb struct {
	URI   string `json:"uri,omitempty"`
	Title string `json:"title,omitempty"`
}

type groundingSupport struct {
	Segment               groundingSegment `json:"segment"`
	GroundingChunkIndices []int            `json:"groundingChunkIndices,omitempty"`
}

// groundingSegment 区间口径是**字节**下标（Gemini 与 OpenAI/Anthropic 不同），
// 转换时必须按字节而非字符换算，否则中文正文的区间整体错位。
type groundingSegment struct {
	PartIndex  int    `json:"partIndex,omitempty"`
	StartIndex int    `json:"startIndex"`
	EndIndex   int    `json:"endIndex"`
	Text       string `json:"text,omitempty"`
}

type usageMetadata struct {
	PromptTokenCount        int `json:"promptTokenCount,omitempty"`
	CachedContentTokenCount int `json:"cachedContentTokenCount,omitempty"`
	CandidatesTokenCount    int `json:"candidatesTokenCount,omitempty"`
	// ThoughtsTokenCount 思考消耗，与 candidatesTokenCount 并列而非其子项
	// （Gemini 的 totalTokenCount 把两者都算进去了）。
	ThoughtsTokenCount int `json:"thoughtsTokenCount,omitempty"`
	TotalTokenCount    int `json:"totalTokenCount,omitempty"`
}

// ---- 错误 ----

type errorResponse struct {
	Error *geminiError `json:"error,omitempty"`
}

type geminiError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Status  string `json:"status"`
	// Details 规范错误类型与上游错误码的落点：三字段的错误体装不下它们，
	// 而 status 是从状态码反推的粗粒度值，无法区分过滤拒绝与上游抖动。
	Details []errorDetail `json:"details,omitempty"`
}

// errorDetail google.rpc.ErrorInfo 外形。
type errorDetail struct {
	Type     string            `json:"@type"`
	Reason   string            `json:"reason,omitempty"`
	Domain   string            `json:"domain,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

func marshal(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte(`{}`)
	}
	return b
}

// sseFrame Gemini 流只有 data: 行（无 event: 行）。
func sseFrame(data []byte) []byte {
	return []byte("data: " + string(data) + "\n\n")
}
