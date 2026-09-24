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
	// Labels 计费/归因标签（gemini 独有）。收下只为诊断可见，不建模值。
	Labels json.RawMessage `json:"labels,omitempty"`
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
	// ExecutableCode / CodeExecutionResult 代码执行部件（模型生成的代码与其
	// 执行结果，历史回传场景出现）。没有任何 IR 块型接得住：降级成文本是
	// 编造正文，静默丢弃连「这里有过一段代码」都不留——收进不透明块，
	// 跨族出站跳过并报损耗。
	ExecutableCode      json.RawMessage `json:"executableCode,omitempty"`
	CodeExecutionResult json.RawMessage `json:"codeExecutionResult,omitempty"`
	// ToolCall / ToolResponse 服务端工具调用与结果（genai Part.toolCall /
	// Part.toolResponse）：客户端把 google_search / url_context / google_maps /
	// file_search / media_processing 这类服务端工具的历史回传上来。toolType 值域
	// 异构、args/response 是各工具专属的泛型 map，没有任何 IR 块型能无损接住——
	// 映成 server_tool_use 会编造出目标族不认的结构。此前 DTO 根本没有这两个字段，
	// 整段历史在 unmarshal 阶段静默蒸发，客户端的上下文被无声截断。收进不透明块
	// （From=gemini），跨族出站跳过并报损耗。
	ToolCall     json.RawMessage `json:"toolCall,omitempty"`
	ToolResponse json.RawMessage `json:"toolResponse,omitempty"`
	// VideoMetadata 视频截取元数据（startOffset/endOffset/fps，官方要求只在
	// 视频 inlineData/fileData 上出现）。RawMessage 延迟到 mediaBlock 决定。
	VideoMetadata json.RawMessage `json:"videoMetadata,omitempty"`
	// PartMetadata 客户端簿记元数据（map，可挂在任意部件型上）。
	PartMetadata json.RawMessage `json:"partMetadata,omitempty"`
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
	// ResponseJsonSchema responseSchema 的替代槽位：接受完整 JSON Schema
	// （responseSchema 只收 OpenAPI 3.0 子集）。官方文档标注两槽互斥。
	ResponseJsonSchema json.RawMessage `json:"responseJsonSchema,omitempty"`
	// ServiceTier 服务质量档位（unspecified/flex/standard/priority）。
	ServiceTier string `json:"serviceTier,omitempty"`
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
	// SpeechConfig 语音输出配置、MediaResolution 媒体分辨率档位：
	// gemini 独有，收下只为诊断可见，不建模值。
	SpeechConfig    json.RawMessage `json:"speechConfig,omitempty"`
	MediaResolution string          `json:"mediaResolution,omitempty"`
	// 以下六键同属「收下只为诊断可见」：imageConfig 图像生成约束（宽高比/
	// 尺寸/人物生成）、audioTimestamp 音频时间戳、enableEnhancedCivicAnswers
	// 增强公民问答、routingConfig/modelSelectionConfig 模型路由、
	// modelArmorConfig 提示词与响应安全审查模板。没有任何出站接得住。
	ImageConfig                json.RawMessage `json:"imageConfig,omitempty"`
	AudioTimestamp             json.RawMessage `json:"audioTimestamp,omitempty"`
	EnableEnhancedCivicAnswers json.RawMessage `json:"enableEnhancedCivicAnswers,omitempty"`
	RoutingConfig              json.RawMessage `json:"routingConfig,omitempty"`
	ModelSelectionConfig       json.RawMessage `json:"modelSelectionConfig,omitempty"`
	ModelArmorConfig           json.RawMessage `json:"modelArmorConfig,omitempty"`
	// AudioTranscriptionConfig 语音转写配置（官方
	// GenerateContentConfig.audioTranscriptionConfig）：输入音频的转写开关，
	// 没有任何出站接得住，同档收键名。
	AudioTranscriptionConfig json.RawMessage `json:"audioTranscriptionConfig,omitempty"`
}

type thinkingConfig struct {
	ThinkingBudget int `json:"thinkingBudget,omitempty"`
	// 指针：未给 / true / false 三态语义不同，只有显式 false 要动作。
	IncludeThoughts *bool `json:"includeThoughts,omitempty"`
	// ThinkingLevel Gemini 3 的思考档位（"low"/"high"），与 thinkingBudget
	// 是新旧两代表达，官方互斥。跨族按 effort 档位传递。
	ThinkingLevel string `json:"thinkingLevel,omitempty"`
}

type toolDef struct {
	FunctionDeclarations []functionDecl `json:"functionDeclarations,omitempty"`
	GoogleSearch         *googleSearch  `json:"googleSearch,omitempty"`
	CodeExecution        *struct{}      `json:"codeExecution,omitempty"`
	// GoogleSearchRetrieval 旧版托管搜索声明（gemini 1.5 时代形态），语义
	// 与 googleSearch 相同，归一到同一 canonical。dynamicRetrievalConfig
	// （动态检索阈值/模式）没有跨族槽位，收下键名让诊断报得出。
	GoogleSearchRetrieval *googleSearchRetrieval `json:"googleSearchRetrieval,omitempty"`
	// 以下托管工具声明没有任何跨族映射：收下只为让出站按「未识别托管
	// 工具」丢弃并报诊断，而不是解码即蒸发。
	URLContext          *struct{}         `json:"urlContext,omitempty"`
	FileSearch          *struct{}         `json:"fileSearch,omitempty"`
	GoogleMaps          *struct{}         `json:"googleMaps,omitempty"`
	ComputerUse         *struct{}         `json:"computerUse,omitempty"`
	EnterpriseWebSearch *struct{}         `json:"enterpriseWebSearch,omitempty"`
	ParallelAISearch    *struct{}         `json:"parallelAiSearch,omitempty"`
	MCPServers          []json.RawMessage `json:"mcpServers,omitempty"`
	// Retrieval / ExaAISearch 官方 Tool 的另外两种托管检索声明（genai
	// types.go Tool.Retrieval / Tool.ExaAISearch），同档收下。
	Retrieval   *struct{} `json:"retrieval,omitempty"`
	ExaAISearch *struct{} `json:"exaAiSearch,omitempty"`
}

// googleSearchRetrieval 旧版托管搜索声明的配置体。
type googleSearchRetrieval struct {
	DynamicRetrievalConfig json.RawMessage `json:"dynamicRetrievalConfig,omitempty"`
}

// googleSearch 托管搜索声明。官方（genai GoogleSearch）还有 searchTypes/
// blockingConfidence/timeRangeFilter 等键，均标注 Gemini API 不支持或是
// Vertex 专属，不建模；excludeDomains 与 IR 的域名黑名单同义，收进来。
type googleSearch struct {
	ExcludeDomains []string `json:"excludeDomains,omitempty"`
}

type functionDecl struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type toolConfig struct {
	FunctionCallingConfig *functionCallingConfig `json:"functionCallingConfig,omitempty"`
	// RetrievalConfig 检索工具的全局配置（官方 ToolConfig.retrievalConfig，
	// 地理位置等）：没有 IR 槽位，收键名让诊断可见。
	RetrievalConfig json.RawMessage `json:"retrievalConfig,omitempty"`
	// IncludeServerSideToolInvocations 让响应携带服务端工具调用过程（官方
	// ToolConfig.includeServerSideToolInvocations）：跨族无槽位，收键名。
	IncludeServerSideToolInvocations *bool `json:"includeServerSideToolInvocations,omitempty"`
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
