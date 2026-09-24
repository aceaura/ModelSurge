package openairesponses

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/aceaura/ModelSurge/agent/ir"
	"github.com/aceaura/ModelSurge/agent/normalize"
	"github.com/aceaura/ModelSurge/agent/proto"
)

func init() { proto.Register(codec{}) }

// codec openai-responses 出站。subscriptionCompat 仅 codex 别名置位：订阅端点
// 强制要求 instructions 字段存在（无 system 时也得给空串）且 store=false，
// 官方 Responses API 没有这两条要求——instructions 缺省即无系统指令，store
// 缺省是 true。对官方端点伪造这两个值会静默改语义：store 一翻成 false，响应
// 不再落库，previous_response_id 会话链与事后拉取一起断掉，而客户端根本没
// 提过这个字段。
type codec struct{ subscriptionCompat bool }

// New 返回 codec（便于测试直接构造）。
func New() proto.Codec { return codec{} }

func (codec) Name() string { return Name }

// Caps Responses：encrypted_content 签名、图片、hosted tools 均支持；
// 思考模式下强制 tool_choice 亦支持。
func (codec) Caps() proto.Capabilities {
	return proto.Capabilities{
		ThinkingSignature: true, Images: true, HostedTools: true, ThinkingForcedToolChoice: true,
		// TopK 留假：Responses 协议原生没有这一维。
		ImageURLs: true, Sampling: true, TopK: false, ParallelToolCalls: true,
		// input_image 的 detail 与 file_id 都是本协议原生的一维（codex 同形共用）。
		ImageDetail: true, ImageFileRef: true,
		// 调参维度只有对数概率一项，且没有独立开关：top_logprobs 兼任。
		// penalties / seed / n / logit_bias 在这一族的请求体里不存在。
		LogProbs: true, LogProbsViaTopN: true,
		// input_file 是不透明容器，input_audio 是音频专属槽位；无视频入口。
		Documents: true, Audio: true, Video: false,
		Refusal: true, // type=refusal content part
		// function_call_output 里没有失败标志位。
		ToolResultError:  false,
		StructuredOutput: true, // text.format
		Citations:        true, // output_text.annotations
		UserID:           true, // user
		// previous_response_id + store。
		ResponseChain: true,
		// include / background / prompt 模板 / conversation。
		ResponsesExtras: true,
		// service_tier（值集含 ultrafast）与 prompt_cache_key。
		ServiceTier: true, PromptCacheKey: true,
		OpenAIExtras: true,
		ToolStrict:   true,
	}
}

// ---- 请求解码：Responses -> IR ----

func (codec) DecodeRequest(body []byte) (*ir.Request, error) {
	var req request
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("openai-responses: decode request: %w", err)
	}
	out := &ir.Request{
		Model:       req.Model,
		MaxTokens:   req.MaxOutputTokens,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		Stream:      req.Stream,
		TopLogProbs: req.TopLogProbs,
	}
	// top_logprobs 在这一族兼任开关：给了档位就等于要对数概率。
	// 不顺手置上 LogProbs，转去 Chat 时只带档位不带开关，上游什么都不会算。
	if req.TopLogProbs != nil {
		yes := true
		out.LogProbs = &yes
	}
	if req.Instructions != nil && *req.Instructions != "" {
		out.System = append(out.System, ir.Block{Type: ir.BlockText, Text: *req.Instructions})
	}
	var items []inputItem
	if len(req.Input) > 0 {
		var s string
		if err := json.Unmarshal(req.Input, &s); err == nil {
			// 官方 API 允许 input 为纯字符串（最简形态），等价单条 user message
			if strings.TrimSpace(s) != "" {
				items = append(items, inputItem{Type: "message", Role: "user", Content: json.RawMessage(marshal(s))})
			}
		} else if err := json.Unmarshal(req.Input, &items); err != nil {
			return nil, fmt.Errorf("openai-responses: decode input items: %w", err)
		}
	}
	for _, it := range items {
		decodeItem(out, it)
	}
	for _, t := range req.Tools {
		switch t.Type {
		case "", "function":
			out.Tools = append(out.Tools, ir.Tool{Name: t.Name, Description: t.Description, InputSchema: t.Parameters, Strict: t.Strict})
		case "custom":
			out.Tools = append(out.Tools, ir.Tool{
				Name: t.Name, Description: t.Description, Kind: ir.ToolCustom,
				InputSchema: customToolInputSchema(), Format: t.Format,
			})
		default:
			out.Tools = append(out.Tools, ir.Tool{Hosted: ir.CanonicalHosted(t.Type)})
		}
	}
	out.ToolChoice = decodeToolChoice(req.ToolChoice)
	// 同 Chat：parallel_tool_calls=false 是「禁止并行」。没给则不表态。
	if req.ParallelToolCalls != nil && !*req.ParallelToolCalls {
		if out.ToolChoice == nil {
			out.ToolChoice = &ir.ToolChoice{Mode: ir.ChoiceAuto}
		}
		out.ToolChoice.DisableParallel = true
	}
	// reasoning 四个子参数逐轴收下。此前只在 effort 非空时才建 Thinking，客户端
	// 单给 {"summary":"detailed"} 会让整个对象连 summary 一起消失。
	// Enabled 只由 effort 决定：summary/context/mode 都不是「要不要思考」的表态。
	// minimal 算开思考——它是「最少的思考」，不是「不思考」；chat 入站同款值就是
	// 这么读的，两族口径必须一致，否则同一个 vendor 值换个入口就变成相反语义
	// （出站按 Enabled 写 effort，读成关就成了 "none"）。
	if req.Reasoning != nil {
		out.Thinking = &ir.ThinkingConfig{
			Enabled: req.Reasoning.Effort != "" && req.Reasoning.Effort != "none",
			Effort:  req.Reasoning.Effort,
			Summary: req.Reasoning.Summary,
		}
		// 显式 null 等同没给（与 Moderation 同款归一）。
		if string(req.Reasoning.Context) != "null" {
			out.Thinking.Context = req.Reasoning.Context
		}
		if string(req.Reasoning.Mode) != "null" {
			out.Thinking.Mode = req.Reasoning.Mode
		}
	}
	if req.Text != nil {
		out.ResponseFormat = decodeResponseFormat(req.Text.Format)
		out.Verbosity = req.Text.Verbosity
	}
	// metadata 是官方文档维度（16 对键值，随响应回显）：整条丢掉等于客户端
	// 的关联数据再也回不来，里面的 user_id 也跟着蒸发。user 字段与
	// metadata.user_id 同维度，同给时 user 胜出（顶层字段比嵌套键更显式）。
	if len(req.Metadata) > 0 {
		out.Metadata = proto.DecodeStringMap(req.Metadata)
	}
	if req.User != "" {
		if out.Metadata == nil {
			out.Metadata = map[string]string{}
		}
		out.Metadata["user_id"] = req.User
	}
	out.PreviousResponseID = req.PreviousResponseID
	// store 三态透传：客户端显式给了就记住，没给保持 nil（出站再决定兜底值）。
	out.Store = req.Store
	// 会话对象锚点：string 与 {id} 两种形态归一成 id；与 PreviousResponseID
	// 互斥是官方约束，同给时两边都留着，让上游照实 400（只报不拒）。
	out.ConversationID = decodeConversation(req.Conversation)
	out.Background = req.Background
	out.Include = req.Include
	out.ServiceTier = req.ServiceTier
	out.PromptCacheKey = req.PromptCacheKey
	out.SafetyIdentifier = req.SafetyIdentifier
	// context_management 原值进 IR（type/threshold 都是结构化字段，无需
	// RawMessage 透传；threshold 三态指针保留「没给」）。
	for _, e := range req.ContextManagement {
		out.ContextMgmt = append(out.ContextMgmt, ir.ContextMgmtEntry{
			Type: e.Type, CompactThreshold: e.CompactThreshold})
	}
	// 显式 null 等同没给（与 chat 同款归一）。
	if string(req.Moderation) != "null" {
		out.Moderation = req.Moderation
	}
	if string(req.PromptCacheOptions) != "null" {
		out.PromptCacheOptions = req.PromptCacheOptions
	}
	if req.Prompt != nil {
		out.Prompt = &ir.PromptRef{ID: req.Prompt.ID, Version: req.Prompt.Version, Variables: req.Prompt.Variables}
	}
	return out, nil
}

// decodeConversation conversation 参数归一：字符串 id 或 {id} 对象。
// 其余形态（官方 spec 之外）解不出 id，按没给处理——会话锚点瞎猜一个
// 比丢了对客户端伤害更大（上游会把请求挂到错误的会话上）。
func decodeConversation(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var obj struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil {
		return obj.ID
	}
	return ""
}

// decodeResponseFormat text.format -> IR。type:"text" 是默认值，
// 等同于「没提要求」，不进 IR。
func customToolInputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"input":{"type":"string"}},"required":["input"],"additionalProperties":false}`)
}

func decodeResponseFormat(f *textFormat) *ir.ResponseFormat {
	if f == nil || f.Type == "" || f.Type == "text" {
		return nil
	}
	return &ir.ResponseFormat{
		Name:   f.Name,
		Schema: f.Schema,
		Strict: f.Strict != nil && *f.Strict,
	}
}

// encodeResponseFormat IR -> text.format。无 schema 时退回 json_object。
func encodeResponseFormat(f *ir.ResponseFormat) *textConfig {
	if f == nil {
		return nil
	}
	if !f.IsSchema() {
		return &textConfig{Format: &textFormat{Type: "json_object"}}
	}
	out := &textFormat{Type: "json_schema", Name: f.Name, Schema: f.Schema}
	if out.Name == "" {
		out.Name = "response" // name 是 json_schema 的必填字段
	}
	if f.Strict {
		strict := true
		out.Strict = &strict
	}
	return &textConfig{Format: out}
}

func decodeItem(req *ir.Request, it inputItem) {
	if it.Type == "" && it.Role != "" {
		it.Type = "message" // 官方 API 允许 message item 省略 type
	}
	switch it.Type {
	case "message":
		switch it.Role {
		case "system", "developer":
			req.System = append(req.System, decodeParts(it.Content)...)
		case "assistant":
			req.Messages = append(req.Messages, ir.Message{Role: ir.RoleAssistant, Content: decodeParts(it.Content)})
		default:
			req.Messages = append(req.Messages, ir.Message{Role: ir.RoleUser, Content: decodeParts(it.Content)})
		}
	case "function_call":
		// function_call 属于 assistant 消息：并入上一条 assistant 或新建
		b := ir.Block{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: it.CallID, Name: it.Name, Input: json.RawMessage(it.Arguments)}}
		appendAssistantBlock(req, b)
	case "custom_tool_call":
		t := &ir.ToolUse{ID: it.CallID, Name: it.Name, Kind: ir.ToolCustom, InputText: it.Input}
		t.Input = t.ObjectInput()
		appendAssistantBlock(req, ir.Block{Type: ir.BlockToolUse, ToolUse: t})
	case "function_call_output", "custom_tool_call_output":
		kind := ir.ToolFunction
		if it.Type == "custom_tool_call_output" {
			kind = ir.ToolCustom
		}
		req.Messages = append(req.Messages, ir.Message{Role: ir.RoleUser, Content: []ir.Block{{
			Type: ir.BlockToolResult,
			ToolResult: &ir.ToolResult{ToolUseID: it.CallID, Kind: kind,
				Content: []ir.Block{{Type: ir.BlockText, Text: it.Output}}},
		}}})
	case "reasoning":
		th := &ir.Thinking{Signature: it.EncryptedContent, SignatureFrom: ir.SigFrom(Name, it.EncryptedContent)}
		th.Text = decodeSummary(it.Summary)
		if th.Text == "" && th.Signature == "" {
			return
		}
		appendAssistantBlock(req, ir.Block{Type: ir.BlockThinking, Thinking: th})
	case "item_reference":
		// 引用的是上游存着的条目，代理无状态解析不了。记数让 Diagnose
		// 报出「有内容没进来」，静默丢弃会让上游看到残缺的上下文。
		req.ItemRefs++
	case "compaction_trigger":
		// Codex CLI 显式压缩请求标记：不进 IR 消息流（无内容可转），
		// 仅置 Compact 供 relay 压缩回退识别。其余字段透传语义由
		// compact 路径承担。
		req.Compact = true
	}
}

// appendAssistantBlock 把块并入最后一条 assistant 消息（不存在则新建）。
func appendAssistantBlock(req *ir.Request, b ir.Block) {
	if n := len(req.Messages); n > 0 && req.Messages[n-1].Role == ir.RoleAssistant {
		req.Messages[n-1].Content = append(req.Messages[n-1].Content, b)
		return
	}
	req.Messages = append(req.Messages, ir.Message{Role: ir.RoleAssistant, Content: []ir.Block{b}})
}

// decodeParts 解析 message content（string 或 []part）为 IR 块。
// 数组逐元素解析：一次性解 []contentPart 时，任一元素的形状冲突都会让整条
// content 的 Unmarshal 失败，同消息里用户真正在问的那句话跟着一起蒸发。
func decodeParts(raw json.RawMessage) []ir.Block {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s == "" {
			return nil
		}
		return []ir.Block{{Type: ir.BlockText, Text: s}}
	}
	var raws []json.RawMessage
	if err := json.Unmarshal(raw, &raws); err != nil {
		return nil
	}
	out := make([]ir.Block, 0, len(raws))
	for _, rp := range raws {
		var p contentPart
		if err := json.Unmarshal(rp, &p); err != nil {
			continue // 单个元素形状冲突：只丢它自己，兄弟块照常解出
		}
		switch p.Type {
		case "input_text", "output_text", "text":
			out = append(out, ir.Block{Type: ir.BlockText, Text: p.Text, Citations: decodeAnnotations(p.Annotations)})
		case "refusal":
			// 拒绝正文是可见内容，不是元数据。漏读会让拒绝响应变成一条空消息，
			// 客户端看到 200 + 空 content 会误判成成功（cc-switch handlers.rs
			// 同款结论）。
			out = append(out, ir.Block{Type: ir.BlockRefusal, Text: p.Refusal})
		case "input_image":
			var url, nested string
			if p.ImageURL != nil {
				url, nested = p.ImageURL.URL, p.ImageURL.Detail
			}
			img := parseImageURL(url)
			img.FileID = p.FileID
			// detail 的规范位置是 part 顶层；Chat 形态把它嵌在 image_url 对象里。
			// 两处都给了以顶层为准——那是本族自己的键位。
			img.Detail = p.Detail
			if img.Detail == "" {
				img.Detail = nested
			}
			out = append(out, ir.Block{Type: ir.BlockImage, Image: img})
		case "input_file":
			out = append(out, ir.Block{Type: ir.BlockMedia, Media: decodeInputFile(p)})
		case "input_audio":
			if p.InputAudio != nil {
				out = append(out, ir.Block{Type: ir.BlockMedia, Media: &ir.Media{
					Kind: ir.MediaAudio, MediaType: "audio/" + p.InputAudio.Format,
					Data: p.InputAudio.Data, Format: p.InputAudio.Format,
				}})
			}
		case "":
			// 连 type 都读不出来的元素不是 content part：留着会在出站时变成
			// {"type":""} 让上游 400（同 anthropic decodeRawBlock 的口径）。
		default:
			// 未知 part（input_video、厂商私有的输入形态）原样留成不透明块。
			// 静默丢掉会让模型以为用户没给这段输入，客户端还拿不到任何注记可循；
			// 逐字段猜则必丢内容。同族原样带回无损，外族整块跳过并报损耗。
			out = append(out, ir.Block{Type: ir.BlockOpaque,
				Opaque: &ir.Opaque{WireType: p.Type, Body: rp, From: Name}})
		}
	}
	return out
}

// decodeInputFile 解 input_file。只给 file_id 时 MIME 不可知，归 MediaOther
// 而不是猜 PDF——猜错会让附件投进目标协议的文档槽位被按 PDF 解析。
func decodeInputFile(p contentPart) *ir.Media {
	m := &ir.Media{Filename: p.Filename, FileID: p.FileID, URL: p.FileURL}
	if p.FileData != "" {
		f := parseImageURL(p.FileData) // data URI 拆解逻辑与图片一致
		m.MediaType = f.MediaType
		m.Data = f.Data
		if f.Data == "" && m.URL == "" {
			m.URL = f.URL
		}
	}
	m.Kind = ir.MediaKindOf(m.MediaType)
	return m
}

func parseImageURL(u string) *ir.Image {
	img := &ir.Image{URL: u}
	if strings.HasPrefix(u, "data:") {
		if i := strings.Index(u, ","); i > 0 {
			img.MediaType = strings.TrimSuffix(u[5:i], ";base64")
			img.Data = u[i+1:]
			img.URL = ""
		}
	}
	return img
}

// encodeImagePart 图片块 -> input_image 部分。
//
// ok 为假表示三种载体（base64 / URL / file_id）一个都没有，本族的图片槽位
// 无从表达：照编会写出一个连 image_url 键都没有的 {"type":"input_image"}，
// 上游按必填字段校验直接 400，而报错只说图片无效，读者看不出是哪一段输入。
// 常见来源是客户端用了本层没建模的键名。损耗由 relay.Diagnose 报告。
func encodeImagePart(img *ir.Image) (contentPart, bool) {
	if img == nil {
		return contentPart{}, false
	}
	switch {
	case img.URL != "":
		return contentPart{Type: "input_image", ImageURL: &imageRef{URL: img.URL},
			Detail: img.Detail, FileID: img.FileID}, true
	case img.Data != "":
		return contentPart{Type: "input_image",
			ImageURL: &imageRef{URL: "data:" + img.MediaType + ";base64," + img.Data},
			Detail:   img.Detail, FileID: img.FileID}, true
	case img.FileID != "":
		// file_id 是本族图片槽位的第二种合法载体，同族往返原样带回。
		return contentPart{Type: "input_image", Detail: img.Detail, FileID: img.FileID}, true
	}
	return contentPart{}, false
}

func decodeSummary(raw json.RawMessage) string {
	var parts []summaryPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return ""
	}
	var sb strings.Builder
	for _, p := range parts {
		sb.WriteString(p.Text)
	}
	return sb.String()
}

func decodeToolChoice(v any) *ir.ToolChoice {
	switch tc := v.(type) {
	case string:
		switch tc {
		case "auto":
			return &ir.ToolChoice{Mode: ir.ChoiceAuto}
		case "none":
			return &ir.ToolChoice{Mode: ir.ChoiceNone}
		case "required":
			return &ir.ToolChoice{Mode: ir.ChoiceAny}
		}
	case map[string]any:
		// allowed_tools 是 responses/codex 一族的白名单形态：type 说的是「这是一条
		// 选择策略」而不是某个已声明工具的类型，内层 mode 才说要不要必须调。此前整个
		// map 分支只认 name，这个形态没有 name，于是返回 nil——tool_choice 被整条
		// 丢掉，连内层的 required 也一起没了，客户端既拿不到限制也拿不到注记。
		if typ, _ := tc["type"].(string); typ == "allowed_tools" {
			out := &ir.ToolChoice{Mode: ir.ChoiceAuto}
			if mode, _ := tc["mode"].(string); mode == "required" {
				out.Mode = ir.ChoiceAny
			}
			out.AllowedTools = decodeAllowedToolNames(tc["tools"])
			return out
		}
		if name, ok := tc["name"].(string); ok {
			out := &ir.ToolChoice{Mode: ir.ChoiceTool, ToolName: name}
			if typ, _ := tc["type"].(string); typ == "custom" {
				out.ToolKind = ir.ToolCustom
			}
			return out
		}
	}
	return nil
}

// decodeAllowedToolNames 取 allowed_tools.tools 里的工具名。条目通常是
// {"type":"function","name":...}，但白名单本质是一串名字，裸字符串形态也照收——
// 认不出来就当没有，宁可少收窄（由 Diagnose 报出）也不要凭空捏一个名字进去。
func decodeAllowedToolNames(v any) []string {
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	var names []string
	for _, item := range list {
		switch t := item.(type) {
		case string:
			if t != "" {
				names = append(names, t)
			}
		case map[string]any:
			name, _ := t["name"].(string)
			if name == "" {
				if fn, ok := t["function"].(map[string]any); ok {
					name, _ = fn["name"].(string)
				}
			}
			if name != "" {
				names = append(names, name)
			}
		}
	}
	return names
}

// ---- 请求编码：IR -> Responses ----

func (c codec) EncodeRequest(req *ir.Request) ([]byte, error) {
	r := req.Clone()
	opts := normalize.Strict()
	opts.EnsureFirstUser = false
	opts.EnsureAlternating = false
	if err := normalize.Request(r, opts); err != nil {
		return nil, err
	}
	out := request{
		Model:           r.Model,
		MaxOutputTokens: r.MaxTokens,
		Temperature:     r.Temperature,
		TopP:            r.TopP,
		Stream:          r.Stream,
		TopLogProbs:     r.TopLogProbs,
	}
	// 客户端只给了开关没给档位：这一族没有独立开关，不补档位就什么都拿不到。
	// 目标协议满足得了的请求不该因为字段形状不同而落空（Caps.LogProbsViaTopN）。
	if out.TopLogProbs == nil && r.LogProbs != nil && *r.LogProbs {
		n := 1
		out.TopLogProbs = &n
	}
	// instructions 缺省即无系统指令；仅订阅端点（Codex）强制字段存在，那一侧
	// 无 system 时输出空串。官方端点伪造空串是把「没给」改写成「给了一条空指令」。
	if sys := joinSystem(r.System); sys != "" || c.subscriptionCompat {
		out.Instructions = &sys
	}
	var items []inputItem
	for _, m := range r.Messages {
		items = append(items, encodeMessageItems(m, true)...)
	}
	if len(items) > 0 {
		out.Input = marshal(items)
	}
	for _, t := range r.Tools {
		if t.Hosted != "" {
			out.Tools = append(out.Tools, tool{Type: nativeHosted(t.Hosted)})
			continue
		}
		if t.Kind == ir.ToolCustom {
			out.Tools = append(out.Tools, tool{Type: "custom", Name: t.Name, Description: t.Description, Format: t.Format})
			continue
		}
		out.Tools = append(out.Tools, tool{Type: "function", Name: t.Name, Description: t.Description, Parameters: t.InputSchema, Strict: t.Strict})
	}
	out.ToolChoice = encodeToolChoice(r.ToolChoice)
	// 只在客户端明确禁止并行时写出。默认值由上游决定，替它写 true 是发明意图。
	if r.ToolChoice != nil && r.ToolChoice.DisableParallel {
		no := false
		out.ParallelToolCalls = &no
	}
	// 档位与「开思考」是两个轴，只有两种情形该写 effort：
	if t := r.Thinking; t != nil {
		rs := &reasoning{Summary: t.Summary, Context: t.Context, Mode: t.Mode}
		switch {
		case t.Enabled:
			rs.Effort = t.Effort
			if rs.Effort == "" {
				// 本族没有独立的思考开关，档位是表达「要思考」的唯一手段。
				rs.Effort = "medium"
			}
			if rs.Summary == "" {
				// 思考正文的回补依赖 reasoning_summary_* 终态帧，不点名要就没有
				// （与下面要 encrypted_content 同一目的，对齐 sub2api）。
				rs.Summary = "auto"
			}
			out.Include = append(out.Include, "reasoning.encrypted_content")
		case t.Effort == "none":
			// 显式关也要写出来：省略整个 reasoning 不等于「不思考」，上游会按自己的
			// 默认档（medium）思考，客户端要的「别思考」就成了「中档思考」。
			rs.Effort = "none"
		}
		// 关着却带别的档位（账号覆盖强制关、anthropic disabled 配 output_config.effort）
		// 时不写档位：写出去等于把「关」翻译成「开」。
		if rs.Effort != "" || rs.Summary != "" || len(rs.Context) > 0 || len(rs.Mode) > 0 {
			out.Reasoning = rs
		}
	}
	// 客户端自己的 include 条目并入（去重）：同协议回写是它们唯一的活路，
	// 其他三族没有「点名要额外回传载荷」的机制。
	for _, inc := range r.Include {
		if !slices.Contains(out.Include, inc) {
			out.Include = append(out.Include, inc)
		}
	}
	// 会话对象锚点与后台模式同协议回写（与 PreviousResponseID 同一族约束）。
	if r.ConversationID != "" {
		out.Conversation = json.RawMessage(marshal(r.ConversationID))
	}
	out.Background = r.Background
	// context_management 同族回写。
	for _, e := range r.ContextMgmt {
		out.ContextManagement = append(out.ContextManagement, contextMgmtEntry{
			Type: e.Type, CompactThreshold: e.CompactThreshold})
	}
	if r.Prompt != nil {
		out.Prompt = &promptRef{ID: r.Prompt.ID, Version: r.Prompt.Version, Variables: r.Prompt.Variables}
	}
	out.Text = encodeResponseFormat(r.ResponseFormat)
	// verbosity 挂在 text 下：没有 format 要求时也要为 verbosity 建容器。
	if r.Verbosity != "" {
		if out.Text == nil {
			out.Text = &textConfig{}
		}
		out.Text.Verbosity = r.Verbosity
	}
	// metadata 随响应回显，是客户端的关联数据通道：同族回吐原样发，user_id
	// 同时落 user 字段（顶层字段是滥用追踪的官方槽位）。
	if len(r.Metadata) > 0 {
		md, err := json.Marshal(r.Metadata)
		if err == nil {
			out.Metadata = md
		}
	}
	if uid := r.Metadata["user_id"]; uid != "" {
		out.User = uid
	}
	// 会话链同协议回写：链锚点是上游侧资源，只有 responses 系出站接得住。
	out.PreviousResponseID = r.PreviousResponseID
	// 客户端显式给了值就透传（显式 true 是客户端的选择，不该替它改）。
	// 缺省时官方 API 不发：store 的官方缺省是 true，伪造 false 会让响应不再
	// 落库，previous_response_id 会话链与事后拉取一起断掉，而客户端根本没提过
	// 这个字段。只有订阅端点（Codex 形态）强制 store=false，那一侧补默认值。
	if r.Store != nil {
		out.Store = r.Store
	} else if c.subscriptionCompat {
		f := false
		out.Store = &f
	}
	// 本族值集是 chat 的超集，只有 anthropic 方言 standard_only 需要翻译。
	if tier, ok := proto.MapServiceTier(r.ServiceTier, Name); ok {
		out.ServiceTier = tier
	}
	out.PromptCacheKey = r.PromptCacheKey
	out.SafetyIdentifier = r.SafetyIdentifier
	out.Moderation = r.Moderation
	out.PromptCacheOptions = r.PromptCacheOptions
	return json.Marshal(out)
}

func joinSystem(blocks []ir.Block) string {
	var parts []string
	for _, b := range blocks {
		if b.Type == ir.BlockText && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// encodeMessageItems 把一条 IR 消息展开为 Responses input items。
// forRequest 区分方向：请求方向要回传给上游，无本族真签名的 reasoning item
// 构造不出合法形态（OpenAI 会拒），整块跳过；响应方向是给客户端看的，
// 思考正文必须留下，只是签名位留空。
// tool_result 内嵌的图片提取为独立 user message（function_call_output 不能挂图片）。
func encodeMessageItems(m ir.Message, forRequest bool) []inputItem {
	var out []inputItem
	switch m.Role {
	case ir.RoleAssistant:
		var parts []contentPart
		flush := func() {
			if len(parts) > 0 {
				out = append(out, inputItem{Type: "message", Role: "assistant", Content: marshal(parts)})
				parts = nil
			}
		}
		for _, b := range m.Content {
			switch b.Type {
			case ir.BlockText:
				parts = append(parts, contentPart{Type: "output_text", Text: b.Text,
					Annotations: encodeAnnotations(b.Text, b.Citations)})
			case ir.BlockRefusal:
				parts = append(parts, contentPart{Type: "refusal", Refusal: b.Text})
			case ir.BlockOpaque:
				// 本族客户端发来的未知 part 原样回吐：丢掉就等于把用户这段输入从
				// 历史里抹掉，模型看不到它，客户端也拿不到任何注记。外族来源的整块
				// 跳过（损耗由 relay.Diagnose 报出）——逐字写进本族的 part 数组就是
				// 一个本族上游不认识的 part 型，会被按 part 型校验直接 400。
				if proto.OpaqueVerbatimFor(b.Opaque, Name) {
					parts = append(parts, contentPart{Raw: b.Opaque.Body})
				}
			case ir.BlockThinking:
				if b.Thinking == nil {
					continue
				}
				genuine := b.Thinking.SignatureGenuineFor(Name)
				if forRequest && !genuine {
					continue
				}
				flush()
				it := inputItem{
					Type:    "reasoning",
					Summary: marshal([]summaryPart{{Type: "summary_text", Text: b.Thinking.Text}}),
				}
				if genuine {
					it.EncryptedContent = b.Thinking.Signature
				}
				out = append(out, it)
			case ir.BlockToolUse:
				flush()
				if b.ToolUse != nil {
					if b.ToolUse.Kind == ir.ToolCustom {
						out = append(out, inputItem{Type: "custom_tool_call", CallID: b.ToolUse.ID, Name: b.ToolUse.Name, Input: b.ToolUse.InputText})
						continue
					}
					args := string(b.ToolUse.Input)
					if args == "" {
						args = "{}"
					}
					out = append(out, inputItem{Type: "function_call", CallID: b.ToolUse.ID, Name: b.ToolUse.Name, Arguments: args})
				}
			}
		}
		flush()
	case ir.RoleUser:
		start := len(out)
		var parts []contentPart
		flush := func() {
			if len(parts) > 0 {
				out = append(out, inputItem{Type: "message", Role: "user", Content: marshal(parts)})
				parts = nil
			}
		}
		for _, b := range m.Content {
			switch b.Type {
			case ir.BlockText:
				parts = append(parts, contentPart{Type: "input_text", Text: b.Text})
			case ir.BlockImage:
				if p, ok := encodeImagePart(b.Image); ok {
					parts = append(parts, p)
				}
			case ir.BlockMedia:
				parts = append(parts, encodeMediaPart(b.Media))
			case ir.BlockOpaque:
				// 处置同助手回合：本族原样回吐，外族整块跳过。
				if proto.OpaqueVerbatimFor(b.Opaque, Name) {
					parts = append(parts, contentPart{Raw: b.Opaque.Body})
				}
			case ir.BlockToolResult:
				flush()
				if b.ToolResult != nil {
					text, images := splitToolResultContent(b.ToolResult.Content)
					typ := "function_call_output"
					if b.ToolResult.Kind == ir.ToolCustom {
						typ = "custom_tool_call_output"
					}
					out = append(out, inputItem{Type: typ, CallID: b.ToolResult.ToolUseID, Output: text})
					if len(images) > 0 {
						out = append(out, inputItem{Type: "message", Role: "user", Content: marshal(images)})
					}
				}
			}
		}
		flush()
		if len(out) == start {
			// 这条消息的部件被编码器全丢了（外族来源的不透明块，或一张没有可投递
			// 载荷的图片），于是整条从 input 里消失。这比留一个空 content 更糟：
			// 若它是唯一的一条，input 会连键都没有，而上游把 input 当必填字段，
			// 400 拒整轮；即便还有别的消息，轮次结构也被悄悄改写（客户端发了 N 条，
			// 上游只收到 N-1 条），后续 tool_call 的配对随之错位。与 anthropic 侧
			// 同口径落约定占位——规整流水线补不上，它跑在编码之前，那会儿这条消息
			// 看起来还是有内容的。丢了什么由 relay.Diagnose 报出。
			out = append(out, inputItem{Type: "message", Role: "user",
				Content: marshal([]contentPart{{Type: "input_text", Text: normalize.Placeholder}})})
		}
	}
	return out
}

// splitToolResultContent 拆出文本与媒体（转成 input_image / input_file /
// input_audio parts）。function_call_output.output 只能是字符串，媒体必须另起
// 一条 user 消息承载。
func splitToolResultContent(blocks []ir.Block) (string, []contentPart) {
	var sb strings.Builder
	var media []contentPart
	for _, b := range blocks {
		switch b.Type {
		case ir.BlockText:
			sb.WriteString(b.Text)
		case ir.BlockImage:
			if p, ok := encodeImagePart(b.Image); ok {
				media = append(media, p)
			}
		case ir.BlockMedia:
			media = append(media, encodeMediaPart(b.Media))
		}
	}
	return sb.String(), media
}

// encodeMediaPart 媒体块 -> Responses 内容部分。音频走 input_audio，
// 其余走 input_file（不透明容器，视频亦借它透传）。
func encodeMediaPart(m *ir.Media) contentPart {
	if m == nil {
		return contentPart{Type: "input_text", Text: (*ir.Media)(nil).Describe()}
	}
	if m.Kind == ir.MediaAudio && m.Data != "" {
		format := m.Format
		if format == "" {
			format = strings.TrimPrefix(m.MediaType, "audio/")
		}
		return contentPart{Type: "input_audio", InputAudio: &inputAudio{Data: m.Data, Format: format}}
	}
	p := contentPart{Type: "input_file", FileID: m.FileID, Filename: m.Filename}
	switch {
	case m.Data != "":
		p.FileData = "data:" + m.MediaType + ";base64," + m.Data
	case m.URL != "":
		p.FileURL = m.URL
	}
	return p
}

func encodeToolChoice(tc *ir.ToolChoice) any {
	if tc == nil {
		return nil
	}
	switch tc.Mode {
	case ir.ChoiceAuto:
		return "auto"
	case ir.ChoiceNone:
		return "none"
	case ir.ChoiceAny:
		return "required"
	case ir.ChoiceTool:
		typ := "function"
		if tc.ToolKind == ir.ToolCustom {
			typ = "custom"
		}
		return toolChoiceNamed{Type: typ, Name: tc.ToolName}
	}
	return nil
}

// nativeHosted 规范托管工具种类 -> Responses 原生 type。
// 未识别种类原样透传（同协议往返场景，如 file_search）。
func nativeHosted(canonical string) string {
	switch canonical {
	case ir.HostedWebSearch:
		return "web_search"
	case ir.HostedCodeExecution:
		return "code_interpreter"
	default:
		return canonical
	}
}

// ---- 错误渲染 ----

func (codec) RenderError(e *ir.Error) (int, []byte) {
	return e.HTTPStatus(), marshal(errorResponse{Error: errorBody{Code: e.Code, Type: e.Type, Message: e.Message}})
}

// RenderStreamError 官方 wire 把错误体放在顶层（{"type":"error","error":{...}}），
// 不是 response.error 下——官方 SDK 的 ErrorEvent 只读顶层，挂在 response 下等于
// 错误对客户端完全不可见，还会顺带多出一个 id/model 全空的畸形 response 对象。
func (codec) RenderStreamError(e *ir.Error) []byte {
	return []byte("data: " + string(marshal(streamEvent{Type: "error",
		Error: &errorBody{Code: e.Code, Type: e.Type, Message: e.Message}})) + "\n\n")
}
