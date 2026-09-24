package anthropic

import (
	"encoding/json"
	"fmt"

	"github.com/aceaura/ModelSurge/agent/ir"
	"github.com/aceaura/ModelSurge/agent/normalize"
	"github.com/aceaura/ModelSurge/agent/proto"
)

// Name 协议标识。
const Name = "anthropic"

func init() { proto.Register(codec{}) }

type codec struct{}

// New 返回 codec（便于测试直接构造）。
func New() proto.Codec { return codec{} }

func (codec) Name() string { return Name }

// Caps Anthropic 是全能力协议：签名、图片、托管工具均原生支持；
// 思考模式下强制 tool_choice（any/tool）亦为协议支持形态。
func (codec) Caps() proto.Capabilities {
	return proto.Capabilities{
		ThinkingSignature: true, Images: true, HostedTools: true, ThinkingForcedToolChoice: true,
		ImageURLs: true, Sampling: true, TopK: true, ParallelToolCalls: true,
		ToolResultError: true,
		// document 块收 PDF 与纯文本；音频与视频没有任何入口。
		Documents: true, Audio: false, Video: false,
		// 没有 refusal 槽位：只有 stop_reason=refusal，正文得并进 text。
		Refusal: false,
		// output_config.format（2026 新增）只接 json_schema 一种形态，
		// 纯 JSON 模式没有槽位。
		StructuredOutput: true, StructuredOutputSchemaOnly: true,
		// service_tier（auto/standard_only）。没有 prompt_cache_key：
		// 缓存走显式 cache_control 断点。
		ServiceTier: true, PromptCacheKey: false,
		ToolStrict: true,
		Citations:  true, // text.citations
		// tool_use.input 是 JSON 对象槽位。
		ToolInputObject: true,
		// metadata.user_id。
		UserID: true,
	}
}

// ClampThinking 把 thinking 预算归一成线上可携带形态：缺省补默认值；
// Anthropic 约束 budget_tokens < max_tokens，越界夹紧（夹紧到 0 则编码时
// omitempty 丢弃）。relay 在转发日志前经可选接口调用，使上游视角参数行
// 反映实发值；EncodeRequest 内同款逻辑保持幂等兜底。
func (codec) ClampThinking(r *ir.Request) {
	if r.Thinking == nil || !r.Thinking.Enabled || r.MaxTokens <= 0 {
		return
	}
	if r.Thinking.BudgetTokens <= 0 {
		r.Thinking.BudgetTokens = 4096
	}
	if r.Thinking.BudgetTokens >= r.MaxTokens {
		r.Thinking.BudgetTokens = r.MaxTokens - 1
	}
}

// ---- 请求解码：Anthropic -> IR ----

func (codec) DecodeRequest(body []byte) (*ir.Request, error) {
	var req request
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("anthropic: decode request: %w", err)
	}
	out := &ir.Request{
		Model:         req.Model,
		MaxTokens:     req.MaxTokens,
		Temperature:   req.Temperature,
		TopP:          req.TopP,
		TopK:          req.TopK,
		StopSequences: req.StopSequences,
		Stream:        req.Stream,
	}
	out.System = decodeContent(req.System)
	for _, m := range req.Messages {
		out.Messages = append(out.Messages, ir.Message{Role: ir.Role(m.Role), Content: decodeContent(m.Content)})
	}
	// 托管工具定义再留一份原文：computer 的 display_*、web_fetch 的
	// max_content_tokens 等未建模声明参数，同族回写时靠 HostedRaw 整块保真。
	var rawTools struct {
		Tools []json.RawMessage `json:"tools"`
	}
	_ = json.Unmarshal(body, &rawTools)
	for i, t := range req.Tools {
		hosted := ""
		if t.Type != "" && t.Type != "custom" {
			hosted = ir.CanonicalHosted(t.Type)
		}
		ctl, ttl := decodeCacheCtl(t.CacheCtl)
		var hostedRaw json.RawMessage
		if hosted != "" && i < len(rawTools.Tools) {
			hostedRaw = rawTools.Tools[i]
		}
		out.Tools = append(out.Tools, ir.Tool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.InputSchema,
			Hosted:      hosted,
			// 原生类型名（含版本后缀）原样进 IR：同族回写要用它，硬编码默认
			// 版本会把客户端指名的版本偷换掉。
			HostedType:   t.Type,
			HostedParams: hostedParamsOf(t),
			HostedRaw:    hostedRaw,
			CacheCtl:     ctl,
			CacheTTL:     ttl,
			Strict:       t.Strict,
			// 2026 修饰四维原样进 IR（hosted 工具上也照收——出站按目标能力取舍）。
			DeferLoading:        t.DeferLoading,
			EagerInputStreaming: t.EagerInputStreaming,
			InputExamples:       t.InputExamples,
			AllowedCallers:      t.AllowedCallers,
		})
	}
	if tc := req.ToolChoice; tc != nil {
		out.ToolChoice = &ir.ToolChoice{
			Mode:            ir.ChoiceMode(tc.Type),
			ToolName:        tc.Name,
			DisableParallel: tc.DisableParallelToolUse,
		}
	}
	if req.Thinking != nil {
		out.Thinking = &ir.ThinkingConfig{
			Enabled:      req.Thinking.Type == "enabled" || req.Thinking.Type == "adaptive",
			Adaptive:     req.Thinking.Type == "adaptive",
			Display:      req.Thinking.Display,
			BudgetTokens: req.Thinking.BudgetTokens,
		}
	}
	if req.Metadata != nil && req.Metadata.UserID != "" {
		out.Metadata = map[string]string{"user_id": req.Metadata.UserID}
	}
	// output_config.format 只有 json_schema 一种 type，且恒为严格语义
	// （没有 strict 开关也没有名称位，与 gemini 的 responseSchema 同款）。
	// schema 为空按没给处理：空约束写出来上游也是自由文本，不发明诉求。
	if f := req.OutputConfig; f != nil && f.Format != nil &&
		f.Format.Type == "json_schema" && len(f.Format.Schema) > 0 && string(f.Format.Schema) != "null" {
		out.ResponseFormat = &ir.ResponseFormat{Schema: f.Format.Schema, Strict: true}
	}
	// output_config.effort 原值进 IR Thinking.Effort（值集是 OpenAI 的
	// 子集，无需翻译）。effort 独立出现也算开了思考。
	if f := req.OutputConfig; f != nil && f.Effort != "" {
		if out.Thinking == nil {
			out.Thinking = &ir.ThinkingConfig{Enabled: true}
		}
		out.Thinking.Effort = f.Effort
	}
	// 原值进 IR，跨族映射是出站的事（proto.MapServiceTier）。
	out.ServiceTier = req.ServiceTier
	// 顶层缓存便捷糖与推理地理偏好原值进 IR；不展开、不映射。
	if req.CacheControl != nil {
		out.TopCacheCtl = req.CacheControl.Type
		out.TopCacheTTL = req.CacheControl.TTL
	}
	out.InferenceGeo = req.InferenceGeo
	// container 两形态（string 简写 / {id,skills} 对象）统一进 IR。
	ct, err := decodeContainerParam(req.Container)
	if err != nil {
		return nil, err
	}
	out.Container = ct
	// beta 两参数原值进 IR：speed 是溢价计费档；mcp_servers 含凭据与
	// 嵌套工具配置，不展开建模（同 Moderation 的不透明原文口径）。
	out.Speed = req.Speed
	out.MCPServers = req.MCPServers
	return out, nil
}

// decodeContainerParam 解请求侧 container：string 简写（仅 id）或
// {id, skills} 对象。空/显式 null 都视为没给。
func decodeContainerParam(raw json.RawMessage) (*ir.Container, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var id string
	if err := json.Unmarshal(raw, &id); err == nil {
		return &ir.Container{ID: id}, nil
	}
	var p containerParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("anthropic: decode container: %w", err)
	}
	ct := &ir.Container{ID: p.ID}
	for _, s := range p.Skills {
		ct.Skills = append(ct.Skills, ir.Skill{SkillID: s.SkillID, Type: s.Type, Version: s.Version})
	}
	return ct, nil
}

// decodeContainer 响应侧容器回显进 IR。
func decodeContainer(c *container) *ir.Container {
	if c == nil {
		return nil
	}
	ct := &ir.Container{ID: c.ID, ExpiresAt: c.ExpiresAt}
	for _, s := range c.Skills {
		ct.Skills = append(ct.Skills, ir.Skill{SkillID: s.SkillID, Type: s.Type, Version: s.Version})
	}
	return ct
}

// encodeContainerInfo 响应侧回写：IR -> {id, expires_at, skills}。
func encodeContainerInfo(ct *ir.Container) *container {
	if ct == nil {
		return nil
	}
	out := &container{ID: ct.ID, ExpiresAt: ct.ExpiresAt}
	for _, s := range ct.Skills {
		out.Skills = append(out.Skills, containerSkill{SkillID: s.SkillID, Type: s.Type, Version: s.Version})
	}
	return out
}

// decodeContent 解析 Anthropic 的 content：纯字符串或 block 数组两种形态。
// system、message.content 与 tool_result.content 三处同形，共用一个实现。
func decodeContent(raw json.RawMessage) []ir.Block {
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
	return decodeBlocks(raw)
}

// decodeBlocks 逐块解析 block 数组。必须逐块而不是一次性 []block：Anthropic
// 在不同块型上复用同一个键名承载不同形状——citations 在 text 块上是引用数组、
// 在 document 块上是 {"enabled":bool}；source 在 document 块上是对象、在
// search_result 块上是字符串。一次性解析时任一块的形状冲突都会让整个
// Unmarshal 失败，同消息的其他块（包括用户真正在问的那句话）随之全部蒸发，
// 而调用方只拿到一个 nil：既没有错误，也没有损耗注记。
func decodeBlocks(raw json.RawMessage) []ir.Block {
	var raws []json.RawMessage
	if err := json.Unmarshal(raw, &raws); err != nil {
		return nil
	}
	out := make([]ir.Block, 0, len(raws))
	for _, r := range raws {
		if b, ok := decodeRawBlock(r); ok {
			out = append(out, b)
		}
	}
	return out
}

// decodeRawBlock 解析单个块。ok=false 表示这个元素根本不是 Anthropic 块
// （连 type 都读不出来），留着只会在出站时变成 {"type":""} 让上游 400。
func decodeRawBlock(raw json.RawMessage) (ir.Block, bool) {
	var b block
	if err := json.Unmarshal(raw, &b); err != nil {
		// 形状冲突：整块原样留成不透明块。丢弃它会破坏 assistant 历史里
		// server_tool_use 与结果块的配平，上游按配平校验拒整轮。
		wt := wireTypeOf(raw)
		if wt == "" {
			return ir.Block{}, false
		}
		return ir.Block{Type: ir.BlockOpaque,
			Opaque: &ir.Opaque{WireType: wt, Body: raw, From: Name}}, true
	}
	if b.Type == "" {
		return ir.Block{}, false
	}
	return decodeBlock(b, raw), true
}

func wireTypeOf(raw json.RawMessage) string {
	var head struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return ""
	}
	return head.Type
}

// decodeBlock 已知块型的解码。raw 是块的原始 JSON，供 default 分支把未知块
// 整块留成不透明块——未知块型的载荷形状由上游定义，逐字段猜必丢内容。
func decodeBlock(b block, raw json.RawMessage) ir.Block {
	out := ir.Block{}
	out.CacheCtl, out.CacheTTL = decodeCacheCtl(b.CacheCtl)
	switch b.Type {
	case "text":
		out.Type = ir.BlockText
		out.Text = b.Text
		out.Citations = decodeCitations(b.Citations)
	case "image":
		out.Type = ir.BlockImage
		if b.Source != nil {
			out.Image = &ir.Image{MediaType: b.Source.MediaType, Data: b.Source.Data, URL: b.Source.URL}
		}
	case "document":
		// source.type=content（官方 union 第四种形态，正文是字符串或 text/image
		// 块数组）归不透明块，不投进 Media：Media 只有 base64 / URL / file_id
		// 三种载体，装不下块数组。此前它落进 decodeDocument 的 default 被当
		// base64 处理——Data 取空、MIME 兜底成 application/pdf，重新编码后写出
		// 一个连 data 键都没有的 base64 PDF source：正文全丢，形状还非法
		// （官方 Base64PDFSourceParam.data 是 Required），上游直接 400。
		// 归不透明块后同族原样带回无损，外族整块丢弃并报损耗，两者都比伪造诚实。
		if b.Source != nil && b.Source.Type == "content" {
			out.Type = ir.BlockOpaque
			out.Opaque = &ir.Opaque{WireType: b.Type, Body: raw, From: Name}
			return out
		}
		out.Type = ir.BlockMedia
		out.Media = decodeDocument(b)
	case "tool_use":
		out.Type = ir.BlockToolUse
		out.ToolUse = &ir.ToolUse{ID: b.ID, Name: b.Name, Input: b.Input}
	case "tool_result":
		out.Type = ir.BlockToolResult
		out.ToolResult = &ir.ToolResult{ToolUseID: b.ToolUseID, IsError: b.IsError, Content: decodeToolResultContent(b.Content)}
	case "thinking":
		out.Type = ir.BlockThinking
		out.Thinking = &ir.Thinking{Text: b.Thinking, Signature: b.Signature, SignatureFrom: ir.SigFrom(Name, b.Signature)}
	case "redacted_thinking":
		// 不落进 default：默认分支按 text 处理，而 redacted_thinking 没有 text
		// 字段，降级过去等于把密文换成一个空文本块——客户端下一轮无从回传，
		// Anthropic 的续话校验直接拒整个请求。
		out.Type = ir.BlockRedactedThinking
		out.RedactedData = b.Data
	case "server_tool_use":
		out.Type = ir.BlockServerToolUse
		out.ServerToolUse = &ir.ServerToolUse{ID: b.ID, Name: b.Name, Input: b.Input}
	case "web_search_tool_result":
		out.Type = ir.BlockWebSearchToolResult
		out.WebSearchToolResult = decodeWebSearchToolResult(b.ToolUseID, b.Caller, b.Content)
	case "container_upload":
		out.Type = ir.BlockContainerUpload
		out.ContainerUpload = &ir.ContainerUploadRef{FileID: b.FileID}
	default:
		// 未知块原样保留，不降级成文本。Anthropic 的服务端工具结果块
		// （web_fetch / code_execution / bash_code_execution /
		// text_editor_code_execution / tool_search）根本没有 text 字段，
		// 降级过去等于把抓取的网页正文、stdout、文件内容换成一个空文本块，
		// 而兄弟 server_tool_use 块还留在原地——发给上游的
		// tool_use/tool_result 配平当场断裂，下一轮可能被整轮拒掉。
		out.Type = ir.BlockOpaque
		out.Opaque = &ir.Opaque{WireType: b.Type, Body: raw, From: Name}
	}
	return out
}

// decodeDocument 解 document 块的 source 形态。source.type=content 不在此列，
// 调用方已把它归成不透明块（Media 装不下块数组，见 decodeBlock）。
// source.type=text 是内联纯文本，MIME 缺省按官方的 text/plain；其余形态缺省
// application/pdf（对齐 cc-switch transform_responses.rs 的同款兜底）。
// 缺省值不能留空：MIME 为空会让 MediaKindOf 判成 other，转出时投错槽位。
func decodeDocument(b block) *ir.Media {
	m := &ir.Media{
		Filename:         b.Title,
		Context:          b.Context,
		CitationsEnabled: decodeCitationsConfig(b.Citations),
	}
	if b.Source == nil {
		m.Kind = ir.MediaDocument
		m.MediaType = "application/pdf"
		return m
	}
	m.MediaType = b.Source.MediaType
	switch b.Source.Type {
	case "text":
		if m.MediaType == "" {
			m.MediaType = "text/plain"
		}
		m.Data = b.Source.Data
	case "url":
		m.URL = b.Source.URL
	case "file":
		m.FileID = b.Source.FileID
	default: // base64
		m.Data = b.Source.Data
	}
	if m.MediaType == "" {
		m.MediaType = "application/pdf"
	}
	m.Kind = ir.MediaKindOf(m.MediaType)
	return m
}

// decodeToolResultContent tool_result.content：与 message.content 同形，但空字符串
// 要留成一个空文本块而不是 nil——tool_result 没有内容会让上游认为工具没返回。
func decodeToolResultContent(raw json.RawMessage) []ir.Block {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []ir.Block{{Type: ir.BlockText, Text: s}}
	}
	return decodeBlocks(raw)
}

// decodeWebSearchToolResult web_search_tool_result.content -> IR 结果。
// content 是 union：错误形态是单个对象（web_search_tool_result_error），
// 结果形态是子块数组。只按数组解会把错误对象解成零条结果——「搜索失败」
// 被伪造成「搜索成功但没找到东西」，两种语义对客户端完全不同。
func decodeWebSearchToolResult(toolUseID string, caller, raw json.RawMessage) *ir.WebSearchToolResult {
	out := &ir.WebSearchToolResult{ToolUseID: toolUseID, Caller: caller}
	if len(raw) == 0 {
		return out
	}
	var eb webSearchToolErrorBlock
	if json.Unmarshal(raw, &eb) == nil && eb.ErrorCode != "" {
		out.ErrorCode = eb.ErrorCode
		return out
	}
	var rs []webSearchResultBlock
	if err := json.Unmarshal(raw, &rs); err != nil {
		rs = nil
	}
	for _, r := range rs {
		out.Results = append(out.Results, ir.WebSearchResult{
			Title: r.Title, URL: r.URL, Snippet: r.EncryptedContent, PageAge: r.PageAge,
		})
	}
	return out
}

func decodeCacheCtl(c *cacheControl) (string, string) {
	if c == nil {
		return "", ""
	}
	return c.Type, c.TTL
}

// ---- 请求编码：IR -> Anthropic ----

// defaultMaxTokens 对齐 sub2api 的缺省值；Anthropic 强制要求 max_tokens。
const defaultMaxTokens = 8192

// nativeHosted 规范托管工具种类 -> Anthropic 带版本的 type 与固定 name。
// 未识别种类原样作为 type 透传（同协议往返场景）。
func nativeHosted(canonical string) (typ, name string, known bool) {
	switch canonical {
	case ir.HostedWebSearch:
		return "web_search_20250305", "web_search", true
	case ir.HostedCodeExecution:
		return "code_execution_20250522", "code_execution", true
	default:
		return canonical, "", false
	}
}

// hostedParamsOf 把托管工具的声明参数收进 IR；一个都没给时保持 nil，
// 同族回写一个键也不造（缺省保持缺省）。
func hostedParamsOf(t tool) *ir.HostedParams {
	if t.MaxUses == 0 && len(t.AllowedDomains) == 0 && len(t.BlockedDomains) == 0 && len(t.UserLocation) == 0 {
		return nil
	}
	return &ir.HostedParams{
		MaxUses:        t.MaxUses,
		AllowedDomains: t.AllowedDomains,
		BlockedDomains: t.BlockedDomains,
		UserLocation:   t.UserLocation,
	}
}

// degradeThinking 把历史消息中无法通过 Anthropic 签名校验的 thinking 块
// （无签名或外族形态签名）降级为 text 块——Anthropic 对历史 thinking 块
// 强制签名校验，透传必 400，宁可断签名链保住请求。
// 仅用于请求方向（EncodeRequest），响应方向不降级。
func degradeThinking(blocks []ir.Block) {
	for i := range blocks {
		b := &blocks[i]
		if b.Type != ir.BlockThinking || b.Thinking == nil {
			continue
		}
		if b.Thinking.SignatureGenuineFor(Name) {
			continue
		}
		blocks[i] = ir.Block{Type: ir.BlockText, Text: b.Thinking.Text}
	}
}

func (codec) EncodeRequest(req *ir.Request) ([]byte, error) {
	r := req.Clone()
	if err := normalize.Request(r, normalize.Strict()); err != nil {
		return nil, err
	}
	for i := range r.Messages {
		degradeThinking(r.Messages[i].Content)
	}
	out := request{
		Model:         r.Model,
		MaxTokens:     r.MaxTokens,
		Temperature:   r.Temperature,
		TopP:          r.TopP,
		TopK:          r.TopK,
		StopSequences: r.StopSequences,
		Stream:        r.Stream,
	}
	if out.MaxTokens <= 0 {
		out.MaxTokens = defaultMaxTokens
	}
	for _, m := range r.Messages {
		blocks := encodeBlocks(m.Content)
		if len(blocks) == 0 {
			// 整条消息的块全被编码器丢掉了（唯一的内容是外族来源的不透明块）：
			// Anthropic 要求 content 非空，空数组直接 400 拒整轮。规整流水线补不上
			// 这个占位——它跑在编码之前，那会儿这条消息看起来还是有内容的。
			// 丢了什么由 relay.Diagnose 报出，占位文本本身不假装是用户的内容。
			blocks = []block{{Type: "text", Text: normalize.Placeholder}}
		}
		out.Messages = append(out.Messages, message{Role: string(m.Role), Content: marshal(blocks)})
	}
	if len(r.System) > 0 {
		out.System = marshal(encodeBlocks(r.System))
	}
	for _, t := range r.Tools {
		if t.Hosted != "" {
			// 原生类型名只在同族来路时可信；外族原名（responses 的
			// web_search_preview、gemini 的 google_search）写进 anthropic 的
			// type 是必 400 的形状，回落默认带版本名。
			typ, name, known := nativeHosted(t.Hosted)
			sameNative := t.HostedType != "" && ir.HostedTypeFamily(t.HostedType) == Name
			if sameNative {
				typ = t.HostedType
			}
			if !sameNative && !known {
				// 未识别种类且没有同族原生名可用：canonical（=外族原名）原样写进
				// type 是上游必 400 的形状，整块丢弃比拒掉整轮诚实
				// （Diagnose 已按 no cross-protocol mapping 报出）。
				continue
			}
			if sameNative && len(t.HostedRaw) > 0 {
				// 同族来路且有原文：整块回吐——未建模的声明参数（computer 的
				// display_*、web_fetch 的 max_content_tokens）建模跟进永远慢半拍，
				// 逐字段重建必丢未来新增键。
				out.Tools = append(out.Tools, tool{Raw: t.HostedRaw})
				continue
			}
			// 固定名只在种类未识别时才让位给 IR 里的名字：web_search 一族的
			// name 上游写死校验（必须 "web_search"），外族来路的原生名
			// （gemini 的 google_search）盖上去是必 400 的形状。
			if name == "" {
				name = t.Name
			}
			ht := tool{
				Type:                typ,
				Name:                name,
				CacheCtl:            encodeCacheCtl(t.CacheCtl, t.CacheTTL),
				Strict:              t.Strict,
				DeferLoading:        t.DeferLoading,
				EagerInputStreaming: t.EagerInputStreaming,
				AllowedCallers:      t.AllowedCallers,
			}
			if p := t.HostedParams; p != nil {
				ht.MaxUses = p.MaxUses
				ht.AllowedDomains = p.AllowedDomains
				ht.BlockedDomains = p.BlockedDomains
				ht.UserLocation = p.UserLocation
			}
			out.Tools = append(out.Tools, ht)
			continue
		}
		out.Tools = append(out.Tools, tool{
			Name:                t.Name,
			Description:         t.Description,
			InputSchema:         t.InputSchema,
			CacheCtl:            encodeCacheCtl(t.CacheCtl, t.CacheTTL),
			Strict:              t.Strict,
			DeferLoading:        t.DeferLoading,
			EagerInputStreaming: t.EagerInputStreaming,
			InputExamples:       t.InputExamples,
			AllowedCallers:      t.AllowedCallers,
		})
	}
	if tc := r.ToolChoice; tc != nil {
		out.ToolChoice = &toolChoice{
			Type:                   string(tc.Mode),
			Name:                   tc.ToolName,
			DisableParallelToolUse: tc.DisableParallel,
		}
	}
	if r.Thinking != nil && r.Thinking.Enabled {
		if r.Thinking.Adaptive {
			// adaptive 是官方推荐的现代形态（enabled 已废弃）：模型自主
			// 决定思考量，不带预算；display 仅在本族有意义。
			out.Thinking = &thinkingCfg{Type: "adaptive", Display: r.Thinking.Display}
		} else {
			budget := r.Thinking.BudgetTokens
			if budget <= 0 {
				budget = 4096
			}
			// Anthropic 约束 budget_tokens < max_tokens：账号级覆盖的强制预算
			// 可能与客户端 max_tokens 冲突，越界时夹紧（夹紧到 0 则 omitempty 丢弃）。
			if budget >= out.MaxTokens {
				budget = out.MaxTokens - 1
			}
			out.Thinking = &thinkingCfg{Type: "enabled", BudgetTokens: budget, Display: r.Thinking.Display}
		}
	}
	uid := r.Metadata["user_id"]
	if uid == "" {
		// safety_identifier 与 user 同一维度（滥用检测标识）：user_id 槽空着
		// 时映进去；两边都有时 user 优先，safety identifier 由诊断报出。
		uid = r.SafetyIdentifier
	}
	if uid != "" {
		out.Metadata = &metadata{UserID: uid}
	}
	// 只回写 schema 约束形态：纯 JSON 模式（没给 schema）在 anthropic 没有
	// 对应物，写出来上游也读不懂——那一档由诊断报出（SchemaOnly 位）。
	if r.ResponseFormat != nil && r.ResponseFormat.IsSchema() {
		out.OutputConfig = &outputConfig{Format: &jsonOutputFormat{
			Type: "json_schema", Schema: r.ResponseFormat.Schema}}
	}
	// effort 在 anthropic 是封闭五值集（low/medium/high/xhigh/max，
	// 没有 none/minimal）：装不下的档位丢弃，由诊断报出；"none" 与
	// 未开思考同义，静默即可。
	if r.Thinking != nil {
		switch r.Thinking.Effort {
		case "low", "medium", "high", "xhigh", "max":
			if out.OutputConfig == nil {
				out.OutputConfig = &outputConfig{}
			}
			out.OutputConfig.Effort = r.Thinking.Effort
		}
	}
	// 值集装不下的档位（flex/scale/priority/fast 等）丢弃，由诊断报出。
	if tier, ok := proto.MapServiceTier(r.ServiceTier, Name); ok {
		out.ServiceTier = tier
	}
	if r.TopCacheCtl != "" {
		out.CacheControl = &cacheControl{Type: r.TopCacheCtl, TTL: r.TopCacheTTL}
	}
	out.InferenceGeo = r.InferenceGeo
	// container 回写：仅 id 无技能时用 string 简写形态（官方简写与对象
	// {id} 无 skills 语义等价，取最简）；带技能时用对象形态。
	if r.Container != nil {
		if len(r.Container.Skills) == 0 {
			out.Container, _ = json.Marshal(r.Container.ID)
		} else {
			p := containerParams{ID: r.Container.ID}
			for _, s := range r.Container.Skills {
				p.Skills = append(p.Skills, containerSkill{SkillID: s.SkillID, Type: s.Type, Version: s.Version})
			}
			out.Container, _ = json.Marshal(p)
		}
	}
	out.Speed = r.Speed
	out.MCPServers = r.MCPServers
	return json.Marshal(out)
}

func encodeBlocks(bs []ir.Block) []block {
	out := make([]block, 0, len(bs))
	for _, b := range bs {
		if !encodableBlock(b) {
			continue
		}
		out = append(out, encodeBlock(b))
	}
	return out
}

// encodableBlock 报告该块能否写进 Anthropic 的线上。两类被拦下。
//
// 一是外族来源的不透明块（OpenAI 两系客户端发来的未知 content part）：逐字写回是
// 一个 Anthropic 不认识的块型，上游按块型校验直接 400 拒整轮，那是比丢内容更糟的
// 结果；降级成文本又是把别家的 part 载荷涂进正文。
//
// 二是没有载荷的图片块：官方 image source 只有 base64（media_type 与 data 都是
// Required）与 url 两种，两者皆空时编出来是
// {"type":"image","source":{"type":"base64"}}——一个连必填键都没有的形状，同样
// 400 拒整轮。常见来源是 Responses 客户端只给了 file_id，而 Anthropic 没有
// 「引用上游文件服务里的图片」这一维。
//
// 整块跳过，损耗由 relay.Diagnose 报出。
func encodableBlock(b ir.Block) bool {
	if b.Type == ir.BlockImage {
		return b.Image.HasPayload()
	}
	return b.Type != ir.BlockOpaque || proto.OpaqueVerbatimFor(b.Opaque, Name)
}

// encodeMedia 把媒体块写成 document，装不下的大类降级为占位文本。
// Anthropic 只有 document 一个附件槽位，音频与视频没有任何入口：写进 document
// 会被上游按 PDF 解析而 400，静默丢掉则让模型以为用户没给附件。占位文本是
// 唯一两者都不发生的形态（参考 cc-switch 的 UNSUPPORTED_IMAGE_MARKER 做法）。
func encodeMedia(b ir.Block, out block) block {
	if b.Media == nil || b.Media.Kind != ir.MediaDocument {
		out.Type = "text"
		out.Text = b.Media.Describe()
		return out
	}
	m := b.Media
	out.Type = "document"
	out.Title = m.Filename
	out.Context = m.Context
	if m.CitationsEnabled != nil {
		// 客户端的引用开关原样带回。丢了它上游按默认（不出引用）处理，
		// 客户端明明开了文档引用却一条都收不到，且没有任何注记可循。
		out.Citations = marshal(citationsConfig{Enabled: *m.CitationsEnabled})
	}
	switch {
	case m.FileID != "":
		out.Source = &mediaSource{Type: "file", FileID: m.FileID}
	case m.URL != "":
		out.Source = &mediaSource{Type: "url", URL: m.URL}
	case m.MediaType == "text/plain":
		// 纯文本走 source.type=text：塞进 base64 槽位需要先编码，而上游对
		// text/plain 只接受 text 形态。
		out.Source = &mediaSource{Type: "text", MediaType: m.MediaType, Data: m.Data}
	default:
		out.Source = &mediaSource{Type: "base64", MediaType: m.MediaType, Data: m.Data}
	}
	return out
}

func encodeBlock(b ir.Block) block {
	out := block{CacheCtl: encodeCacheCtl(b.CacheCtl, b.CacheTTL)}
	switch b.Type {
	case ir.BlockText:
		out.Type = "text"
		out.Text = b.Text
		if cs, _ := encodeCitations(b.Text, b.Citations); len(cs) > 0 {
			out.Citations = marshal(cs)
		}
	case ir.BlockImage:
		out.Type = "image"
		if b.Image != nil {
			if b.Image.URL != "" {
				out.Source = &mediaSource{Type: "url", URL: b.Image.URL}
			} else {
				out.Source = &mediaSource{Type: "base64", MediaType: b.Image.MediaType, Data: b.Image.Data}
			}
		}
	case ir.BlockMedia:
		return encodeMedia(b, out)
	case ir.BlockToolUse:
		out.Type = "tool_use"
		if b.ToolUse != nil {
			out.ID = b.ToolUse.ID
			out.Name = b.ToolUse.Name
			out.Input = b.ToolUse.ObjectInput()
		}
	case ir.BlockToolResult:
		out.Type = "tool_result"
		if b.ToolResult != nil {
			out.ToolUseID = b.ToolResult.ToolUseID
			out.IsError = b.ToolResult.IsError
			out.Content = marshal(encodeBlocks(b.ToolResult.Content))
		}
	case ir.BlockThinking:
		out.Type = "thinking"
		if b.Thinking != nil {
			out.Thinking = b.Thinking.Text
			// 只回本族真签名。外族签名与本代理造的占位签名写进这一格就是冒充：
			// 客户端会把它当账号绑定的真签名在下一轮回传，上游校验必拒。
			if b.Thinking.SignatureGenuineFor(Name) {
				out.Signature = b.Thinking.Signature
			}
		}
	case ir.BlockRedactedThinking:
		out.Type = "redacted_thinking"
		out.Data = b.RedactedData
	case ir.BlockServerToolUse:
		out.Type = "server_tool_use"
		if b.ServerToolUse != nil {
			out.ID = b.ServerToolUse.ID
			out.Name = b.ServerToolUse.Name
			out.Input = b.ServerToolUse.Input
			if len(out.Input) == 0 {
				out.Input = json.RawMessage(`{}`)
			}
		}
	case ir.BlockWebSearchToolResult:
		out.Type = "web_search_tool_result"
		if b.WebSearchToolResult != nil {
			out.ToolUseID = b.WebSearchToolResult.ToolUseID
			out.Caller = b.WebSearchToolResult.Caller
			if b.WebSearchToolResult.ErrorCode != "" {
				out.Content = marshal(webSearchToolErrorBlock{
					Type: "web_search_tool_result_error", ErrorCode: b.WebSearchToolResult.ErrorCode,
				})
			} else {
				rs := make([]webSearchResultBlock, 0, len(b.WebSearchToolResult.Results))
				for _, r := range b.WebSearchToolResult.Results {
					rs = append(rs, webSearchResultBlock{
						Type: "web_search_result", Title: r.Title, URL: r.URL, EncryptedContent: r.Snippet,
						PageAge: r.PageAge,
					})
				}
				out.Content = marshal(rs)
			}
		}
	case ir.BlockContainerUpload:
		out.Type = "container_upload"
		if b.ContainerUpload != nil {
			out.FileID = b.ContainerUpload.FileID
		}
	case ir.BlockOpaque:
		// 整块原样写回。逐字段重建会丢掉 block 没建模的键，而这些块
		// （web_fetch_tool_result 的 caller、search_result 的 source/citations）
		// 的回传契约要求原样带回。
		if b.Opaque != nil {
			out.Type = b.Opaque.WireType
			out.OpaqueRaw = b.Opaque.Body
		}
	default:
		// BlockRefusal 也落这里：Anthropic 没有 refusal 槽位，降级为文本而不是
		// 丢弃——拒绝正文是模型真正说出的话，丢了客户端只剩空消息配
		// stop_reason=refusal。不加标注前缀，那会污染正文。
		out.Type = "text"
		out.Text = b.Text
	}
	return out
}

func encodeCacheCtl(s, ttl string) *cacheControl {
	if s == "" {
		return nil
	}
	return &cacheControl{Type: s, TTL: ttl}
}

// ---- 错误渲染 ----

func (codec) RenderError(e *ir.Error) (int, []byte) {
	return e.HTTPStatus(), marshal(errorResponse{
		Type:  "error",
		Error: errorBody{Type: e.Type, Message: e.Message},
	})
}

func (codec) RenderStreamError(e *ir.Error) []byte {
	return sseFrame("error", marshal(streamEvent{
		Type:  "error",
		Error: &errorBody{Type: e.Type, Message: e.Message},
	}))
}

// sseFrame 生成 Anthropic 风格的 event:+data: 双行帧。
func sseFrame(event string, data []byte) []byte {
	return []byte("event: " + event + "\ndata: " + string(data) + "\n\n")
}
