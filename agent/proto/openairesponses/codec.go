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

type codec struct{}

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
	if req.Instructions != "" {
		out.System = append(out.System, ir.Block{Type: ir.BlockText, Text: req.Instructions})
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
		if t.Type != "" && t.Type != "function" {
			out.Tools = append(out.Tools, ir.Tool{Hosted: ir.CanonicalHosted(t.Type)})
			continue
		}
		out.Tools = append(out.Tools, ir.Tool{Name: t.Name, Description: t.Description, InputSchema: t.Parameters, Strict: t.Strict})
	}
	out.ToolChoice = decodeToolChoice(req.ToolChoice)
	// 同 Chat：parallel_tool_calls=false 是「禁止并行」。没给则不表态。
	if req.ParallelToolCalls != nil && !*req.ParallelToolCalls {
		if out.ToolChoice == nil {
			out.ToolChoice = &ir.ToolChoice{Mode: ir.ChoiceAuto}
		}
		out.ToolChoice.DisableParallel = true
	}
	if req.Reasoning != nil && req.Reasoning.Effort != "" {
		out.Thinking = &ir.ThinkingConfig{Enabled: req.Reasoning.Effort != "none" && req.Reasoning.Effort != "minimal", Effort: req.Reasoning.Effort}
	}
	if req.Text != nil {
		out.ResponseFormat = decodeResponseFormat(req.Text.Format)
		out.Verbosity = req.Text.Verbosity
	}
	if req.User != "" {
		out.Metadata = map[string]string{"user_id": req.User}
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
	case "function_call_output":
		req.Messages = append(req.Messages, ir.Message{Role: ir.RoleUser, Content: []ir.Block{{
			Type:       ir.BlockToolResult,
			ToolResult: &ir.ToolResult{ToolUseID: it.CallID, Content: []ir.Block{{Type: ir.BlockText, Text: it.Output}}},
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
	var parts []contentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil
	}
	out := make([]ir.Block, 0, len(parts))
	for _, p := range parts {
		switch p.Type {
		case "input_text", "output_text", "text":
			out = append(out, ir.Block{Type: ir.BlockText, Text: p.Text, Citations: decodeAnnotations(p.Annotations)})
		case "refusal":
			// 拒绝正文是可见内容，不是元数据。漏读会让拒绝响应变成一条空消息，
			// 客户端看到 200 + 空 content 会误判成成功（cc-switch handlers.rs
			// 同款结论）。
			out = append(out, ir.Block{Type: ir.BlockRefusal, Text: p.Refusal})
		case "input_image":
			out = append(out, ir.Block{Type: ir.BlockImage, Image: parseImageURL(p.ImageURL)})
		case "input_file":
			out = append(out, ir.Block{Type: ir.BlockMedia, Media: decodeInputFile(p)})
		case "input_audio":
			if p.InputAudio != nil {
				out = append(out, ir.Block{Type: ir.BlockMedia, Media: &ir.Media{
					Kind: ir.MediaAudio, MediaType: "audio/" + p.InputAudio.Format,
					Data: p.InputAudio.Data, Format: p.InputAudio.Format,
				}})
			}
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
		if name, ok := tc["name"].(string); ok {
			return &ir.ToolChoice{Mode: ir.ChoiceTool, ToolName: name}
		}
	}
	return nil
}

// ---- 请求编码：IR -> Responses ----

func (codec) EncodeRequest(req *ir.Request) ([]byte, error) {
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
	out.Instructions = joinSystem(r.System)
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
		out.Tools = append(out.Tools, tool{Type: "function", Name: t.Name, Description: t.Description, Parameters: t.InputSchema, Strict: t.Strict})
	}
	out.ToolChoice = encodeToolChoice(r.ToolChoice)
	// 只在客户端明确禁止并行时写出。默认值由上游决定，替它写 true 是发明意图。
	if r.ToolChoice != nil && r.ToolChoice.DisableParallel {
		no := false
		out.ParallelToolCalls = &no
	}
	if r.Thinking != nil && r.Thinking.Enabled {
		effort := r.Thinking.Effort
		if effort == "" {
			effort = "medium"
		}
		out.Reasoning = &reasoning{Effort: effort, Summary: "auto"}
		// 要求上游回传 encrypted_content 以便还原 thinking 签名（对齐 sub2api）
		out.Include = append(out.Include, "reasoning.encrypted_content")
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
	if uid := r.Metadata["user_id"]; uid != "" {
		out.User = uid
	}
	// 会话链同协议回写：链锚点是上游侧资源，只有 responses 系出站接得住。
	out.PreviousResponseID = r.PreviousResponseID
	// 订阅端点（Codex 形态）要求 store=false；对官方 API 无害。
	// 客户端显式给了值就透传（显式 true 是客户端的选择，不该替它改）。
	if r.Store != nil {
		out.Store = r.Store
	} else {
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
				if b.Image != nil {
					url := b.Image.URL
					if url == "" && b.Image.Data != "" {
						url = "data:" + b.Image.MediaType + ";base64," + b.Image.Data
					}
					parts = append(parts, contentPart{Type: "input_image", ImageURL: url})
				}
			case ir.BlockMedia:
				parts = append(parts, encodeMediaPart(b.Media))
			case ir.BlockToolResult:
				flush()
				if b.ToolResult != nil {
					text, images := splitToolResultContent(b.ToolResult.Content)
					out = append(out, inputItem{Type: "function_call_output", CallID: b.ToolResult.ToolUseID, Output: text})
					if len(images) > 0 {
						out = append(out, inputItem{Type: "message", Role: "user", Content: marshal(images)})
					}
				}
			}
		}
		flush()
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
			if b.Image != nil {
				url := b.Image.URL
				if url == "" && b.Image.Data != "" {
					url = "data:" + b.Image.MediaType + ";base64," + b.Image.Data
				}
				media = append(media, contentPart{Type: "input_image", ImageURL: url})
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
		return toolChoiceNamed{Type: "function", Name: tc.ToolName}
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

func (codec) RenderStreamError(e *ir.Error) []byte {
	return []byte("data: " + string(marshal(streamEvent{Type: "error", Response: &responseObj{Error: &errorBody{Code: e.Code, Type: e.Type, Message: e.Message}}})) + "\n\n")
}
