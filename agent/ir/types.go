// Package ir 定义协议无关的统一中间表示（canonical IR）。
// 所有协议 codec 的唯一职责是 协议格式 <-> IR 的互相转换；
// 跨协议转换一律经由 IR 中转，不存在协议两两直转。
package ir

import "encoding/json"

// Role 消息角色。tool 结果不作为独立角色，而是 user 消息中的 BlockToolResult 块
// （采用 Anthropic 模型作为超集）。
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// BlockType 内容块类型。
type BlockType string

const (
	BlockText  BlockType = "text"
	BlockImage BlockType = "image"
	// BlockMedia 非图片附件（PDF、音频、视频等）。与 BlockImage 分开是因为
	// 目标协议的承载槽位本来就是分开的：Anthropic 只有 document，OpenAI 两系
	// 是 file / input_file 与 input_audio，各槽位收的 MIME 集合互不相同。
	// 合到 BlockImage 会让音频被写进图片槽位，上游按图片解码后 400。
	BlockMedia BlockType = "media"
	// BlockRefusal 模型拒绝作答的正文。文本放 Text 字段。
	// 与 BlockText 分开是因为 OpenAI 两系有独立槽位（Chat 的 message.refusal、
	// Responses 的 refusal content part），而 Anthropic 与 kiro 没有——合进
	// BlockText 会让同协议往返把拒绝降级成普通回答，客户端无法区分「模型拒绝了」
	// 和「模型这么答的」。只靠 stop_reason=refusal 也不够：正文若丢，客户端看到
	// 的是一条空消息配一个拒绝标记，像成功的空回复。
	BlockRefusal             BlockType = "refusal"
	BlockToolUse             BlockType = "tool_use"
	BlockToolResult          BlockType = "tool_result"
	BlockThinking            BlockType = "thinking"
	BlockServerToolUse       BlockType = "server_tool_use"
	BlockWebSearchToolResult BlockType = "web_search_tool_result"
)

// Block 消息内容块。按 Type 取用对应字段，其余字段为零值。
type Block struct {
	Type                BlockType
	Text                string               // BlockText / BlockRefusal
	Image               *Image               // BlockImage
	Media               *Media               // BlockMedia
	ToolUse             *ToolUse             // BlockToolUse
	ToolResult          *ToolResult          // BlockToolResult
	Thinking            *Thinking            // BlockThinking
	ServerToolUse       *ServerToolUse       // BlockServerToolUse
	WebSearchToolResult *WebSearchToolResult // BlockWebSearchToolResult
	// Citations 本块正文引用的来源。挂在块上而非消息上，是因为三家协议都把它
	// 绑到单个文本块：Anthropic 的 text.citations、Chat 的 message.annotations、
	// Gemini 的 groundingSupports（按 part 定位）。偏移量也只有在单块正文内才
	// 有意义——跨块累加会在块被重排或降级时全部错位。
	Citations []Citation
	CacheCtl  string // 如 "ephemeral"，仅 Anthropic 方向保留
}

// Citation 正文中一段文字的来源标注。
//
// Start/End 是本块 Text 内的 rune 下标（半开区间），零值表示上游没给范围。
// 用 rune 而非 byte：Anthropic 与 OpenAI 的索引口径都是字符数，按字节算会让
// 中文引用整体错位。CitedText 是被引用的原文片段；两者互为冗余但都要保留，
// 因为各协议只给其中一种，缺的那种在编码时按另一种反推（参照 new-api
// claude_messages/citations.go 与 oai_chat/citations.go 的双向互推）。
type Citation struct {
	URL       string
	Title     string
	CitedText string
	Start     int
	End       int
	// EncryptedIndex Anthropic 托管搜索回传时用的不透明游标。跨协议无对应槽位，
	// 但同协议往返必须原样带回，否则上游拒绝续话。
	EncryptedIndex string
}

// HasRange 报告该引用是否带可用的正文范围。
func (c Citation) HasRange() bool { return c.End > c.Start }

// ServerToolUse 服务端托管工具调用（如网关代执行 web_search）。
// 外形同 ToolUse；结果以 ToolUseID 关联到 BlockWebSearchToolResult。
type ServerToolUse struct {
	ID    string
	Name  string
	Input json.RawMessage
}

// WebSearchToolResult web_search 服务端工具的结果块（Anthropic 形态）。
type WebSearchToolResult struct {
	ToolUseID string
	Results   []WebSearchResult
}

// WebSearchResult 单条搜索结果。Snippet 对应 Anthropic 的
// encrypted_content 字段（原文摘要，非加密）。
type WebSearchResult struct {
	Title   string
	URL     string
	Snippet string
}

// Image 图片内容。Data 为 base64 编码；URL 与 Data 二选一。
type Image struct {
	MediaType string // 如 "image/png"
	Data      string // base64
	URL       string
}

// MediaKind 非图片附件的大类。按大类而非按 MIME 全串分流，是因为目标协议的
// 槽位是按大类划分的（Anthropic document 只收文档，OpenAI input_audio 只收音频）。
type MediaKind string

const (
	MediaDocument MediaKind = "document" // application/pdf 等
	MediaAudio    MediaKind = "audio"    // audio/*
	MediaVideo    MediaKind = "video"    // video/*
	MediaOther    MediaKind = "other"    // 认不出大类，只能当不透明附件
)

// Media 非图片附件。Data 为 base64，与 URL、FileID 三者取一：
// 各协议表达同一份附件的方式不同（内联 base64 / 远程 URL / 上游文件 ID），
// 而它们之间不可互相换算——只能原样带过去，装不下时降级。
type Media struct {
	Kind      MediaKind
	MediaType string // 完整 MIME，如 "application/pdf"
	Data      string // base64
	URL       string
	FileID    string // 上游侧已上传文件的 ID（OpenAI file_id / Gemini fileUri 之外的形态）
	Filename  string
	// Format OpenAI input_audio 的 format 字段（"wav"/"mp3"）。它与 MediaType
	// 可互推，但 Chat 协议只认 format，故原样留存避免反复猜。
	Format string
}

// MediaKindOf 从 MIME 推大类。空 MIME 归 MediaOther 而不是猜测：
// 猜错会把附件投进错误的协议槽位，比认不出更糟。
func MediaKindOf(mime string) MediaKind {
	switch {
	case mime == "application/pdf" || hasPrefix(mime, "text/"):
		return MediaDocument
	case hasPrefix(mime, "audio/"):
		return MediaAudio
	case hasPrefix(mime, "video/"):
		return MediaVideo
	default:
		return MediaOther
	}
}

func hasPrefix(s, p string) bool { return len(s) >= len(p) && s[:len(p)] == p }

// Describe 返回给占位文本用的人类可读描述。目标协议装不下这一模态时，
// 参考 cc-switch 的 UNSUPPORTED_IMAGE_MARKER 做法换成占位文本块，而不是
// 静默丢弃：模型至少知道"这里本来有个附件"，不会把缺失当成用户没给。
func (m *Media) Describe() string {
	if m == nil {
		return "[attachment dropped: unsupported by upstream]"
	}
	desc := string(m.Kind)
	if m.MediaType != "" {
		desc = m.MediaType
	}
	if m.Filename != "" {
		desc += " " + m.Filename
	}
	return "[attachment dropped: " + desc + " — upstream protocol cannot carry it]"
}

// ToolUse 一次工具调用。Input 为 JSON 对象。
type ToolUse struct {
	ID    string
	Name  string
	Input json.RawMessage
}

// ToolResult 工具结果，通过 ToolUseID 关联调用。
type ToolResult struct {
	ToolUseID string
	Content   []Block // 通常为 text，可含 image
	IsError   bool
}

// Thinking 推理内容。Signature 为账号绑定的加密签名（Anthropic），
// 跨协议转换时按目标协议形态映射或丢弃。
// SignatureFrom 标记签名到达时的协议形态（如 "anthropic"、"gemini"）。
// 签名内容不透明、无法判别真实签发方，因此只以"形态族别"作保守判断：
// 与目标上游同族时透传（会话粘性下链完好），跨族时丢弃（宁可断链也不冒 400）。
type Thinking struct {
	Text          string
	Signature     string
	SignatureFrom string // 空表示无签名
}

// Message 一条对话消息。
type Message struct {
	Role    Role
	Content []Block
}

// SigFrom 签名非空时返回协议名作为 SignatureFrom，空签名为空串。
func SigFrom(protoName, sig string) string {
	if sig == "" {
		return ""
	}
	return protoName
}

// SigSynthetic 本代理自己造出来的占位签名的来源标记（kiro 的 fake reasoning
// 不带真签名，但 thinking 块下游需要一个非空值占位）。它与任何协议名都不相等，
// 因此永远过不了同族门控——占位签名既不会被回传给上游，也不会被写进客户端的
// 原生签名位冒充真签名（客户端拿它重放必被上游拒绝）。
const SigSynthetic = "synthetic"

// SignatureGenuineFor 报告该签名能否在 protoName 形态的通道上原样使用。
// 判据只有一条：来源形态与目标形态同族。合成签名与外族签名都为假。
func (t *Thinking) SignatureGenuineFor(protoName string) bool {
	return t != nil && t.Signature != "" && t.SignatureFrom == protoName
}

// Text 返回消息中所有 text 块拼接的纯文本，便于日志与测试断言。
func (m Message) Text() string {
	var out string
	for _, b := range m.Content {
		if b.Type == BlockText {
			out += b.Text
		}
	}
	return out
}

// Tool 工具定义。Hosted 为非空时表示服务端托管工具（如 "web_search"）。
type Tool struct {
	Name        string
	Description string
	InputSchema json.RawMessage
	Hosted      string
}

// ChoiceMode 工具选择模式。
type ChoiceMode string

const (
	ChoiceAuto ChoiceMode = "auto"
	ChoiceAny  ChoiceMode = "any"  // 必须调用某个工具（OpenAI required）
	ChoiceNone ChoiceMode = "none" // 禁止工具
	ChoiceTool ChoiceMode = "tool" // 指定工具
)

// ToolChoice 工具选择。
type ToolChoice struct {
	Mode            ChoiceMode
	ToolName        string // Mode == ChoiceTool 时有效
	DisableParallel bool
}

// ThinkingConfig 推理配置。Effort 为 OpenAI 风格的等级
// （minimal/low/medium/high/xhigh），BudgetTokens 为 Anthropic 风格预算，
// 两者可并存，codec 按目标协议取用。
type ThinkingConfig struct {
	Enabled      bool
	Effort       string
	BudgetTokens int

	// HideThoughts 客户端要求思考内容不回显。Gemini 的
	// thinkingConfig.includeThoughts=false 是唯一能表达这一点的入站形态；
	// 三元语义（未给 / true / false）里只有显式 false 需要动作，所以用正向
	// 的「隐藏」而不是「包含」，零值即「客户端没表态，照常回显」。
	HideThoughts bool
}

// ResponseFormat 结构化输出约束。两档语义：JSON 模式（只要求合法 JSON）与
// JSON Schema（要求符合给定 schema）。Schema 为空即前者。
//
// 各协议形态：Chat 的 response_format、Responses 的 text.format、
// Gemini 的 generationConfig.responseMimeType + responseSchema。
// Anthropic 与 kiro 原生没有这一维——官方做法是把 schema 塞进 system 提示或
// 声明一个单工具后强制调用，两者都是改写请求语义，不在本层做。
type ResponseFormat struct {
	// Name schema 名称（OpenAI json_schema.name），无对应形态的协议会丢掉。
	Name string
	// Schema JSON Schema 原文；为空表示只要求「输出合法 JSON」。
	Schema json.RawMessage
	// Strict OpenAI 的 json_schema.strict：要求严格符合 schema。
	// 只有 OpenAI 两系有这一维，Gemini 的 responseSchema 恒为严格语义。
	Strict bool
}

// IsSchema 是否带 schema（区别于只要求合法 JSON 的 JSON 模式）。
// 同时排掉 "null"：Clone 走 JSON 往返，空 RawMessage 会被序列化成 null 再读回
// 成 4 字节，只看长度会把「没给 schema」判成「给了」，进而写出 schema:null。
func (f *ResponseFormat) IsSchema() bool {
	return f != nil && len(f.Schema) > 0 && string(f.Schema) != "null"
}

// Request 统一请求模型。
type Request struct {
	Model         string
	Messages      []Message
	System        []Block // 顶层 system（text 块），Anthropic 形态
	Tools         []Tool
	ToolChoice    *ToolChoice
	MaxTokens     int
	Temperature   *float64
	TopP          *float64
	TopK          *int
	StopSequences []string
	Stream        bool
	Thinking      *ThinkingConfig
	// ResponseFormat 结构化输出约束（JSON 模式 / JSON Schema）。
	// nil = 客户端没要求，自由文本。
	ResponseFormat *ResponseFormat
	Metadata       map[string]string
	// Compact 显式压缩请求标记：openai-responses 入站 compact 路径或
	// input 含 compaction_trigger 条目。客户端自述压缩语义（如 Codex CLI），
	// 原模型失败时 Agent 可换 compress_model 兜底（第一档）。
	Compact bool

	// 以下调参维度一律用指针/空值表达「客户端没给」。不能用零值表达：
	// penalty 的 0 是「不惩罚」、seed 的 0 是一个具体种子、logprobs 的
	// false 是「明确不要」——都与「没提」不同，混起来就是替客户端表态。
	PresencePenalty  *float64
	FrequencyPenalty *float64
	Seed             *int
	// Candidates 候选数（Chat 的 n、Gemini 的 candidateCount）。
	Candidates *int
	// LogProbs 是否要对数概率；TopLogProbs 每个 token 返回几个候选。
	// 两维分开：Chat 有独立开关，Responses 只有 top_logprobs 兼任开关。
	LogProbs    *bool
	TopLogProbs *int
	// LogitBias token id -> 偏置。键是 token id，词表随模型而变，跨模型
	// 重映射没有正确答案，所以只在目标协议有这一维时原样透传，否则报丢弃。
	LogitBias map[string]float64

	// 以下两维只有 Gemini 一族有，没有任何出站协议接得住。收进 IR 只为
	// 让 Diagnose 看得见「客户端给了但装不下」，不作任何映射尝试。
	// SafetySettings 内容安全档位（类别+阈值）。
	SafetySettings []SafetySetting
	// CachedContent 服务端缓存名（context caching 的资源 id）。
	CachedContent string

	// 以下三维是 Responses 一族的服务端会话链语义。PreviousResponseID 与
	// Store 在同协议出站时原样回写（链确实能接上）；ItemRefs 恒为诊断
	// 载体：item_reference 指向服务端存着的条目，代理无状态解析不了，
	// 展开成 IR 后引用已失，即便回 responses 出站也无法复原。
	// PreviousResponseID 上一轮响应 id（链式增量请求的锚点）。
	PreviousResponseID string
	// Store 是否要求上游留存响应。三态：nil = 客户端没提。
	Store *bool
	// ItemRefs input 里 item_reference 条目的计数（内容拿不到，只记数）。
	ItemRefs int
}

// SafetySetting 一条内容安全档位（Gemini safetySettings 的等价物）。
type SafetySetting struct {
	Category  string
	Threshold string
}

// Clone 深拷贝请求，用于重试隔离（参考 new-api 的 DeepCopy 惯例）。
func (r *Request) Clone() *Request {
	b, err := json.Marshal(r)
	if err != nil {
		return r
	}
	var c Request
	if err := json.Unmarshal(b, &c); err != nil {
		return r
	}
	return &c
}

// Overrides 请求参数覆盖（账号/上游级）：转发前覆盖 IR 请求的对应字段，
// 客户端发了什么不重要。每个字段独立生效，nil = 透传客户端值。
// json 标签供账号存储与管理面 API；yaml 标签供 upstream 配置。
type Overrides struct {
	Thinking    *ThinkingOverride `json:"thinking,omitempty" yaml:"thinking,omitempty"`
	Temperature *float64          `json:"temperature,omitempty" yaml:"temperature,omitempty"`
	TopP        *float64          `json:"top_p,omitempty" yaml:"top_p,omitempty"`
	MaxTokens   *int              `json:"max_tokens,omitempty" yaml:"max_tokens,omitempty"`
}

// ThinkingOverride thinking 配置覆盖，强制语义：
// Enabled=true 强制开启（BudgetTokens<=0 时由目标 codec 兜底默认值）；
// Enabled=false 强制剥掉 thinking 参数（对上游不发送该字段）。
// Effort 为 OpenAI 风格等级，仅 openai/kiro/gemini 上游取用。
type ThinkingOverride struct {
	Enabled      bool   `json:"enabled" yaml:"enabled"`
	BudgetTokens int    `json:"budget_tokens,omitempty" yaml:"budget_tokens,omitempty"`
	Effort       string `json:"effort,omitempty" yaml:"effort,omitempty"`
}

// Apply 把覆盖应用到请求（原地修改，调用方负责先 Clone）。
func (o *Overrides) Apply(req *Request) {
	if o == nil {
		return
	}
	if o.Thinking != nil {
		req.Thinking = &ThinkingConfig{
			Enabled:      o.Thinking.Enabled,
			BudgetTokens: o.Thinking.BudgetTokens,
			Effort:       o.Thinking.Effort,
		}
	}
	if o.Temperature != nil {
		req.Temperature = o.Temperature
	}
	if o.TopP != nil {
		req.TopP = o.TopP
	}
	if o.MaxTokens != nil {
		req.MaxTokens = *o.MaxTokens
	}
}

// Configured 返回是否有任一字段被配置（全空则无需应用与记日志）。
func (o *Overrides) Configured() bool {
	return o != nil && (o.Thinking != nil || o.Temperature != nil || o.TopP != nil || o.MaxTokens != nil)
}
