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
	// OutputConfig 2026 新增的输出控制：format 是结构化输出槽位，
	// effort 是思考档位（R63 起双向贯通）。
	OutputConfig *outputConfig `json:"output_config,omitempty"`
	// ServiceTier 服务质量档位：auto / standard_only。
	ServiceTier string `json:"service_tier,omitempty"`
	// CacheControl 顶层缓存便捷糖：自动给最后一个可缓存块打断点。
	// 不展开成块级，原样进出（理由见 ir.Request.TopCacheCtl）。
	CacheControl *cacheControl `json:"cache_control,omitempty"`
	// InferenceGeo 推理地理偏好（如 "us"）；缺省按 workspace 默认。
	InferenceGeo string `json:"inference_geo,omitempty"`
	// Container 代码执行容器复用标识与技能声明。官方两形态：string 简写
	// （仅 id）或 {id, skills} 对象——RawMessage 延迟判断。
	Container json.RawMessage `json:"container,omitempty"`
}

// containerParams 请求侧 container 的对象形态（官方 ContainerParams）。
type containerParams struct {
	ID     string           `json:"id,omitempty"`
	Skills []containerSkill `json:"skills,omitempty"`
}

// container 响应侧容器回显（官方 Container：id/expires_at/skills 恒在，
// skills 可为 null）。请求侧技能 version 可缺省（=latest），响应侧必有值，
// 同形复用。
type container struct {
	ID        string           `json:"id"`
	ExpiresAt string           `json:"expires_at"`
	Skills    []containerSkill `json:"skills"`
}

type containerSkill struct {
	SkillID string `json:"skill_id"`
	Type    string `json:"type"` // "anthropic" / "custom"
	Version string `json:"version,omitempty"`
}

// outputConfig 输出控制。format 只定义了 json_schema 一种 type。
type outputConfig struct {
	Format *jsonOutputFormat `json:"format,omitempty"`
	// Effort 思考档位（low/medium/high/xhigh/max，官方 OutputConfig.effort，
	// 与 OpenAI reasoning_effort 值集的交集——没有 none/minimal）。
	Effort string `json:"effort,omitempty"`
}

type jsonOutputFormat struct {
	Type   string          `json:"type"` // "json_schema"
	Schema json.RawMessage `json:"schema,omitempty"`
}

type metadata struct {
	UserID string `json:"user_id,omitempty"`
}

type thinkingCfg struct {
	Type         string `json:"type"` // "enabled" / "disabled" / "adaptive"
	BudgetTokens int    `json:"budget_tokens,omitempty"`
	// Display 思考内容回显形态（"summarized"/"omitted"）。
	Display string `json:"display,omitempty"`
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
	// FileID container_upload 块的文件引用（type=container_upload 时唯一载荷）。
	FileID    string          `json:"file_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"` // tool_result / web_search_tool_result
	IsError   bool            `json:"is_error,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	Signature string          `json:"signature,omitempty"`
	// Data redacted_thinking 块的唯一载荷：被安全系统涂抹的思考内容密文。
	// 与 Signature 不是一回事，也不互相替代。
	Data string `json:"data,omitempty"`
	// Citations text 块的来源标注（托管搜索与文档引用都会下发）。用 RawMessage
	// 而不是 []citationIn：Anthropic 在 document / search_result 块上复用同一个
	// 键名承载 {"enabled":bool} 配置对象。声明成数组时那种块会让整条 content 的
	// json.Unmarshal 直接失败，同消息里的其他块（包括用户真正的问题）一起蒸发。
	Citations json.RawMessage `json:"citations,omitempty"`
	CacheCtl  *cacheControl   `json:"cache_control,omitempty"`
	// OpaqueRaw 不透明块的原样块体。标 json:"-" 是为了不参与逐字段序列化：
	// MarshalJSON 见到它就把整块原样吐出去。
	OpaqueRaw json.RawMessage `json:"-"`
}

// MarshalJSON 不透明块整块原样写出，其余按字段序列化。
// 逐字段重建会丢掉 block 没建模的键（web_fetch_tool_result 的 caller、
// search_result 的 citations 配置等），而这些块的回传契约要求原样带回。
func (b block) MarshalJSON() ([]byte, error) {
	if len(b.OpaqueRaw) > 0 {
		return b.OpaqueRaw, nil
	}
	type plain block
	return json.Marshal(plain(b))
}

// citationIn text 块 citations 数组元素的解码形状：官方 union 五种形态的字段并集。
// 只用来把可跨协议的字段投影进 IR；同族往返的保真靠 ir.Citation.Raw，不靠它。
type citationIn struct {
	Type           string `json:"type"`
	URL            string `json:"url"`
	Title          string `json:"title,omitempty"`
	CitedText      string `json:"cited_text,omitempty"`
	EncryptedIndex string `json:"encrypted_index,omitempty"`
	StartCharIndex int `json:"start_char_index"`
	EndCharIndex   int `json:"end_char_index"`
	// Source search_result_location 的来源 URL——该形态没有 url 键。
	Source string `json:"source,omitempty"`
	// DocumentTitle char/page/content_block 三种形态的文档标题——它们没有 title 键。
	DocumentTitle string `json:"document_title,omitempty"`
}

// citationOut 编码形状，严格照 web_search_result_location 的官方 schema：
// type / url / title / cited_text / encrypted_index。
//
// 刻意没有 start_char_index / end_char_index：那两个键属 char_location，
// 官方这一形态根本没有它们。按判别式校验的上游会把多出来的键当非法输入拒掉，
// 而客户端定位靠的是 cited_text，索引本来就用不上。
type citationOut struct {
	Type           string `json:"type"`
	URL            string `json:"url,omitempty"`
	Title          string `json:"title,omitempty"`
	CitedText      string `json:"cited_text"`
	EncryptedIndex string `json:"encrypted_index,omitempty"`
}

// webSearchResultBlock web_search_tool_result.content 的子块形态。
type webSearchResultBlock struct {
	Type             string `json:"type"` // "web_search_result"
	Title            string `json:"title"`
	URL              string `json:"url"`
	EncryptedContent string `json:"encrypted_content"` // 原文摘要（上游侧加密，原样透传）
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
	// TTL 缓存存活档位（"5m"/"1h"，缺省 5m）。官方 extended-cache-ttl
	// 能力，丢了会让 1h 断点静默降级成 5m——计费与命中率都变。
	TTL string `json:"ttl,omitempty"`
}

type tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
	Type        string          `json:"type,omitempty"` // 服务端托管工具，如 "web_search_20250305"
	// CacheCtl 工具定义上的缓存断点（官方 Tool.cache_control）。
	CacheCtl *cacheControl `json:"cache_control,omitempty"`
	// Strict 保证工具名与入参的 schema 校验（官方 Tool.strict）。
	Strict *bool `json:"strict,omitempty"`
	// DeferLoading 不进初始 system prompt，由 tool search 按需加载。
	DeferLoading bool `json:"defer_loading,omitempty"`
	// EagerInputStreaming 细粒度流式入参开关（null=按 beta 头默认）。
	EagerInputStreaming *bool `json:"eager_input_streaming,omitempty"`
	// InputExamples 入参示例，不透明对象数组原文透传。
	InputExamples []json.RawMessage `json:"input_examples,omitempty"`
	// AllowedCallers 允许的程序化调用方（direct / code_execution_*）。
	AllowedCallers []string `json:"allowed_callers,omitempty"`
}

type toolChoice struct {
	Type                   string `json:"type"` // auto/any/none/tool
	Name                   string `json:"name,omitempty"`
	DisableParallelToolUse bool   `json:"disable_parallel_tool_use,omitempty"`
}

// ---- 流式事件 DTO ----

// streamEvent 统一解析所有 SSE 事件的 data 载荷，按 Type 分派。
type streamEvent struct {
	Type    string        `json:"type"`
	Index   int           `json:"index,omitempty"`
	Message *eventMessage `json:"message,omitempty"` // message_start
	// ContentBlock 存 raw 而不是 *block：块体里出现 block 没建模的字段形态时，
	// 整个 SSE 事件的 json.Unmarshal 会失败并把流打断；逐块解析才能只降级那一个块。
	ContentBlock json.RawMessage `json:"content_block,omitempty"` // content_block_start
	Delta        *delta          `json:"delta,omitempty"`         // content_block_delta / message_delta
	Usage        *usage          `json:"usage,omitempty"`         // message_delta
	Error        *errorBody      `json:"error,omitempty"`         // error
}

type eventMessage struct {
	ID    string `json:"id"`
	Model string `json:"model"`
	Usage *usage `json:"usage,omitempty"`
	// ServiceTier 实际服务档位回显（standard/priority/batch）。
	ServiceTier string `json:"service_tier,omitempty"`
	// Container 代码执行容器回显（按需出场，缺键与 null 同义）。
	Container *container `json:"container,omitempty"`
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
	// Citation citations_delta 携带的单条引用原文。官方一帧一条，故不是数组。
	// 用 RawMessage 收：五种形态字段互不相同，逐字段建模会在解码这一步就把
	// 文档类引用的定位字段丢掉，编码回去只能凭空重建。
	Citation json.RawMessage `json:"citation,omitempty"`
	// Container message_delta 上晚到的容器回显（官方 Delta.container）。
	Container *container `json:"container,omitempty"`
}

type usage struct {
	InputTokens              int                 `json:"input_tokens,omitempty"`
	OutputTokens             int                 `json:"output_tokens,omitempty"`
	CacheReadInputTokens     int                 `json:"cache_read_input_tokens,omitempty"`
	CacheCreationInputTokens int                 `json:"cache_creation_input_tokens,omitempty"`
	CacheCreation            *cacheCreationUsage `json:"cache_creation,omitempty"`
}

type cacheCreationUsage struct {
	Ephemeral5mInputTokens int `json:"ephemeral_5m_input_tokens"`
	Ephemeral1hInputTokens int `json:"ephemeral_1h_input_tokens"`
}

type errorBody struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// ---- 非流式响应 DTO ----

type response struct {
	ID    string `json:"id"`
	Type  string `json:"type"`
	Role  string `json:"role"`
	Model string `json:"model"`
	// Content 与 message.Content 同形：解码时逐块拆 raw（一个块的字段冲突
	// 不能牵连整条消息），编码时由 encodeBlocks 的结果 marshal 回来。
	Content      json.RawMessage `json:"content"`
	StopReason   string          `json:"stop_reason"`
	StopSequence string          `json:"stop_sequence,omitempty"`
	Usage        usage           `json:"usage"`
	ServiceTier  string          `json:"service_tier,omitempty"`
	// Container 代码执行容器回显（按需出场，缺键与 null 同义）。
	Container *container `json:"container,omitempty"`
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
