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
	out.System = decodeSystem(req.System)
	for _, m := range req.Messages {
		out.Messages = append(out.Messages, ir.Message{Role: ir.Role(m.Role), Content: decodeContent(m.Content)})
	}
	for _, t := range req.Tools {
		hosted := ""
		if t.Type != "" && t.Type != "custom" {
			hosted = ir.CanonicalHosted(t.Type)
		}
		ctl, ttl := decodeCacheCtl(t.CacheCtl)
		out.Tools = append(out.Tools, ir.Tool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.InputSchema,
			Hosted:      hosted,
			CacheCtl:    ctl,
			CacheTTL:    ttl,
			Strict:      t.Strict,
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

func decodeSystem(raw json.RawMessage) []ir.Block {
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
	var blocks []block
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil
	}
	return decodeBlocks(blocks)
}

// decodeContent 消息 content：Anthropic 允许纯字符串或 block 数组两种形态。
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
	var blocks []block
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil
	}
	return decodeBlocks(blocks)
}

func decodeBlocks(bs []block) []ir.Block {
	out := make([]ir.Block, 0, len(bs))
	for _, b := range bs {
		out = append(out, decodeBlock(b))
	}
	return out
}

func decodeBlock(b block) ir.Block {
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
		out.WebSearchToolResult = decodeWebSearchToolResult(b.ToolUseID, b.Content)
	case "container_upload":
		out.Type = ir.BlockContainerUpload
		out.ContainerUpload = &ir.ContainerUploadRef{FileID: b.FileID}
	default:
		// 未知块降级为文本，保证不丢信息
		out.Type = ir.BlockText
		out.Text = b.Text
	}
	return out
}

// decodeDocument 解 document 块的四种 source 形态。
// source.type=text 是内联纯文本，MIME 缺省按官方的 text/plain；其余形态缺省
// application/pdf（对齐 cc-switch transform_responses.rs 的同款兜底）。
// 缺省值不能留空：MIME 为空会让 MediaKindOf 判成 other，转出时投错槽位。
func decodeDocument(b block) *ir.Media {
	m := &ir.Media{Filename: b.Title}
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

func decodeToolResultContent(raw json.RawMessage) []ir.Block {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []ir.Block{{Type: ir.BlockText, Text: s}}
	}
	var blocks []block
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil
	}
	return decodeBlocks(blocks)
}

// decodeWebSearchToolResult web_search_tool_result.content 子块数组 -> IR 结果。
func decodeWebSearchToolResult(toolUseID string, raw json.RawMessage) *ir.WebSearchToolResult {
	var rs []webSearchResultBlock
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &rs); err != nil {
			rs = nil
		}
	}
	out := &ir.WebSearchToolResult{ToolUseID: toolUseID}
	for _, r := range rs {
		out.Results = append(out.Results, ir.WebSearchResult{
			Title: r.Title, URL: r.URL, Snippet: r.EncryptedContent,
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
func nativeHosted(canonical string) (typ, name string) {
	switch canonical {
	case ir.HostedWebSearch:
		return "web_search_20250305", "web_search"
	case ir.HostedCodeExecution:
		return "code_execution_20250522", "code_execution"
	default:
		return canonical, ""
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
		out.Messages = append(out.Messages, message{Role: string(m.Role), Content: marshal(encodeBlocks(m.Content))})
	}
	if len(r.System) > 0 {
		out.System = marshal(encodeBlocks(r.System))
	}
	for _, t := range r.Tools {
		if t.Hosted != "" {
			typ, name := nativeHosted(t.Hosted)
			if name == "" {
				name = t.Name
			}
			out.Tools = append(out.Tools, tool{Type: typ, Name: name})
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
	return json.Marshal(out)
}

func encodeBlocks(bs []ir.Block) []block {
	out := make([]block, 0, len(bs))
	for _, b := range bs {
		out = append(out, encodeBlock(b))
	}
	return out
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
		out.Citations = encodeCitations(b.Text, b.Citations)
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
			rs := make([]webSearchResultBlock, 0, len(b.WebSearchToolResult.Results))
			for _, r := range b.WebSearchToolResult.Results {
				rs = append(rs, webSearchResultBlock{
					Type: "web_search_result", Title: r.Title, URL: r.URL, EncryptedContent: r.Snippet,
				})
			}
			out.Content = marshal(rs)
		}
	case ir.BlockContainerUpload:
		out.Type = "container_upload"
		if b.ContainerUpload != nil {
			out.FileID = b.ContainerUpload.FileID
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
