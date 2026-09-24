// Package openairesponses 实现 OpenAI Responses 协议（/v1/responses）的 codec。
package openairesponses

import "encoding/json"

// Name 协议标识。
const Name = "openai-responses"

// ---- 请求 DTO ----

type request struct {
	Model string `json:"model"`
	// Instructions 指针化：官方 API 缺省即无系统指令，伪造空串是把「没给」
	// 改写成「给了一条空指令」；仅订阅端点（Codex 形态）强制要求字段存在，
	// 那一侧无 system 时输出空串。
	Instructions      *string         `json:"instructions,omitempty"`
	Input             json.RawMessage `json:"input,omitempty"` // string 或 []inputItem（官方两种形态均支持）
	MaxOutputTokens   int             `json:"max_output_tokens,omitempty"`
	Temperature       *float64        `json:"temperature,omitempty"`
	TopP              *float64        `json:"top_p,omitempty"`
	Stream            bool            `json:"stream,omitempty"`
	Store             *bool           `json:"store,omitempty"`
	Tools             []tool          `json:"tools,omitempty"`
	ToolChoice        any             `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool           `json:"parallel_tool_calls,omitempty"`
	Reasoning         *reasoning      `json:"reasoning,omitempty"`
	Include           []string        `json:"include,omitempty"`
	Text              *textConfig     `json:"text,omitempty"`
	Metadata          json.RawMessage `json:"metadata,omitempty"`
	// User 终端用户标识（滥用追踪/计费归属）。与 anthropic 的 metadata.user_id
	// 同一维度，IR 里统一放 Metadata["user_id"]。
	User               string `json:"user,omitempty"`
	PreviousResponseID string `json:"previous_response_id,omitempty"`
	// Conversation 会话对象锚点（与 previous_response_id 互斥）。官方两种
	// 形态：字符串 id 或 {id} 对象，按原文收下，解码时归一成 id。
	Conversation json.RawMessage `json:"conversation,omitempty"`
	// Background 后台运行模式（长任务异步执行）。
	Background *bool `json:"background,omitempty"`
	// Prompt 服务端 prompt 模板引用 {id, version?, variables?}。
	Prompt *promptRef `json:"prompt,omitempty"`
	// ServiceTier 服务质量档位（chat 值集 + ultrafast）。
	ServiceTier string `json:"service_tier,omitempty"`
	// PromptCacheKey 提示缓存路由键。
	PromptCacheKey string `json:"prompt_cache_key,omitempty"`
	// SafetyIdentifier 滥用检测标识，user 字段的官方替代。与 user 同一维度。
	SafetyIdentifier string `json:"safety_identifier,omitempty"`
	// Moderation 请求级审核策略 {model, policy{input/output}}，原文透传。
	Moderation json.RawMessage `json:"moderation,omitempty"`
	// PromptCacheOptions 显式缓存断点控制 {mode, ttl, ...}，原文透传。
	PromptCacheOptions json.RawMessage `json:"prompt_cache_options,omitempty"`
	// TopLogProbs 兼任开关与档位：Responses 没有独立的 logprobs 布尔。
	TopLogProbs *int `json:"top_logprobs,omitempty"`
	// ContextManagement 服务端上下文管理策略（目前唯一条目 type 是
	// "compaction" + compact_threshold tokens 阈值）。
	ContextManagement []contextMgmtEntry `json:"context_management,omitempty"`
}

// contextMgmtEntry context_management 数组元素。CompactThreshold 三态：
// nil = 客户端没给（上游默认），与显式 0 不同。
type contextMgmtEntry struct {
	Type             string `json:"type"`
	CompactThreshold *int   `json:"compact_threshold,omitempty"`
}

// promptRef prompt 模板引用。Variables 值可为字符串/图像/文件对象，
// 按原文保留不解析（代理展开不了服务端模板，回写时必须字节保真）。
type promptRef struct {
	ID        string          `json:"id"`
	Version   string          `json:"version,omitempty"`
	Variables json.RawMessage `json:"variables,omitempty"`
}

type reasoning struct {
	Effort  string `json:"effort,omitempty"`
	Summary string `json:"summary,omitempty"`
	// Context 推理带多少会话上下文（auto/current_turn/all_turns）。
	// Mode 推理模式（standard/pro）。两者值形态仍在演进，按原文透传不解析。
	Context json.RawMessage `json:"context,omitempty"`
	Mode    json.RawMessage `json:"mode,omitempty"`
}

// textConfig text.format 结构化输出：Responses 把 Chat 的 response_format
// 挪进了 text 下，并把 json_schema 的三个字段平铺（没有嵌套的 json_schema 层）。
// verbosity 也挂在 text 下（Chat 里是顶层字段）。
type textConfig struct {
	Format    *textFormat `json:"format,omitempty"`
	Verbosity string      `json:"verbosity,omitempty"`
}

type textFormat struct {
	Type   string          `json:"type"`
	Name   string          `json:"name,omitempty"`
	Strict *bool           `json:"strict,omitempty"`
	Schema json.RawMessage `json:"schema,omitempty"`
}

// inputItem 输入项：message / function_call / custom_tool_call / 各自 output / reasoning。
type inputItem struct {
	Type string `json:"type"`
	Role string `json:"role,omitempty"` // message
	// message content：[]contentPart
	Content json.RawMessage `json:"content,omitempty"`
	// function_call / custom_tool_call
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	Input     string `json:"input,omitempty"`
	// function_call_output / custom_tool_call_output。官方允许两种形态：
	// 字符串，或 content part 数组（[{"type":"output_text",...},...]）。
	// 声明成 string 时数组形态会让 item 级 Unmarshal 报类型错误——请求侧
	// 整单 400（合法请求被硬拒），响应侧整条 item 静默蒸发（连 default 的
	// 不透明块兜底都进不去，computer_call_output 这类兄弟 item 同病）。
	// RawMessage 双形态兜底，逐 part 解析在 decodeItem 做。
	Output json.RawMessage `json:"output,omitempty"`
	// reasoning
	ID               string          `json:"id,omitempty"`
	Summary          json.RawMessage `json:"summary,omitempty"` // []summaryPart
	EncryptedContent string          `json:"encrypted_content,omitempty"`
	// web_search_call（托管搜索输出项）：status 与 action 只在这类 item 上出现。
	Status string           `json:"status,omitempty"`
	Action *webSearchAction `json:"action,omitempty"`
	// Raw 整块原样线体（同族未知托管 item 的回吐通道）。标 json:"-" 不参与
	// 逐字段序列化：MarshalJSON 优先整块吐出它（同 contentPart.Raw 的做法）。
	Raw json.RawMessage `json:"-"`
}

func (it inputItem) MarshalJSON() ([]byte, error) {
	if len(it.Raw) > 0 {
		return it.Raw, nil
	}
	type plain inputItem
	return json.Marshal(plain(it))
}

// webSearchAction web_search_call 的 action 子对象（官方 ResponseFunctionWebSearch）。
// search 带 queries（旧形态是单数 query）与 sources；open_page 带 url；
// find_in_page 带 url+pattern。三形态字段平铺，按 type 区分。
type webSearchAction struct {
	Type    string            `json:"type"`
	Query   string            `json:"query,omitempty"`
	Queries []string          `json:"queries,omitempty"`
	URL     string            `json:"url,omitempty"`
	Pattern string            `json:"pattern,omitempty"`
	Sources []webSearchSource `json:"sources,omitempty"`
}

// webSearchSource search 动作的来源条目。官方只有 {type:"url", url} 一形，
// 没有标题与摘要——跨族投到 anthropic 的结果块时那两个字段只能留空。
type webSearchSource struct {
	Type string `json:"type,omitempty"`
	URL  string `json:"url"`
}

type contentPart struct {
	Type     string    `json:"type"` // input_text / input_image / input_file / input_audio / output_text
	Text     string    `json:"text,omitempty"`
	ImageURL *imageRef `json:"image_url,omitempty"`
	// Detail input_image 的分辨率档位（low / high / auto）。官方把它放在 part
	// 顶层、与 image_url 平级，不是嵌在 image_url 里——Chat 形态才嵌。
	Detail string `json:"detail,omitempty"`
	// input_file：三者取一。file_data 是 data URI，file_url 是远程地址。
	// file_id 同时是 input_image 的第二种载体。
	FileData string `json:"file_data,omitempty"`
	FileURL  string `json:"file_url,omitempty"`
	FileID   string `json:"file_id,omitempty"`
	Filename string `json:"filename,omitempty"`
	// InputAudio input_audio 的 {data, format}，与 Chat 同形。
	InputAudio *inputAudio `json:"input_audio,omitempty"`
	// Refusal type=refusal 的正文。官方用独立字段而非 text，故不能并入上面。
	Refusal string `json:"refusal,omitempty"`
	// Annotations output_text 部分的来源标注（托管搜索开启时下发）。
	Annotations []annotation `json:"annotations,omitempty"`
	// Raw 本族未知 part 的原样线体。标 json:"-" 是为了不参与逐字段序列化：
	// MarshalJSON 优先整块吐出它（同 anthropic block.OpaqueRaw 的做法），
	// 逐字段重建会丢掉这个 part 没建模的键。
	Raw json.RawMessage `json:"-"`
}

func (p contentPart) MarshalJSON() ([]byte, error) {
	if len(p.Raw) > 0 {
		return p.Raw, nil
	}
	type plain contentPart
	return json.Marshal(plain(p))
}

// imageRef input_image 的图片载荷。
//
// 本族的规范形状是裸字符串（data URI 或远程 URL），但 Chat 形态的对象
// {"url":…,"detail":…} 也会到这里来：sub2api 的 responses 桥对这两路都做了
// 分支，实测客户端确实混发。此前这里声明成 string，遇到对象整个 part 的
// json.Unmarshal 失败被丢掉；若它是消息里唯一的部件，normalize 随后把整条
// 消息改写成 "(empty)"——图片连同所在消息一起消失，客户端拿不到任何注记。
// 出站一律写回裸字符串：本族上游只认这一种。
type imageRef struct {
	URL    string
	Detail string
}

func (r *imageRef) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		r.URL = s
		return nil
	}
	var o struct {
		URL    string `json:"url"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(b, &o); err != nil {
		return err
	}
	r.URL, r.Detail = o.URL, o.Detail
	return nil
}

func (r imageRef) MarshalJSON() ([]byte, error) { return json.Marshal(r.URL) }

// annotation output_text.annotations 元素。Responses 的形态是平铺的
// （不像 Chat 包在 url_citation 子对象里），索引口径同为字符下标。
type annotation struct {
	Type  string `json:"type"` // "url_citation"
	URL   string `json:"url"`
	Title string `json:"title,omitempty"`
	// 不可 omitempty：start_index=0 是合法值。
	StartIndex int `json:"start_index"`
	EndIndex   int `json:"end_index"`
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
	Type        string          `json:"type"` // "function" / "custom" / hosted types
	Name        string          `json:"name,omitempty"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Format      json.RawMessage `json:"format,omitempty"`
	// Strict 严格 schema 校验开关（官方 FunctionTool.strict）。
	Strict *bool `json:"strict,omitempty"`
	// 以下三维是 web_search / web_search_preview 托管工具的声明参数
	// （官方 WebSearchTool）。函数工具上这些键不存在。
	Filters           *webSearchFilters `json:"filters,omitempty"`
	UserLocation      json.RawMessage   `json:"user_location,omitempty"`
	SearchContextSize string            `json:"search_context_size,omitempty"`
}

// webSearchFilters web_search 工具的检索过滤器。官方目前只有 allowed_domains。
type webSearchFilters struct {
	AllowedDomains []string `json:"allowed_domains,omitempty"`
}

type toolChoiceNamed struct {
	Type string `json:"type"` // "function"
	Name string `json:"name"`
}

// ---- 响应 / 流式事件 DTO ----

// streamEvent 统一解析流式事件载荷，按 Type 分派。
// 三个索引是指针：它们在携带块的事件上是必填字段，而 0 是合法值，用 int +
// omitempty 会把 output_index:0 整个抹掉——官方 SDK 拿它去索引 response.output[]，
// 缺字段直接读成 undefined。不携带索引的事件（response.created / error）留 nil。
type streamEvent struct {
	Type         string          `json:"type"`
	OutputIndex  *int            `json:"output_index,omitempty"`
	ContentIndex *int            `json:"content_index,omitempty"`
	SummaryIndex *int            `json:"summary_index,omitempty"`
	Item         *inputItem      `json:"-"`                   // output_item.added / done（由 ItemRaw 解出，见 UnmarshalJSON）
	ItemRaw      json.RawMessage `json:"-"`                   // item 的原始线体：未知托管 item 归不透明块时要整块带回
	Part         *contentPart    `json:"part,omitempty"`      // content_part.added
	Delta        string          `json:"delta,omitempty"`     // *.delta
	Arguments    string          `json:"arguments,omitempty"` // function_call_arguments.done
	Input        string          `json:"input,omitempty"`     // custom_tool_call_input.done
	Response     *responseObj    `json:"response,omitempty"`  // response.created / completed / incomplete / failed
	// Error 裸 error 事件携带的错误体。官方 wire 把它放在顶层
	// （{"type":"error","error":{...}}），不是 response.error 下；此前只读后者，
	// 于是上游给的 type/code/message 三个字段全部丢失，风控拦截被当成可重试的
	// 上游错误，把账号池白烧一遍。Response.Error 仍作回落：response.failed 走
	// 那条路径，两仓（cc-switch / sub2api）也都做这个双层回落。
	Error *errorBody `json:"error,omitempty"`
	// Annotation response.output_text.annotation.added 携带的单条引用。
	// 该事件没有 delta 字段，正文与标注是两个独立事件。
	Annotation *annotation `json:"annotation,omitempty"`
	// Text / Refusal / Annotations 是 done 事件携带的完整终态值：
	// output_text.done 给 text+annotations，refusal.done 给 refusal，
	// reasoning_summary_text.done / reasoning_text.done 给 text。
	// 只发终态不发增量的上游全靠这三个字段，漏读就是整段正文静默丢失。
	Text        string       `json:"text,omitempty"`
	Refusal     string       `json:"refusal,omitempty"`
	Annotations []annotation `json:"annotations,omitempty"`
}

// MarshalJSON item 走 ItemRaw：编码侧只填 Item（结构体），这里统一落线；
// 解码侧捕获的原文在同族回吐时一字不差。
func (se streamEvent) MarshalJSON() ([]byte, error) {
	type plain streamEvent
	raw := se.ItemRaw
	if len(raw) == 0 && se.Item != nil {
		raw = marshal(se.Item)
	}
	return json.Marshal(struct {
		plain
		Item json.RawMessage `json:"item,omitempty"`
	}{plain: plain(se), Item: raw})
}

// UnmarshalJSON item 双形态收下：Item 供已知类型的逐字段分派，ItemRaw 供
// 未知托管 item 整块归不透明块（逐字段重建必丢没建模的键）。
func (se *streamEvent) UnmarshalJSON(data []byte) error {
	type plain streamEvent
	var sh struct {
		plain
		Item json.RawMessage `json:"item"`
	}
	if err := json.Unmarshal(data, &sh); err != nil {
		return err
	}
	*se = streamEvent(sh.plain)
	se.ItemRaw = sh.Item
	if len(sh.Item) > 0 {
		var it inputItem
		if err := json.Unmarshal(sh.Item, &it); err == nil {
			se.Item = &it
		}
	}
	return nil
}

type responseObj struct {
	ID        string `json:"id"`
	Object    string `json:"object,omitempty"`
	CreatedAt int64  `json:"created_at,omitempty"`
	Model     string `json:"model"`
	Status    string `json:"status,omitempty"` // completed / incomplete / failed / in_progress
	// Output 保持原始线体数组：解码侧要按 item 原文把未知托管 item 归不透明块，
	// 编码侧每条在落线前才 marshal（fullOutput / EncodeResponse）。
	Output            []json.RawMessage  `json:"output,omitempty"`
	Usage             *usage             `json:"usage,omitempty"`
	Error             *errorBody         `json:"error,omitempty"`
	IncompleteDetails *incompleteDetails `json:"incomplete_details,omitempty"`
	// ServiceTier 实际服务档位回显（chat 值集 + ultrafast）。
	ServiceTier string `json:"service_tier,omitempty"`
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
