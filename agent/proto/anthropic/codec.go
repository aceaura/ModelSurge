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
		// 载荷里没有 response_format 之类的字段。
		StructuredOutput: false,
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
		out.Tools = append(out.Tools, ir.Tool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.InputSchema,
			Hosted:      hosted,
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
			Enabled:      req.Thinking.Type == "enabled",
			BudgetTokens: req.Thinking.BudgetTokens,
		}
	}
	if req.Metadata != nil && req.Metadata.UserID != "" {
		out.Metadata = map[string]string{"user_id": req.Metadata.UserID}
	}
	return out, nil
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
	out := ir.Block{CacheCtl: cacheCtlString(b.CacheCtl)}
	switch b.Type {
	case "text":
		out.Type = ir.BlockText
		out.Text = b.Text
	case "image":
		out.Type = ir.BlockImage
		if b.Source != nil {
			out.Image = &ir.Image{MediaType: b.Source.MediaType, Data: b.Source.Data, URL: b.Source.URL}
		}
	case "tool_use":
		out.Type = ir.BlockToolUse
		out.ToolUse = &ir.ToolUse{ID: b.ID, Name: b.Name, Input: b.Input}
	case "tool_result":
		out.Type = ir.BlockToolResult
		out.ToolResult = &ir.ToolResult{ToolUseID: b.ToolUseID, IsError: b.IsError, Content: decodeToolResultContent(b.Content)}
	case "thinking":
		out.Type = ir.BlockThinking
		out.Thinking = &ir.Thinking{Text: b.Thinking, Signature: b.Signature, SignatureFrom: ir.SigFrom(Name, b.Signature)}
	case "server_tool_use":
		out.Type = ir.BlockServerToolUse
		out.ServerToolUse = &ir.ServerToolUse{ID: b.ID, Name: b.Name, Input: b.Input}
	case "web_search_tool_result":
		out.Type = ir.BlockWebSearchToolResult
		out.WebSearchToolResult = decodeWebSearchToolResult(b.ToolUseID, b.Content)
	default:
		// 未知块降级为文本，保证不丢信息
		out.Type = ir.BlockText
		out.Text = b.Text
	}
	return out
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

func cacheCtlString(c *cacheControl) string {
	if c == nil {
		return ""
	}
	return c.Type
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
		if b.Thinking.Signature != "" && b.Thinking.SignatureFrom == Name {
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
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.InputSchema,
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
		budget := r.Thinking.BudgetTokens
		if budget <= 0 {
			budget = 4096
		}
		// Anthropic 约束 budget_tokens < max_tokens：账号级覆盖的强制预算
		// 可能与客户端 max_tokens 冲突，越界时夹紧（夹紧到 0 则 omitempty 丢弃）。
		if budget >= out.MaxTokens {
			budget = out.MaxTokens - 1
		}
		out.Thinking = &thinkingCfg{Type: "enabled", BudgetTokens: budget}
	}
	if uid := r.Metadata["user_id"]; uid != "" {
		out.Metadata = &metadata{UserID: uid}
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

func encodeBlock(b ir.Block) block {
	out := block{CacheCtl: encodeCacheCtl(b.CacheCtl)}
	switch b.Type {
	case ir.BlockText:
		out.Type = "text"
		out.Text = b.Text
	case ir.BlockImage:
		out.Type = "image"
		if b.Image != nil {
			if b.Image.URL != "" {
				out.Source = &imageSource{Type: "url", URL: b.Image.URL}
			} else {
				out.Source = &imageSource{Type: "base64", MediaType: b.Image.MediaType, Data: b.Image.Data}
			}
		}
	case ir.BlockToolUse:
		out.Type = "tool_use"
		if b.ToolUse != nil {
			out.ID = b.ToolUse.ID
			out.Name = b.ToolUse.Name
			out.Input = b.ToolUse.Input
			if len(out.Input) == 0 {
				out.Input = json.RawMessage(`{}`)
			}
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
			out.Signature = b.Thinking.Signature
		}
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
	default:
		out.Type = "text"
		out.Text = b.Text
	}
	return out
}

func encodeCacheCtl(s string) *cacheControl {
	if s == "" {
		return nil
	}
	return &cacheControl{Type: s}
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
