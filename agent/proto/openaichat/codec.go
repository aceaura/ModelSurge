package openaichat

import (
	"encoding/json"
	"fmt"
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

// Caps Chat Completions：reasoning_content 无签名机制，也无 hosted tools；
// DeepSeek 系上游思考模式下强制 tool_choice 会 400（EncodeRequest 兜底降级 auto）。
func (codec) Caps() proto.Capabilities {
	return proto.Capabilities{
		ThinkingSignature: false, Images: true, HostedTools: false, ThinkingForcedToolChoice: false,
		// TopK 留假：Chat 协议原生没有这一维，不是能力缺失而是字段不存在。
		ImageURLs: true, Sampling: true, TopK: false, ParallelToolCalls: true,
		// image_url.detail 是本协议原生的一维。
		ImageDetail: true,
		// 调参维度最全的一家：penalties / seed / n / logprobs / logit_bias 全有。
		Penalties: true, Seed: true, Candidates: true, LogProbs: true, LogitBias: true,
		// file 是不透明容器（文档与其他都能塞），input_audio 是音频专属槽位；
		// 视频没有任何专属入口，只能借 file 透传，故不声明能力。
		Documents: true, Audio: true, Video: false,
		Refusal: true, // message.refusal
		// tool 消息里没有失败标志位，失败结果与成功结果同形。
		ToolResultError:  false,
		StructuredOutput: true, // response_format
		Citations:        true, // message.annotations
		UserID:           true, // user
		// service_tier 与 prompt_cache_key 都有原生槽位。
		ServiceTier: true, PromptCacheKey: true,
		OpenAIExtras: true,
		ToolStrict:   true,
	}
}

// MapFinishReason OpenAI finish_reason -> 规范 StopReason。
func MapFinishReason(s string) ir.StopReason {
	switch s {
	case "stop":
		return ir.StopEndTurn
	case "length":
		return ir.StopMaxTokens
	case "tool_calls", "function_call":
		return ir.StopToolUse
	case "content_filter":
		return ir.StopRefusal
	default:
		return ir.StopEndTurn
	}
}

// UnmapFinishReason 规范 StopReason -> OpenAI finish_reason。
func UnmapFinishReason(s ir.StopReason) string {
	switch s {
	case ir.StopMaxTokens:
		return "length"
	case ir.StopToolUse:
		return "tool_calls"
	case ir.StopRefusal:
		return "content_filter"
	case ir.StopPauseTurn, ir.StopAborted, ir.StopContextWindow:
		// pause_turn 是「这一轮没做完，回传对话继续」，aborted 是「流断了」，
		// context_window 是输入占满窗口挤断输出，OpenAI 侧都没有对应值。
		// 取 length 而非 stop：三者都表示输出不完整，
		// 客户端至少不会把半截结果当成最终答案（stop 会）。真正的语义无法
		// 保留，由诊断告知。
		return "length"
	default: // end_turn 与 stop_sequence 都是 stop（OpenAI 不区分后者）
		return "stop"
	}
}

// ---- 请求解码：OpenAI -> IR ----

func (codec) DecodeRequest(body []byte) (*ir.Request, error) {
	var req request
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("openai-chat: decode request: %w", err)
	}
	out := &ir.Request{
		Model:            req.Model,
		Temperature:      req.Temperature,
		TopP:             req.TopP,
		Stream:           req.Stream,
		PresencePenalty:  req.PresencePenalty,
		FrequencyPenalty: req.FrequencyPenalty,
		Seed:             req.Seed,
		Candidates:       req.N,
		LogProbs:         req.LogProbs,
		TopLogProbs:      req.TopLogProbs,
		LogitBias:        req.LogitBias,
	}
	out.MaxTokens = req.MaxCompletionTokens
	out.MaxCompletionKey = req.MaxCompletionTokens > 0
	if out.MaxTokens == 0 {
		out.MaxTokens = req.MaxTokens
	}
	out.StopSequences = decodeStop(req.Stop)
	// stream_options.include_usage 决定流末那个只带 usage 的帧发不发。不解析就
	// 等于替客户端表态「要」：该帧的 choices 是空数组，没要的客户端按
	// choices[0] 取增量会越界。
	if req.StreamOptions != nil {
		out.IncludeUsage = req.StreamOptions.IncludeUsage
		out.IncludeObfuscation = req.StreamOptions.IncludeObfuscation
	}
	for _, m := range req.Messages {
		decodeMessage(out, m)
	}
	for _, t := range req.Tools {
		out.Tools = append(out.Tools, ir.Tool{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			InputSchema: t.Function.Parameters,
			Strict:      t.Function.Strict,
		})
	}
	out.ToolChoice = decodeToolChoice(req.ToolChoice)
	// parallel_tool_calls=false 是「禁止并行」，与 anthropic 的
	// disable_parallel_tool_use 同一维度。客户端没给时不表态（IR 零值即允许并行）。
	if req.ParallelToolCalls != nil && !*req.ParallelToolCalls {
		if out.ToolChoice == nil {
			out.ToolChoice = &ir.ToolChoice{Mode: ir.ChoiceAuto}
		}
		out.ToolChoice.DisableParallel = true
	}
	if req.ReasoningEffort != "" {
		out.Thinking = &ir.ThinkingConfig{Enabled: req.ReasoningEffort != "none", Effort: req.ReasoningEffort}
	}
	out.ResponseFormat = decodeResponseFormat(req.ResponseFormat)
	// metadata 是官方文档维度（16 对键值）：整条丢掉等于客户端的关联数据
	// 再也回不来。user 字段与 metadata.user_id 同维度，同给时 user 胜出
	// （顶层字段比嵌套键更显式）。
	if len(req.Metadata) > 0 {
		out.Metadata = proto.DecodeStringMap(req.Metadata)
	}
	if req.User != "" {
		if out.Metadata == nil {
			out.Metadata = map[string]string{}
		}
		out.Metadata["user_id"] = req.User
	}
	// 原值进 IR，跨族映射是出站的事（proto.MapServiceTier）。
	out.ServiceTier = req.ServiceTier
	out.PromptCacheKey = req.PromptCacheKey
	out.PromptCacheRetention = req.PromptCacheRetention
	out.Store = req.Store
	out.Verbosity = req.Verbosity
	out.SafetyIdentifier = req.SafetyIdentifier
	// 显式 null 等同没给：不归一的话 Clone 往返后变成非空 "null"，
	// 出站会多一个 null 键、诊断也会误报。
	if string(req.Moderation) != "null" {
		out.Moderation = req.Moderation
	}
	if string(req.PromptCacheOptions) != "null" {
		out.PromptCacheOptions = req.PromptCacheOptions
	}
	out.Modalities = req.Modalities
	// voice 两形态归一成 string：内置名直取，{id} 对象取 id（语义等价）。
	if req.Audio != nil {
		ao := &ir.AudioOutParam{Format: req.Audio.Format}
		var v string
		if err := json.Unmarshal(req.Audio.Voice, &v); err == nil {
			ao.Voice = v
		} else {
			var obj struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(req.Audio.Voice, &obj); err == nil {
				ao.Voice = obj.ID
			}
		}
		out.AudioOut = ao
	}
	// 显式 null 等同没给（同 Moderation 先例）。
	if string(req.Prediction) != "null" {
		out.Prediction = req.Prediction
	}
	if string(req.WebSearchOptions) != "null" {
		out.WebSearchOptions = req.WebSearchOptions
	}
	return out, nil
}

// decodeResponseFormat response_format -> IR。type:"text" 是默认值，
// 等同于「没提要求」，不进 IR——否则下游会以为客户端要求了什么。
func decodeResponseFormat(f *responseFormat) *ir.ResponseFormat {
	if f == nil || f.Type == "" || f.Type == "text" {
		return nil
	}
	out := &ir.ResponseFormat{}
	if f.JSONSchema != nil {
		out.Name = f.JSONSchema.Name
		out.Description = f.JSONSchema.Description
		out.Schema = f.JSONSchema.Schema
		out.Strict = f.JSONSchema.Strict != nil && *f.JSONSchema.Strict
	}
	return out
}

// encodeResponseFormat IR -> response_format。无 schema 时退回 json_object：
// 「要求合法 JSON」这层语义 json_object 能完整表达。
func encodeResponseFormat(f *ir.ResponseFormat) *responseFormat {
	if f == nil {
		return nil
	}
	if !f.IsSchema() {
		return &responseFormat{Type: "json_object"}
	}
	js := &jsonSchema{Name: f.Name, Description: f.Description, Schema: f.Schema}
	if js.Name == "" {
		js.Name = "response" // name 是 json_schema 的必填字段
	}
	if f.Strict {
		strict := true
		js.Strict = &strict
	}
	return &responseFormat{Type: "json_schema", JSONSchema: js}
}

func decodeStop(v any) []string {
	switch s := v.(type) {
	case string:
		return []string{s}
	case []any:
		out := make([]string, 0, len(s))
		for _, item := range s {
			if str, ok := item.(string); ok {
				out = append(out, str)
			}
		}
		return out
	}
	return nil
}

// decodeMessage 把一条 OpenAI 消息并入 IR 请求。
// system/developer 进顶层 System；tool 消息转成 user 消息的 tool_result 块。
func decodeMessage(req *ir.Request, m message) {
	switch m.Role {
	case "system", "developer":
		req.System = append(req.System, contentBlocks(m.Content)...)
	case "user":
		req.Messages = append(req.Messages, ir.Message{Role: ir.RoleUser, Content: contentBlocks(m.Content), Name: m.Name})
	case "assistant":
		msg := ir.Message{Role: ir.RoleAssistant, Content: attachCitations(contentBlocks(m.Content), decodeAnnotations(m.Annotations)), Name: m.Name}
		if len(m.Audio) > 0 && string(m.Audio) != "null" {
			var a audioRef
			if json.Unmarshal(m.Audio, &a) == nil {
				msg.AudioID = a.ID
			}
		}
		if m.Refusal != "" {
			msg.Content = append(msg.Content, ir.Block{Type: ir.BlockRefusal, Text: m.Refusal})
		}
		if m.ReasoningContent != "" {
			msg.Content = append([]ir.Block{{Type: ir.BlockThinking, Thinking: &ir.Thinking{Text: m.ReasoningContent}}}, msg.Content...)
		}
		for _, tc := range m.ToolCalls {
			msg.Content = append(msg.Content, ir.Block{Type: ir.BlockToolUse, ToolUse: toolUseFromCall(tc)})
		}
		if len(m.ToolCalls) == 0 && m.FunctionCall != nil && m.FunctionCall.Name != "" {
			// 废弃形态但载荷完整：name+arguments 直接进 IR（无 id 可带）
			msg.Content = append(msg.Content, ir.Block{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{
				Name:  m.FunctionCall.Name,
				Input: json.RawMessage(m.FunctionCall.Arguments),
			}})
		}
		req.Messages = append(req.Messages, msg)
	case "tool":
		req.Messages = append(req.Messages, ir.Message{Role: ir.RoleUser, Name: m.Name, Content: []ir.Block{{
			Type: ir.BlockToolResult,
			ToolResult: &ir.ToolResult{
				ToolUseID: m.ToolCallID,
				Content:   contentBlocks(m.Content),
			},
		}}})
	default: // function 等未知角色归一为 user
		req.Messages = append(req.Messages, ir.Message{Role: ir.RoleUser, Content: contentBlocks(m.Content), Name: m.Name})
	}
}

// toolUseFromCall wire 工具调用 -> IR。type=custom 是原生形态
// （openai_chat.go ChatCompletionMessageCustomToolCall）：此前只读 function
// 槽位，custom 调用被伪造成空名函数调用，客户端按函数名路由必然落空。
func toolUseFromCall(tc toolCall) *ir.ToolUse {
	if tc.Type == "custom" && tc.Custom != nil {
		t := &ir.ToolUse{ID: tc.ID, Name: tc.Custom.Name, Kind: ir.ToolCustom, InputText: tc.Custom.Input}
		t.Input = t.ObjectInput()
		return t
	}
	return &ir.ToolUse{
		ID:    tc.ID,
		Name:  tc.Function.Name,
		Input: json.RawMessage(tc.Function.Arguments),
	}
}

// contentBlocks 解析 content（string 或 []part）为 IR 块。
// 数组逐元素解析：一次性解 []part 时，任一元素的形状冲突都会让整条 content 的
// Unmarshal 失败，同消息里用户真正在问的那句话跟着一起蒸发，调用方只拿到 nil。
func contentBlocks(raw json.RawMessage) []ir.Block {
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
		var p part
		if err := json.Unmarshal(rp, &p); err != nil {
			continue // 单个元素形状冲突：只丢它自己，兄弟块照常解出
		}
		switch p.Type {
		case "text":
			out = append(out, ir.Block{Type: ir.BlockText, Text: p.Text})
		case "image_url":
			if p.ImageURL != nil {
				img := parseImageURL(p.ImageURL.URL)
				img.Detail = p.ImageURL.Detail
				out = append(out, ir.Block{Type: ir.BlockImage, Image: img})
			}
		case "input_audio":
			if p.InputAudio != nil {
				mime := "audio/" + p.InputAudio.Format
				out = append(out, ir.Block{Type: ir.BlockMedia, Media: &ir.Media{
					Kind: ir.MediaAudio, MediaType: mime,
					Data: p.InputAudio.Data, Format: p.InputAudio.Format,
				}})
			}
		case "file":
			if p.File != nil {
				out = append(out, ir.Block{Type: ir.BlockMedia, Media: decodeFilePart(p.File)})
			}
		case "":
			// 连 type 都读不出来的元素不是 content part：留着会在出站时变成
			// {"type":""} 让上游 400（同 anthropic decodeRawBlock 的口径）。
		default:
			// 未知 part（video_url、厂商私有的输入形态）原样留成不透明块。
			// 静默丢掉会让模型以为用户没给这段输入，客户端还拿不到任何注记可循；
			// 逐字段猜则必丢内容。同族原样带回无损，外族整块跳过并报损耗。
			out = append(out, ir.Block{Type: ir.BlockOpaque,
				Opaque: &ir.Opaque{WireType: p.Type, Body: rp, From: Name}})
		}
	}
	return out
}

// decodeFilePart 解 file 部分。file_data 是 data URI，从里面拆出真 MIME；
// 只给 file_id 时 MIME 不可知，归 MediaOther 而不是猜 PDF——猜错会让附件
// 投进目标协议的文档槽位，上游解析失败。
func decodeFilePart(f *filePart) *ir.Media {
	m := &ir.Media{Filename: f.Filename, FileID: f.FileID}
	if f.FileData != "" {
		img := parseImageURL(f.FileData) // data URI 拆解逻辑与图片一致
		m.MediaType = img.MediaType
		m.Data = img.Data
		if img.Data == "" {
			m.URL = img.URL
		}
	}
	m.Kind = ir.MediaKindOf(m.MediaType)
	return m
}

// encodeMediaPart 媒体块 -> Chat 内容部分。音频有专属槽位 input_audio，
// 其余（文档 / 视频 / 认不出的）走 file。Chat 没有视频入口，但 file 是不透明
// 容器，原样带过去比换成占位文本保留更多信息——真装不下时由上游报错，而诊断
// 已经告知了客户端。这与 Anthropic 侧不同：那边 document 会被按 PDF 解析。
func encodeMediaPart(m *ir.Media) part {
	if m == nil {
		return part{Type: "text", Text: (*ir.Media)(nil).Describe()}
	}
	if m.Kind == ir.MediaAudio && m.Data != "" {
		format := m.Format
		if format == "" {
			format = strings.TrimPrefix(m.MediaType, "audio/")
		}
		return part{Type: "input_audio", InputAudio: &inputAudio{Data: m.Data, Format: format}}
	}
	f := &filePart{FileID: m.FileID, Filename: m.Filename}
	switch {
	case m.Data != "":
		f.FileData = "data:" + m.MediaType + ";base64," + m.Data
	case m.URL != "":
		f.FileData = m.URL
	}
	return part{Type: "file", File: f}
}

// parseImageURL 解析 image_url，data URI 拆出 media type 与 base64。
func parseImageURL(u string) *ir.Image {
	img := &ir.Image{URL: u}
	if strings.HasPrefix(u, "data:") {
		if i := strings.Index(u, ","); i > 0 {
			head := u[5:i] // 去掉 "data:"
			img.MediaType = strings.TrimSuffix(head, ";base64")
			img.Data = u[i+1:]
			img.URL = ""
		}
	}
	return img
}

// encodeImagePart 图片块 -> image_url 部分。ok 为假表示这张图没有可投递的
// 载荷（base64 / URL / file_id 三者全空），调用方必须整个部件跳过：照编会
// 写出 {"url":""}，上游按 URL 形态校验直接 400，而报错只指向「图片无效」，
// 读者看不出是哪一段输入害的。Chat 的图片槽位也不认 file_id，所以只带
// file_id 的 Responses 图片投到这里同样落在这一支，损耗由 relay.Diagnose 报出。
func encodeImagePart(img *ir.Image) (part, bool) {
	if !img.HasPayload() {
		return part{}, false
	}
	url := img.URL
	if url == "" {
		url = "data:" + img.MediaType + ";base64," + img.Data
	}
	return part{Type: "image_url", ImageURL: &imageURL{URL: url, Detail: img.Detail}}, true
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
		// allowed_tools 是 chat 一族的白名单形态（官方
		// ChatCompletionAllowedToolChoiceParam）：与 responses 的平铺不同，chat
		// 是嵌套的 {"type":"allowed_tools","allowed_tools":{"mode":...,"tools":[...]}}。
		// 此前 map 分支只认 function.name，这个形态整条 tool_choice 被丢掉，连内层
		// 的 required 也一起没了。
		if typ, _ := tc["type"].(string); typ == "allowed_tools" {
			inner, _ := tc["allowed_tools"].(map[string]any)
			out := &ir.ToolChoice{Mode: ir.ChoiceAuto}
			if mode, _ := inner["mode"].(string); mode == "required" {
				out.Mode = ir.ChoiceAny
			}
			out.AllowedTools = decodeChatAllowedToolNames(inner["tools"])
			return out
		}
		if fn, ok := tc["function"].(map[string]any); ok {
			if name, ok := fn["name"].(string); ok {
				return &ir.ToolChoice{Mode: ir.ChoiceTool, ToolName: name}
			}
		}
		// custom 指名（官方 ChatCompletionNamedToolChoiceCustomParam）：
		// {"type":"custom","custom":{"name":...}}，指名的是 custom 工具不是 function。
		if cu, ok := tc["custom"].(map[string]any); ok {
			if name, ok := cu["name"].(string); ok {
				return &ir.ToolChoice{Mode: ir.ChoiceTool, ToolName: name, ToolKind: ir.ToolCustom}
			}
		}
	}
	return nil
}

// decodeChatAllowedToolNames 取 chat allowed_tools.tools 里的工具名。条目是工具
// 定义（{"type":"function","function":{"name":...}} 或 custom 同形），名字嵌在
// 子对象里；认不出来的条目跳过，宁可少收窄（由 Diagnose 报出）也不要凭空捏名字。
func decodeChatAllowedToolNames(v any) []string {
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	var names []string
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		for _, key := range []string{"function", "custom"} {
			if sub, ok := m[key].(map[string]any); ok {
				if name, _ := sub["name"].(string); name != "" {
					names = append(names, name)
					break
				}
			}
		}
	}
	return names
}

// ---- 请求编码：IR -> OpenAI ----

func (codec) EncodeRequest(req *ir.Request) ([]byte, error) {
	r := req.Clone()
	// OpenAI 对结构约束宽松，但 tool_use 后须有 tool 结果（role:tool），
	// 无 tools 时上游会拒绝 tool 消息，因此仍跑规整。
	opts := normalize.Strict()
	opts.EnsureFirstUser = false // OpenAI 允许 system 开头，system 已单独处理
	opts.EnsureAlternating = false
	if err := normalize.Request(r, opts); err != nil {
		return nil, err
	}
	out := request{
		Model:            r.Model,
		Temperature:      r.Temperature,
		TopP:             r.TopP,
		Stream:           r.Stream,
		PresencePenalty:  r.PresencePenalty,
		FrequencyPenalty: r.FrequencyPenalty,
		Seed:             r.Seed,
		N:                r.Candidates,
		LogProbs:         r.LogProbs,
		TopLogProbs:      r.TopLogProbs,
		LogitBias:        r.LogitBias,
	}
	// 同族往返按来路键名带回：客户端给的现代键 max_completion_tokens 不能
	// 被换写成官方已废弃的旧键（旧键不兼容 o 系推理模型，openai_chat.go:3826）。
	// 旧键客户端的 wire 原样保留；跨族投影维持既有形状不额外改形。
	if r.MaxCompletionKey {
		out.MaxCompletionTokens = r.MaxTokens
	} else {
		out.MaxTokens = r.MaxTokens
	}
	if len(r.StopSequences) == 1 {
		out.Stop = r.StopSequences[0]
	} else if len(r.StopSequences) > 1 {
		out.Stop = r.StopSequences
	}
	if r.Stream {
		// 恒注入，刻意不跟随 r.IncludeUsage：Agent 记账（ResultReport 的 usage）
		// 依赖上游回报用量，客户端要不要看是另一回事，由客户端侧编码器决定
		// （见 stream_encoder.go 的 includeUsage）。把这里改成跟随客户端意图会
		// 让没 opt-in 的请求丢掉记账数据。
		out.StreamOptions = &streamOptions{IncludeUsage: true}
	}
	if r.IncludeObfuscation != nil {
		if out.StreamOptions == nil {
			out.StreamOptions = &streamOptions{}
		}
		out.StreamOptions.IncludeObfuscation = r.IncludeObfuscation
	}
	if sys := joinSystem(r.System); sys != "" {
		out.Messages = append(out.Messages, message{Role: "system", Content: json.RawMessage(marshalString(sys))})
	}
	for _, m := range r.Messages {
		out.Messages = append(out.Messages, encodeMessages(m)...)
	}
	out.Messages = fixToolOrder(out.Messages)
	for _, t := range r.Tools {
		if t.Hosted != "" {
			continue // Chat Completions 无托管工具能力，丢弃（diagnose 已记录）
		}
		out.Tools = append(out.Tools, tool{Type: "function", Function: toolFunc{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  t.InputSchema,
			Strict:      t.Strict,
		}})
	}
	out.ToolChoice = encodeToolChoice(r.ToolChoice)
	// 只在客户端明确禁止并行时写出。默认值由上游决定，替它写 true 是发明意图。
	if r.ToolChoice != nil && r.ToolChoice.DisableParallel {
		no := false
		out.ParallelToolCalls = &no
	}
	thinkingOn := r.Thinking != nil && r.Thinking.Enabled
	if thinkingOn {
		// DeepSeek 系上游思考模式下强制 tool_choice（required/指定函数）会 400，降级 auto
		if r.ToolChoice != nil && (r.ToolChoice.Mode == ir.ChoiceAny || r.ToolChoice.Mode == ir.ChoiceTool) {
			out.ToolChoice = "auto"
		}
	}
	// 档位与「开思考」是两个轴，只有两种情形该写 reasoning_effort：
	if r.Thinking != nil {
		switch {
		case r.Thinking.Enabled:
			effort := r.Thinking.Effort
			if effort == "" {
				// 本族没有独立的思考开关，档位是表达「要思考」的唯一手段。
				effort = "medium"
			}
			out.ReasoningEffort = effort
		case r.Thinking.Effort == "none":
			// 显式关也要写出来：省略 reasoning_effort 不等于「不思考」，上游会按
			// 自己的默认档思考，客户端要的「别思考」就成了「中档思考」。
			out.ReasoningEffort = "none"
		}
		// 关着却带别的档位（账号覆盖强制关）时不写：写出去等于把「关」翻译成「开」。
	}
	out.ResponseFormat = encodeResponseFormat(r.ResponseFormat)
	// metadata 同族回吐：客户端的关联数据通道，user_id 同时落 user 字段
	// （顶层字段是滥用追踪的官方槽位）。
	if len(r.Metadata) > 0 {
		md, err := json.Marshal(r.Metadata)
		if err == nil {
			out.Metadata = md
		}
	}
	if uid := r.Metadata["user_id"]; uid != "" {
		out.User = uid
	}
	// ultrafast 是 responses 专属，chat 值集 provably 装不下：丢弃由诊断报出。
	if tier, ok := proto.MapServiceTier(r.ServiceTier, Name); ok {
		out.ServiceTier = tier
	}
	out.PromptCacheKey = r.PromptCacheKey
	out.PromptCacheRetention = r.PromptCacheRetention
	out.Store = r.Store
	out.Verbosity = r.Verbosity
	out.SafetyIdentifier = r.SafetyIdentifier
	out.Moderation = r.Moderation
	out.PromptCacheOptions = r.PromptCacheOptions
	// modalities 的官方值集只有 text/audio：gemini 入站归一来的 image 等值
	// 写出去是上游必 400 的形状，被滤掉的值由诊断报出。
	for _, m := range r.Modalities {
		if m == "text" || m == "audio" {
			out.Modalities = append(out.Modalities, m)
		}
	}
	// voice 恒写 string 形态（{id} 对象与 string 语义等价，取最简）。
	if r.AudioOut != nil {
		out.Audio = &audioOutParam{Format: r.AudioOut.Format,
			Voice: json.RawMessage(marshal(r.AudioOut.Voice))}
	}
	out.Prediction = r.Prediction
	out.WebSearchOptions = r.WebSearchOptions
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

func marshalString(s string) []byte {
	b, _ := json.Marshal(s)
	return b
}

// encodeMessages 把一条 IR 消息展开为 OpenAI 消息：
// user 的 tool_result 块拆成独立 role:tool 消息；
// assistant 的 tool_use 块并入 tool_calls。
func encodeMessages(m ir.Message) []message {
	switch m.Role {
	case ir.RoleAssistant:
		msg := message{Role: "assistant", Name: m.Name}
		if m.AudioID != "" {
			msg.Audio = marshal(audioRef{ID: m.AudioID})
		}
		var text string
		var cites []ir.Citation
		for _, b := range m.Content {
			switch b.Type {
			case ir.BlockText:
				// 多个文本块会被拼成一条 content，块内偏移量要整体平移到拼接后
				// 的位置，否则第二个块的引用会指到第一个块的正文里。
				cites = append(cites, shiftCitations(b.Citations, text, b.Text)...)
				text += b.Text
			case ir.BlockRefusal:
				// 历史里的拒绝也要带回：上一轮模型拒绝过是下一轮的上下文，
				// 丢了会让模型看不到自己拒绝过，可能被同样的追问绕过。
				msg.Refusal += b.Text
			case ir.BlockThinking:
				if b.Thinking != nil {
					msg.ReasoningContent += b.Thinking.Text
				}
			case ir.BlockToolUse:
				if b.ToolUse != nil {
					if b.ToolUse.Kind == ir.ToolCustom {
						// 本族原生形态（custom{name,input}），不再投影成函数
						msg.ToolCalls = append(msg.ToolCalls, toolCall{
							Index:  len(msg.ToolCalls),
							ID:     b.ToolUse.ID,
							Type:   "custom",
							Custom: &customCall{Name: b.ToolUse.Name, Input: b.ToolUse.InputText},
						})
						continue
					}
					args := string(b.ToolUse.Input)
					if args == "" {
						args = "{}"
					}
					msg.ToolCalls = append(msg.ToolCalls, toolCall{
						Index:    len(msg.ToolCalls),
						ID:       b.ToolUse.ID,
						Type:     "function",
						Function: functionCall{Name: b.ToolUse.Name, Arguments: args},
					})
				}
			}
		}
		if text != "" {
			msg.Content = json.RawMessage(marshalString(text))
		}
		msg.Annotations = encodeAnnotations(text, cites)
		return []message{msg}
	case ir.RoleUser:
		var out []message
		var parts []part
		flush := func() {
			if len(parts) == 0 {
				return
			}
			// 纯文本单块用 string 形态，兼容性最好
			if len(parts) == 1 && parts[0].Type == "text" {
				out = append(out, message{Role: "user", Content: json.RawMessage(marshalString(parts[0].Text)), Name: m.Name})
			} else {
				out = append(out, message{Role: "user", Content: marshal(parts), Name: m.Name})
			}
			parts = nil
		}
		for _, b := range m.Content {
			switch b.Type {
			case ir.BlockText:
				parts = append(parts, part{Type: "text", Text: b.Text})
			case ir.BlockImage:
				if p, ok := encodeImagePart(b.Image); ok {
					parts = append(parts, p)
				}
			case ir.BlockMedia:
				parts = append(parts, encodeMediaPart(b.Media))
			case ir.BlockOpaque:
				// 本族客户端发来的未知 part 原样回吐：丢掉就等于把用户这段输入从
				// 历史里抹掉，模型看不到它，客户端也拿不到任何注记。外族来源的整块
				// 跳过（损耗由 relay.Diagnose 报出）——逐字写进本族的 part 数组就是
				// 一个本族上游不认识的 part 型，会被按 part 型校验直接 400。
				// 助手回合不在此列：本族的 assistant content 出站是纯字符串形态，
				// 装不下 part 数组。
				if proto.OpaqueVerbatimFor(b.Opaque, Name) {
					parts = append(parts, part{Raw: b.Opaque.Body})
				}
			case ir.BlockToolResult:
				flush()
				if b.ToolResult != nil {
					out = append(out, message{
						Role:       "tool",
						ToolCallID: b.ToolResult.ToolUseID,
						Content:    json.RawMessage(marshalString(blocksText(b.ToolResult.Content))),
					})
					// tool 消息 content 只能是文本；结果里的媒体块抽出为
					// 紧随的 user 媒体消息（fixToolOrder 会挪到整组回复之后）
					var imgParts []part
					for _, c := range b.ToolResult.Content {
						switch {
						case c.Type == ir.BlockImage:
							if p, ok := encodeImagePart(c.Image); ok {
								imgParts = append(imgParts, p)
							}
						case c.Type == ir.BlockMedia:
							imgParts = append(imgParts, encodeMediaPart(c.Media))
						}
					}
					if len(imgParts) > 0 {
						out = append(out, message{Role: "user", Content: marshal(imgParts), media: true})
					}
				}
			}
		}
		flush()
		if len(out) == 0 {
			// 这条消息的部件被编码器全丢了（外族来源的不透明块，或一张没有可投递
			// 载荷的图片）。此前落的是空字符串 content：上游看来等于「用户什么都
			// 没说」，客户端也无从分辨这是占位还是正文，与 anthropic 侧的约定占位
			// 不同口径。规整流水线补不上——它跑在编码之前，那会儿这条消息看起来
			// 还是有内容的。丢了什么由 relay.Diagnose 报出。
			out = append(out, message{Role: "user",
				Content: json.RawMessage(marshalString(normalize.Placeholder))})
		}
		return out
	}
	return []message{{Role: string(m.Role), Content: json.RawMessage(`""`)}}
}

// fixToolOrder 重建 tool 消息布局，满足严格上游（DeepSeek 等）的不变式：
// assistant 的每个 tool_calls 都要有对应 role:tool 回复，且紧随该 assistant
// （按 call 顺序连续排列）。客户端裁剪/错序历史里可挽救的 tool 回复被
// 重排到 governing assistant 旁（参考 sub2api normalize 思路）；孤儿
// （id 无对应 call）降级为 user 文本，重复 id 静默丢弃；tool 结果抽出的
// 媒体消息统一压到整组回复之后。
func fixToolOrder(msgs []message) []message {
	// 索引：id -> 首条 tool 消息下标；mediaOf：tool 消息 -> 其后紧随的媒体消息
	byID := map[string]int{}
	mediaOf := map[int][]message{}
	owned := map[int]bool{} // 已归属到某 tool 消息的媒体下标
	for i, m := range msgs {
		if m.Role == "tool" {
			if _, ok := byID[m.ToolCallID]; !ok {
				byID[m.ToolCallID] = i
			}
			continue
		}
		if m.media && i > 0 && msgs[i-1].Role == "tool" {
			mediaOf[i-1] = append(mediaOf[i-1], m)
			owned[i] = true
		}
	}
	used := make([]bool, len(msgs))
	out := make([]message, 0, len(msgs))
	downgrade := func(m message) {
		var text string
		_ = json.Unmarshal(m.Content, &text)
		out = append(out, message{Role: "user", Content: json.RawMessage(marshalString(
			fmt.Sprintf("[Tool Result (%s)]\n%s", m.ToolCallID, text)))})
	}
	for i, m := range msgs {
		switch {
		case m.Role == "tool":
			if used[i] || byID[m.ToolCallID] != i {
				continue // 已重排安置；重复 id 丢弃
			}
			used[i] = true
			downgrade(m)                     // 孤儿：无 governing call
			out = append(out, mediaOf[i]...) // 孤儿的媒体跟随降级文本
		case m.media:
			if owned[i] {
				continue // 已随所属 tool 消息安置
			}
			out = append(out, m) // 未归属（非 tool 紧随）：按普通 user 消息输出
		case m.Role == "assistant" && len(m.ToolCalls) > 0:
			out = append(out, m)
			var tail []message
			for _, tc := range m.ToolCalls {
				j, ok := byID[tc.ID]
				if !ok || used[j] {
					continue
				}
				used[j] = true
				out = append(out, msgs[j])
				tail = append(tail, mediaOf[j]...)
			}
			out = append(out, tail...) // 媒体压组尾，不打断 tool 序列
		default:
			out = append(out, m)
		}
	}
	return out
}

func blocksText(blocks []ir.Block) string {
	var sb strings.Builder
	for _, b := range blocks {
		if b.Type == ir.BlockText {
			sb.WriteString(b.Text)
		}
	}
	return sb.String()
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
		if tc.ToolKind == ir.ToolCustom {
			c := toolChoiceNamedCustom{Type: "custom"}
			c.Custom.Name = tc.ToolName
			return c
		}
		c := toolChoiceNamed{Type: "function"}
		c.Function.Name = tc.ToolName
		return c
	}
	return nil
}

// ---- 错误渲染 ----

func (codec) RenderError(e *ir.Error) (int, []byte) {
	return e.HTTPStatus(), marshal(errorResponse{Error: errorBody{Type: e.Type, Code: e.Code, Message: e.Message}})
}

func (codec) RenderStreamError(e *ir.Error) []byte {
	return []byte("data: " + string(marshal(errorResponse{Error: errorBody{Type: e.Type, Code: e.Code, Message: e.Message}})) + "\n\ndata: [DONE]\n\n")
}
