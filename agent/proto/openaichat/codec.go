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

// Caps Chat Completions：reasoning_content 无签名机制，也无 hosted tools。
func (codec) Caps() proto.Capabilities {
	return proto.Capabilities{ThinkingSignature: false, Images: true, HostedTools: false}
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
	default:
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
		Model:       req.Model,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		Stream:      req.Stream,
	}
	out.MaxTokens = req.MaxCompletionTokens
	if out.MaxTokens == 0 {
		out.MaxTokens = req.MaxTokens
	}
	out.StopSequences = decodeStop(req.Stop)
	for _, m := range req.Messages {
		decodeMessage(out, m)
	}
	for _, t := range req.Tools {
		out.Tools = append(out.Tools, ir.Tool{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			InputSchema: t.Function.Parameters,
		})
	}
	out.ToolChoice = decodeToolChoice(req.ToolChoice)
	if req.ReasoningEffort != "" {
		out.Thinking = &ir.ThinkingConfig{Enabled: req.ReasoningEffort != "none", Effort: req.ReasoningEffort}
	}
	return out, nil
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
		req.Messages = append(req.Messages, ir.Message{Role: ir.RoleUser, Content: contentBlocks(m.Content)})
	case "assistant":
		msg := ir.Message{Role: ir.RoleAssistant, Content: contentBlocks(m.Content)}
		if m.ReasoningContent != "" {
			msg.Content = append([]ir.Block{{Type: ir.BlockThinking, Thinking: &ir.Thinking{Text: m.ReasoningContent}}}, msg.Content...)
		}
		for _, tc := range m.ToolCalls {
			msg.Content = append(msg.Content, ir.Block{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{
				ID:    tc.ID,
				Name:  tc.Function.Name,
				Input: json.RawMessage(tc.Function.Arguments),
			}})
		}
		req.Messages = append(req.Messages, msg)
	case "tool":
		req.Messages = append(req.Messages, ir.Message{Role: ir.RoleUser, Content: []ir.Block{{
			Type: ir.BlockToolResult,
			ToolResult: &ir.ToolResult{
				ToolUseID: m.ToolCallID,
				Content:   contentBlocks(m.Content),
			},
		}}})
	default: // function 等未知角色归一为 user
		req.Messages = append(req.Messages, ir.Message{Role: ir.RoleUser, Content: contentBlocks(m.Content)})
	}
}

// contentBlocks 解析 content（string 或 []part）为 IR 块。
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
	var parts []part
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil
	}
	out := make([]ir.Block, 0, len(parts))
	for _, p := range parts {
		switch p.Type {
		case "text":
			out = append(out, ir.Block{Type: ir.BlockText, Text: p.Text})
		case "image_url":
			if p.ImageURL != nil {
				out = append(out, ir.Block{Type: ir.BlockImage, Image: parseImageURL(p.ImageURL.URL)})
			}
		}
	}
	return out
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
		if fn, ok := tc["function"].(map[string]any); ok {
			if name, ok := fn["name"].(string); ok {
				return &ir.ToolChoice{Mode: ir.ChoiceTool, ToolName: name}
			}
		}
	}
	return nil
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
		Model:       r.Model,
		MaxTokens:   r.MaxTokens,
		Temperature: r.Temperature,
		TopP:        r.TopP,
		Stream:      r.Stream,
	}
	if len(r.StopSequences) == 1 {
		out.Stop = r.StopSequences[0]
	} else if len(r.StopSequences) > 1 {
		out.Stop = r.StopSequences
	}
	if r.Stream {
		out.StreamOptions = &streamOptions{IncludeUsage: true}
	}
	if sys := joinSystem(r.System); sys != "" {
		out.Messages = append(out.Messages, message{Role: "system", Content: json.RawMessage(marshalString(sys))})
	}
	for _, m := range r.Messages {
		out.Messages = append(out.Messages, encodeMessages(m)...)
	}
	for _, t := range r.Tools {
		if t.Hosted != "" {
			continue // Chat Completions 无托管工具能力，丢弃（diagnose 已记录）
		}
		out.Tools = append(out.Tools, tool{Type: "function", Function: toolFunc{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  t.InputSchema,
		}})
	}
	out.ToolChoice = encodeToolChoice(r.ToolChoice)
	if r.Thinking != nil && r.Thinking.Enabled {
		effort := r.Thinking.Effort
		if effort == "" {
			effort = "medium"
		}
		out.ReasoningEffort = effort
	}
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
		msg := message{Role: "assistant"}
		var text string
		for _, b := range m.Content {
			switch b.Type {
			case ir.BlockText:
				text += b.Text
			case ir.BlockThinking:
				if b.Thinking != nil {
					msg.ReasoningContent += b.Thinking.Text
				}
			case ir.BlockToolUse:
				if b.ToolUse != nil {
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
				out = append(out, message{Role: "user", Content: json.RawMessage(marshalString(parts[0].Text))})
			} else {
				out = append(out, message{Role: "user", Content: marshal(parts)})
			}
			parts = nil
		}
		for _, b := range m.Content {
			switch b.Type {
			case ir.BlockText:
				parts = append(parts, part{Type: "text", Text: b.Text})
			case ir.BlockImage:
				if b.Image != nil {
					url := b.Image.URL
					if url == "" && b.Image.Data != "" {
						url = "data:" + b.Image.MediaType + ";base64," + b.Image.Data
					}
					parts = append(parts, part{Type: "image_url", ImageURL: &imageURL{URL: url}})
				}
			case ir.BlockToolResult:
				flush()
				if b.ToolResult != nil {
					out = append(out, message{
						Role:       "tool",
						ToolCallID: b.ToolResult.ToolUseID,
						Content:    json.RawMessage(marshalString(blocksText(b.ToolResult.Content))),
					})
				}
			}
		}
		flush()
		if len(out) == 0 {
			out = append(out, message{Role: "user", Content: json.RawMessage(`""`)})
		}
		return out
	}
	return []message{{Role: string(m.Role), Content: json.RawMessage(`""`)}}
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
		c := toolChoiceNamed{Type: "function"}
		c.Function.Name = tc.ToolName
		return c
	}
	return nil
}

// ---- 错误渲染 ----

func (codec) RenderError(e *ir.Error) (int, []byte) {
	status := e.StatusCode
	if status == 0 {
		status = 500
	}
	return status, marshal(errorResponse{Error: errorBody{Type: e.Type, Code: e.Code, Message: e.Message}})
}

func (codec) RenderStreamError(e *ir.Error) []byte {
	return []byte("data: " + string(marshal(errorResponse{Error: errorBody{Type: e.Type, Code: e.Code, Message: e.Message}})) + "\n\ndata: [DONE]\n\n")
}
