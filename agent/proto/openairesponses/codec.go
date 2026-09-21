package openairesponses

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

// Caps Responses：encrypted_content 签名、图片、hosted tools 均支持；
// 思考模式下强制 tool_choice 亦支持。
func (codec) Caps() proto.Capabilities {
	return proto.Capabilities{
		ThinkingSignature: true, Images: true, HostedTools: true, ThinkingForcedToolChoice: true,
		// TopK 留假：Responses 协议原生没有这一维。
		ImageURLs: true, Sampling: true, TopK: false, ParallelToolCalls: true,
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
		out.Tools = append(out.Tools, ir.Tool{Name: t.Name, Description: t.Description, InputSchema: t.Parameters})
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
	return out, nil
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
			out = append(out, ir.Block{Type: ir.BlockText, Text: p.Text})
		case "input_image":
			out = append(out, ir.Block{Type: ir.BlockImage, Image: parseImageURL(p.ImageURL)})
		}
	}
	return out
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
	}
	out.Instructions = joinSystem(r.System)
	var items []inputItem
	for _, m := range r.Messages {
		items = append(items, encodeMessageItems(m)...)
	}
	if len(items) > 0 {
		out.Input = marshal(items)
	}
	for _, t := range r.Tools {
		if t.Hosted != "" {
			out.Tools = append(out.Tools, tool{Type: nativeHosted(t.Hosted)})
			continue
		}
		out.Tools = append(out.Tools, tool{Type: "function", Name: t.Name, Description: t.Description, Parameters: t.InputSchema})
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
	// 订阅端点（Codex 形态）要求 store=false；对官方 API 无害
	f := false
	out.Store = &f
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
// tool_result 内嵌的图片提取为独立 user message（function_call_output 不能挂图片）。
func encodeMessageItems(m ir.Message) []inputItem {
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
				parts = append(parts, contentPart{Type: "output_text", Text: b.Text})
			case ir.BlockThinking:
				// 仅本族形态签名可还原 reasoning item；无签名或外族签名
				// 无法构造合法 item（OpenAI 会拒绝），跳过。
				if b.Thinking != nil && b.Thinking.Signature != "" && b.Thinking.SignatureFrom == Name {
					flush()
					out = append(out, inputItem{
						Type:             "reasoning",
						Summary:          marshal([]summaryPart{{Type: "summary_text", Text: b.Thinking.Text}}),
						EncryptedContent: b.Thinking.Signature,
					})
				}
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

// splitToolResultContent 拆出文本与图片（图片转成 input_image parts）。
func splitToolResultContent(blocks []ir.Block) (string, []contentPart) {
	var sb strings.Builder
	var images []contentPart
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
				images = append(images, contentPart{Type: "input_image", ImageURL: url})
			}
		}
	}
	return sb.String(), images
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
	status := e.StatusCode
	if status == 0 {
		status = 500
	}
	return status, marshal(errorResponse{Error: errorBody{Code: e.Code, Type: e.Type, Message: e.Message}})
}

func (codec) RenderStreamError(e *ir.Error) []byte {
	return []byte("data: " + string(marshal(streamEvent{Type: "error", Response: &responseObj{Error: &errorBody{Code: e.Code, Type: e.Type, Message: e.Message}}})) + "\n\n")
}
