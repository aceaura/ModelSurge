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
	// Responses 的 refusal content part），而 Anthropic 与 Gemini 没有——合进
	// BlockText 会让同协议往返把拒绝降级成普通回答，客户端无法区分「模型拒绝了」
	// 和「模型这么答的」。只靠 stop_reason=refusal 也不够：正文若丢，客户端看到
	// 的是一条空消息配一个拒绝标记，像成功的空回复。
	BlockRefusal    BlockType = "refusal"
	BlockToolUse    BlockType = "tool_use"
	BlockToolResult BlockType = "tool_result"
	BlockThinking   BlockType = "thinking"
	// BlockRedactedThinking 被安全系统涂抹掉的思考块（anthropic
	// redacted_thinking）。载荷只有一段不透明密文 Data，没有明文也没有签名。
	// 与 BlockThinking 分开是因为两者的回传契约相反：thinking 块要过签名校验，
	// 外族签名一律降级；redacted_thinking 没有签名可言，Anthropic 要求原样回传，
	// 任何改写都会让下一轮请求被拒。合进 BlockThinking 会让 degradeThinking 把
	// 它降级成空文本块，加密续话状态当场销毁。
	BlockRedactedThinking    BlockType = "redacted_thinking"
	BlockServerToolUse       BlockType = "server_tool_use"
	BlockWebSearchToolResult BlockType = "web_search_tool_result"
	// BlockContainerUpload 容器文件引用块（anthropic container_upload）：
	// 请求侧把已上传文件送进代码执行容器的输入目录，响应侧是模型运行
	// 代码后产出的文件引用。与 BlockMedia 分开是因为它没有内容本体——
	// 只有一个 file_id 和「进容器」的语义，塞进 Media.FileID 会让外族
	// 把它当普通附件投递，上游按内容解码后 400。
	BlockContainerUpload BlockType = "container_upload"
	// BlockOpaque 载荷无法用 IR 表达的块：原样保留 wire 判别值与整个块体。
	// 「未知块降级成文本」并不安全——Anthropic 有一整族服务端工具结果块
	// （web_fetch / code_execution / bash_code_execution /
	// text_editor_code_execution / tool_search）根本没有 text 字段，降级过去
	// 等于把抓取的网页正文、stdout、文件内容换成一个空文本块，而兄弟
	// server_tool_use 块还留在原地，发给上游的 tool_use/tool_result 配平
	// 当场断裂。原样透传让同族往返无损，外族整块丢弃并报损耗，两种结果都
	// 比伪造一个空文本块诚实。
	BlockOpaque BlockType = "opaque"
)

// Block 消息内容块。按 Type 取用对应字段，其余字段为零值。
type Block struct {
	Type       BlockType
	Text       string      // BlockText / BlockRefusal
	Image      *Image      // BlockImage
	Media      *Media      // BlockMedia
	ToolUse    *ToolUse    // BlockToolUse
	ToolResult *ToolResult // BlockToolResult
	Thinking   *Thinking   // BlockThinking
	// RedactedData redacted_thinking 块的不透明密文。原样收、原样发：它由
	// Anthropic 加密与解密，任何改写（含降级成文本）都会让下一轮请求被拒。
	// 密文可能很长且属会话内容，不进日志与诊断注记。
	RedactedData        string               // BlockRedactedThinking
	ServerToolUse       *ServerToolUse       // BlockServerToolUse
	WebSearchToolResult *WebSearchToolResult // BlockWebSearchToolResult
	ContainerUpload     *ContainerUploadRef  // BlockContainerUpload
	Opaque              *Opaque              // BlockOpaque
	// Citations 本块正文引用的来源。挂在块上而非消息上，是因为三家协议都把它
	// 绑到单个文本块：Anthropic 的 text.citations、Chat 的 message.annotations、
	// Gemini 的 groundingSupports（按 part 定位）。偏移量也只有在单块正文内才
	// 有意义——跨块累加会在块被重排或降级时全部错位。
	Citations []Citation
	CacheCtl  string // 如 "ephemeral"，仅 Anthropic 方向保留
	// CacheTTL 缓存断点的存活档位（"5m"/"1h"，空=官方默认 5m）。
	// 仅 Anthropic 方向保留；与 CacheCtl 并列而非合并进字符串，
	// 是因为 type 与 ttl 是 cache_control 对象里两个独立键。
	CacheTTL string
}

// ContextMgmtEntry 一条服务端上下文管理策略（responses context_management
// 数组元素）。Type 目前官方只有 "compaction"；CompactThreshold 是触发压缩的
// token 阈值，nil = 客户端没给（上游默认）。
type ContextMgmtEntry struct {
	Type             string
	CompactThreshold *int
}

// Container 代码执行容器的标识与技能声明（仅 Anthropic 一族）。
// 请求侧：ID 是跨请求复用的容器标识，Skills 是要加载的技能（Version 缺省
// 为 latest）；响应侧：ID/ExpiresAt 是实际使用的容器与过期时间，Skills 是
// 已加载技能（Version 必有值）。两侧共用一个类型，请求侧 ExpiresAt 恒空。
type Container struct {
	ID        string
	ExpiresAt string
	Skills    []Skill
}

// Skill 容器技能声明。Type 是 "anthropic"（内置）或 "custom"（用户自定义）。
type Skill struct {
	SkillID string
	Type    string
	Version string
}

// ContainerUploadRef 容器文件引用（container_upload 块的载荷）。
// 只有 file_id：文件本体在 Files API 侧，块只是指针。
type ContainerUploadRef struct {
	FileID string
}

// Opaque 不透明块的载荷：wire 判别值 + 上游给的完整块 JSON。
// Body 保留整块而非挑字段，是因为这些块的形状由上游定义且随版本增长
// （web_fetch_tool_result 有 caller，search_result 有 source/title/citations），
// 逐个建模永远慢一步，而原样带回是「同族往返无损」的唯一可靠做法。
// Body 属会话内容，不进日志与诊断注记。
type Opaque struct {
	WireType string
	Body     json.RawMessage
}

// AudioOutParam Chat 音频输出配置。Format 是 wav/aac/mp3/flac/opus/pcm16；
// Voice 是内置音色名或自定义音色 id（对象形态已归一）。
type AudioOutParam struct {
	Format string
	Voice  string
}

// AudioOutput Chat 非流式模型音频输出。ID 是下一轮 assistant 历史唯一允许
// 回传的引用；Data/ExpiresAt/Transcript 只属于本轮完整响应，不能塞回请求。
type AudioOutput struct {
	ID         string
	Data       string
	ExpiresAt  int64
	Transcript string
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
	// WireType 来源协议自报的引用种类（Anthropic 的 char_location /
	// page_location / content_block_location / search_result_location /
	// web_search_result_location）。空 = 来源协议的标注只有一种形态
	// （Chat/Responses 的 url_citation、Gemini 的 groundingChunk）。
	WireType string
	// Raw 引用的原始块体。同族往返一律原样带回：官方 union 五种形态的字段
	// 互不相同（文档类靠 document_index 与页号/块下标/file_id 定位，托管搜索
	// 靠 url + encrypted_index），逐字段重建必造出上游不认的形状。
	// 属会话内容，不进日志与诊断注记。
	//
	// omitempty 是承重的，不是省字节：Request.Clone() 走 JSON 往返，空
	// RawMessage 没有 omitempty 会被序列化成字面量 null 再读回成 4 字节，
	// 「原样带回」那条分支就会把 null 当成上游原文塞进 citations 数组，
	// 发给 anthropic 上游的是 "citations":[null]——整轮被拒，引用也没了。
	// 跨协议投影来的引用本来就没有 Raw，正是这条路径的常态。
	// 同源的坑见 ResponseFormat.IsSchema。
	Raw json.RawMessage `json:",omitempty"`
}

// HasRange 报告该引用是否带可用的正文范围。
func (c Citation) HasRange() bool { return c.End > c.Start }

// Portable 报告该引用能否落到外族协议的标注槽位上。Chat/Responses 的
// url_citation 与 Gemini 的 groundingChunk 都以 URL 作为来源身份；Anthropic 的
// 文档类引用只有 document_index 与页/块/字符下标，没有 URL，外族无从表达，
// 只能干净丢弃并报损耗（同族往返走 Raw，不受影响）。
func (c Citation) Portable() bool { return c.URL != "" }

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

// ToolKind 工具调用形态。零值等同 function，保持既有构造兼容。
type ToolKind string

const (
	ToolFunction ToolKind = ""
	ToolCustom   ToolKind = "custom"
)

// ToolUse 一次工具调用。function 使用 JSON 对象 Input；custom 使用自由文本 InputText。
// custom 同时保留 Input 的 {"input":...} 投影，供不支持 custom 的协议安全降级。
type ToolUse struct {
	ID        string
	Name      string
	Kind      ToolKind
	Input     json.RawMessage
	InputText string
}

// ObjectInput 返回对象槽位协议可承载的工具参数。
func (t *ToolUse) ObjectInput() json.RawMessage {
	if t == nil {
		return json.RawMessage(`{}`)
	}
	if t.Kind == ToolCustom {
		return json.RawMessage(marshalCustomInput(t.InputText))
	}
	out, _ := NormalizeToolInput(t.Input)
	return out
}

func marshalCustomInput(text string) []byte {
	b, _ := json.Marshal(struct {
		Input string `json:"input"`
	}{Input: text})
	return b
}

// ToolResult 工具结果，通过 ToolUseID 关联调用。
type ToolResult struct {
	ToolUseID string
	Kind      ToolKind
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
	// AudioID 是 Chat assistant 历史消息的音频引用。官方请求只接受
	// {audio:{id}}，完整音频数据不会在多轮上下文中重复回传。
	AudioID string
}

// SigFrom 签名非空时返回协议名作为 SignatureFrom，空签名为空串。
func SigFrom(protoName, sig string) string {
	if sig == "" {
		return ""
	}
	return protoName
}

// SigSynthetic 本代理自己造出来的占位签名的来源标记（Gemini 出站拿不到真
// thoughtSignature 时塞的占位值被客户端原样回传，不能被洗白成真签名）。
// 它与任何协议名都不相等，
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
	Kind        ToolKind
	// Format 是 Responses custom tool 的文本/grammar 格式对象；外族只能降级成
	// 一个必填 input 字符串参数的 function tool。
	Format json.RawMessage
	// CacheCtl/CacheTTL 工具定义上的缓存断点（Anthropic tools[].cache_control）。
	// 其余协议的工具定义没有这一维。
	CacheCtl string
	CacheTTL string
	// Strict 工具入参 schema 严格校验开关（anthropic tool.strict、OpenAI 两系
	// function.strict）。三态指针：nil=没给（上游默认），显式 false 是「明确
	// 不要严格校验」，与没给语义不同。
	Strict *bool
	// 以下四维是 anthropic 工具定义的 2026 修饰槽位，其余协议的工具定义
	// 一个都没有（跨族丢+报）：
	// DeferLoading 工具不进初始 system prompt，由 tool search 按需加载。
	DeferLoading bool
	// EagerInputStreaming 细粒度流式入参（null=按 beta 头默认，三态指针）。
	EagerInputStreaming *bool
	// InputExamples 入参示例（不透明对象数组，原文透传）。
	InputExamples []json.RawMessage
	// AllowedCallers 允许的程序化调用方（direct / code_execution_*）。
	AllowedCallers []string
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
	ToolKind        ToolKind
	DisableParallel bool
}

// ThinkingConfig 推理配置。Effort 为 OpenAI 风格的等级
// （minimal/low/medium/high/xhigh/max），BudgetTokens 为 Anthropic 风格预算，
// 两者可并存，codec 按目标协议取用。
type ThinkingConfig struct {
	Enabled      bool
	Effort       string
	BudgetTokens int

	// Adaptive 模型自主决定思考量（Anthropic thinking.type=adaptive，
	// 官方已标 enabled 废弃）。与 BudgetTokens 互斥：adaptive 不带预算。
	// 其余协议没有「自适应」这一档——OpenAI 的 effort 是显式档位。
	Adaptive bool
	// Display 思考内容回显形态（Anthropic thinking.display：
	// "summarized"=正常回显 / "omitted"=只回签名供多轮续接）。
	// 仅 Anthropic 有；与 HideThoughts 不同义——omitted 仍要回显签名。
	Display string

	// HideThoughts 客户端要求思考内容不回显。Gemini 的
	// thinkingConfig.includeThoughts=false 是唯一能表达这一点的入站形态；
	// 三元语义（未给 / true / false）里只有显式 false 需要动作，所以用正向
	// 的「隐藏」而不是「包含」，零值即「客户端没表态，照常回显」。
	HideThoughts bool

	// Summary 思考摘要的啰嗦程度（OpenAI Responses reasoning.summary：
	// auto/concise/detailed）。与 Display 不同轴：Display 管可见性，Summary 管
	// 摘要详略，两者不构成等价物。仅 responses 一族有槽位。
	Summary string
	// Context / Mode reasoning 的另两维（context: auto/current_turn/all_turns；
	// mode: standard/pro）。值形态仍在演进，按原文收下不解析（与 Conversation /
	// Moderation 同款约定），仅 responses 一族能回写。
	// omitempty 是必须的：Clone 走 JSON 往返，nil 不带标签会变成非空 "null"，
	// 出站据此判断「客户端给过」就会凭空写出一个 reasoning 对象。
	Context json.RawMessage `json:",omitempty"`
	Mode    json.RawMessage `json:",omitempty"`
}

// ResponseFormat 结构化输出约束。两档语义：JSON 模式（只要求合法 JSON）与
// JSON Schema（要求符合给定 schema）。Schema 为空即前者。
//
// 各协议形态：Chat 的 response_format、Responses 的 text.format、
// Gemini 的 generationConfig.responseMimeType + responseSchema、
// Anthropic 的 output_config.format（仅 json_schema 形态，2026 新增）。
// 装不下这一维的协议只能把 schema 塞进 system 提示或声明单工具后强制调用，
// 两者都是改写请求语义，不在本层做。
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
	// IncludeUsage 客户端显式要求流式末尾补一个只带 usage 的帧（Chat 的
	// stream_options.include_usage）。这里用两态而非像下面的调参维度那样用指针：
	// OpenAI 契约里「没提」与「显式 false」行为完全相同（都不发该帧），三态区分
	// 不出任何可观测差异。只有 Chat 一族有这个开关，Anthropic/Gemini/Responses
	// 的 usage 是协议内建、无条件回传。
	IncludeUsage bool
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

	// 以下四维只有 Responses 一族有。ConversationID 与 PreviousResponseID
	// 互斥（官方 API 同给会 400），是同族会话锚点的另一种形态，同协议出站
	// 回写；Include / Background / Prompt 是同族专属的请求修饰，其他三族
	// 没有任何对应物，收进 IR 只为同协议回写 + 跨协议诊断，不作映射尝试。
	// ConversationID 会话对象锚点（conversation 参数，string 或 {id} 形态）。
	ConversationID string
	// Background 后台运行模式。三态：nil = 客户端没提；显式 true 被丢时
	// 客户端期待的异步行为会变成同步等待，必须报。
	Background *bool
	// Include 客户端点名要回传的额外载荷（message.output_text.logprobs、
	// reasoning.encrypted_content、file_search_call.results 等）。
	Include []string
	// Prompt 服务端 prompt 模板引用。模板内容存在上游服务端，代理展开不了；
	// 丢了它上游只能看到裸消息（模板指令全丢）。
	Prompt *PromptRef
	// ContextMgmt 服务端上下文管理策略（responses 一族的 context_management，
	// 目前唯一条目 type 是 "compaction"：到 compact_threshold tokens 时服务端
	// 自动压缩上下文）。外族无对应，跨族由诊断报出。
	ContextMgmt []ContextMgmtEntry
	// ServiceTier 服务质量档位原值（anthropic auto/standard_only；OpenAI 两系
	// auto/default/flex/scale/priority/fast，responses 另有 ultrafast）。
	// 保留原值不规整：跨族映射在出站编码按目标协议值集进行（proto.MapServiceTier）。
	ServiceTier string
	// PromptCacheKey 提示缓存路由键（OpenAI 两系的 prompt_cache_key）。
	// 值可能是客户端自选串，诊断与日志一律不回显值本身。
	PromptCacheKey string

	// 以下两维只有 Anthropic 一族有，外族没有任何对应物。收进 IR 只为
	// 同协议回写 + 跨协议诊断，不作映射尝试（不同 SafetySettings 组并列）。
	// TopCacheCtl 顶层 cache_control 便捷糖的 type（如 "ephemeral"）。
	// 官方语义是「自动给最后一个可缓存块打缓存断点」；IR 不展开成块级——
	// 展开要猜「最后一个可缓存块」是哪一个（tools→system→messages 的查找
	// 顺序官方没钉死），猜错位置比不展开更糟。原样保留顶层形态。
	TopCacheCtl string
	// TopCacheTTL 顶层糖的存活档位（"5m"/"1h"，空=官方默认 5m）。
	TopCacheTTL string
	// InferenceGeo 推理地理偏好（inference_geo，如 "us"）。空=没给，
	// 上游按 workspace 的 default_inference_geo 处理；显式 null 与缺省
	// 在 JSON 层同义，解码后都是空。
	InferenceGeo string
	// Container 代码执行容器复用标识与技能声明（anthropic 的 container
	// 参数，string 简写与 {id,skills} 对象两形态统一成此结构；外族无对应，
	// 跨族由诊断报出）。nil = 客户端没提。
	Container *Container
	// Verbosity 输出啰嗦程度档位（low/medium/high）。Chat 是顶层 verbosity，
	// Responses 是 text.verbosity；其余协议没有输出长度转向这一维。
	Verbosity string
	// SafetyIdentifier 滥用检测标识（OpenAI 两系，user 字段的官方替代）。
	// 与 metadata.user_id 同一维度：跨族到 anthropic 时映进 metadata.user_id
	// 槽位（该槽被 user_id 占了才丢，丢要报）。
	SafetyIdentifier string
	// Moderation 请求级审核策略（OpenAI 两系 {model, policy{input/output}}）。
	// 不透明原文透传：代理不解释审核策略，只负责送达或报出。
	// json tag 只服务 Clone（marshal/unmarshal 往返）：缺它 nil 会被
	// 序列化成 "null"、回来变成非空，出站多一个 null 键。
	Moderation json.RawMessage `json:",omitempty"`
	// PromptCacheOptions 显式缓存断点控制（OpenAI 两系 {mode, ttl, ...}，
	// gpt-5.6+）。不透明原文透传。omitempty 同 Moderation。
	PromptCacheOptions json.RawMessage `json:",omitempty"`

	// 以下四维只有 Chat 一族有（responses 全系无对应槽位，SDK 核对零命中）。
	// 收进 IR 只为同协议回写 + 跨协议诊断，不作映射尝试。
	// Modalities 输出模态（"text"/"audio"）。
	Modalities []string
	// AudioOut 音频输出配置 {format, voice}。voice 官方两形态（内置名 string
	// 或自定义 {id} 对象），对象形态归一成 string（语义等价）；仅在
	// Modalities 含 "audio" 时有效，同给与否都透传让上游判定。
	AudioOut *AudioOutParam
	// Prediction 预测输出配置（重生成场景提速，{type:"content", content:
	// string|parts[]}）。嵌套内容数组，不透明原文透传（同 Moderation）。
	Prediction json.RawMessage `json:",omitempty"`
	// WebSearchOptions 联网搜索选项（{search_context_size, user_location}）。
	// 不透明原文透传。
	WebSearchOptions json.RawMessage `json:",omitempty"`
}

// PromptRef 服务端 prompt 模板引用（Responses 的 prompt 参数）。
// Variables 值可为字符串/图像/文件对象，按原文保留不解析。
type PromptRef struct {
	ID        string
	Version   string
	Variables json.RawMessage
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
// Effort 为 OpenAI 风格等级，仅 openai/gemini 上游取用。
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
