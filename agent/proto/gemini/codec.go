package gemini

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/aceaura/ModelSurge/agent/ir"
	"github.com/aceaura/ModelSurge/agent/proto"
)

func init() { proto.RegisterInbound(codec{}) }

type codec struct{}

// New 返回入口 codec（便于测试直接构造）。
func New() proto.InboundCodec { return codec{} }

func (codec) Name() string { return Name }

// UnmapFinishReason 规范 StopReason -> Gemini finishReason。
func UnmapFinishReason(s ir.StopReason) string {
	switch s {
	case ir.StopMaxTokens:
		return "MAX_TOKENS"
	case ir.StopRefusal:
		return "SAFETY"
	default: // end_turn / tool_use 都以 STOP 收尾
		return "STOP"
	}
}

// encodeUsage 规范 Usage -> Gemini。output 无法拆回 thoughts，全部计入 candidates。
func encodeUsage(u ir.Usage) *usageMetadata {
	prompt := u.TotalInput()
	return &usageMetadata{
		PromptTokenCount:        prompt,
		CachedContentTokenCount: u.CacheReadTokens,
		CandidatesTokenCount:    u.OutputTokens,
		TotalTokenCount:         prompt + u.OutputTokens,
	}
}

// dummyThoughtSignature Gemini 3 要求 functionCall 所在 part 必须携带
// thoughtSignature；历史会话中的签名不可还原时注入官方认可的占位值
// （参考 sub2api ensureGeminiFunctionCallThoughtSignatures）。
const dummyThoughtSignature = "skip_thought_signature_validator"

// ---- 请求解码：Gemini -> IR ----

func (codec) DecodeRequest(body []byte) (*ir.Request, error) {
	var req generateRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("gemini: decode request: %w", err)
	}
	out := &ir.Request{}
	if gc := req.GenerationConfig; gc != nil {
		out.MaxTokens = gc.MaxOutputTokens
		out.Temperature = gc.Temperature
		out.TopP = gc.TopP
		out.TopK = gc.TopK
		out.StopSequences = gc.StopSequences
		if tc := gc.ThinkingConfig; tc != nil {
			// thinkingBudget=0 是官方的「关闭思考」，-1 是动态思考；
			// 一律 Enabled=true 会把显式关闭翻成开启。
			out.Thinking = &ir.ThinkingConfig{
				Enabled:      tc.ThinkingBudget != 0,
				BudgetTokens: tc.ThinkingBudget,
				// includeThoughts=false 是「照常思考但别把思考内容给我」。
				// 上游无法据此少想，所以只能在回客户端的方向上抑制。
				HideThoughts: tc.IncludeThoughts != nil && !*tc.IncludeThoughts,
			}
		}
	}
	if req.SystemInstruction != nil {
		for _, p := range req.SystemInstruction.Parts {
			if p.Text != "" {
				out.System = append(out.System, ir.Block{Type: ir.BlockText, Text: p.Text})
			}
		}
	}

	// Gemini 的 functionCall/functionResponse 历史上没有 ID。
	// 为 functionCall 合成 call_N，functionResponse 按"同名称按顺序"关联回去
	// （参考 new-api geminiFunctionCallHistory）；新版 API 自带的 id 优先使用。
	var callSeq int
	pending := map[string][]string{} // name -> 未匹配的 call id 队列
	synthID := func() string {
		callSeq++
		return fmt.Sprintf("call_%d", callSeq)
	}
	for _, c := range req.Contents {
		msg := ir.Message{Role: decodeRole(c.Role)}
		for _, p := range c.Parts {
			switch {
			case p.FunctionCall != nil:
				id := p.FunctionCall.ID
				if id == "" {
					id = synthID()
				}
				pending[p.FunctionCall.Name] = append(pending[p.FunctionCall.Name], id)
				args := p.FunctionCall.Args
				if len(args) == 0 {
					args = json.RawMessage(`{}`)
				}
				msg.Content = append(msg.Content, ir.Block{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{
					ID: id, Name: p.FunctionCall.Name, Input: args,
				}})
			case p.FunctionResponse != nil:
				id := p.FunctionResponse.ID
				if id == "" {
					if q := pending[p.FunctionResponse.Name]; len(q) > 0 {
						id = q[0]
						pending[p.FunctionResponse.Name] = q[1:]
					} else {
						id = synthID() // 孤儿 response，交给 normalize 降级
					}
				}
				msg.Content = append(msg.Content, ir.Block{Type: ir.BlockToolResult, ToolResult: &ir.ToolResult{
					ToolUseID: id,
					Content:   []ir.Block{{Type: ir.BlockText, Text: decodeFuncResponseText(p.FunctionResponse.Response)}},
				}})
			case p.InlineData != nil:
				msg.Content = append(msg.Content, ir.Block{Type: ir.BlockImage, Image: &ir.Image{
					MediaType: p.InlineData.MimeType, Data: p.InlineData.Data,
				}})
			case p.FileData != nil:
				msg.Content = append(msg.Content, ir.Block{Type: ir.BlockImage, Image: &ir.Image{
					MediaType: p.FileData.MimeType, URL: p.FileData.FileURI,
				}})
			case p.Thought || p.ThoughtSignature != "":
				msg.Content = append(msg.Content, ir.Block{Type: ir.BlockThinking, Thinking: &ir.Thinking{
					Text: p.Text, Signature: p.ThoughtSignature, SignatureFrom: ir.SigFrom(Name, p.ThoughtSignature),
				}})
			case p.Text != "":
				msg.Content = append(msg.Content, ir.Block{Type: ir.BlockText, Text: p.Text})
			}
		}
		out.Messages = append(out.Messages, msg)
	}
	for _, t := range req.Tools {
		if t.GoogleSearch != nil {
			out.Tools = append(out.Tools, ir.Tool{Name: "google_search", Hosted: ir.HostedWebSearch})
		}
		if t.CodeExecution != nil {
			out.Tools = append(out.Tools, ir.Tool{Name: "code_execution", Hosted: ir.HostedCodeExecution})
		}
		for _, fd := range t.FunctionDeclarations {
			out.Tools = append(out.Tools, ir.Tool{Name: fd.Name, Description: fd.Description, InputSchema: fd.Parameters})
		}
	}
	if cfg := req.ToolConfig; cfg != nil && cfg.FunctionCallingConfig != nil {
		out.ToolChoice = decodeToolChoice(cfg.FunctionCallingConfig)
	}
	return out, nil
}

func decodeRole(role string) ir.Role {
	if role == "model" {
		return ir.RoleAssistant
	}
	return ir.RoleUser
}

// decodeFuncResponseText functionResponse.response 是任意 JSON object。
// 优先提取常见的字符串字段，否则保留原始 JSON 文本，保证不丢信息。
func decodeFuncResponseText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return string(raw)
	}
	for _, key := range []string{"result", "output", "content"} {
		if s, ok := obj[key].(string); ok {
			return s
		}
	}
	return string(raw)
}

func decodeToolChoice(cfg *functionCallingConfig) *ir.ToolChoice {
	switch strings.ToUpper(cfg.Mode) {
	case "ANY":
		if len(cfg.AllowedFunctionNames) == 1 {
			return &ir.ToolChoice{Mode: ir.ChoiceTool, ToolName: cfg.AllowedFunctionNames[0]}
		}
		return &ir.ToolChoice{Mode: ir.ChoiceAny}
	case "NONE":
		return &ir.ToolChoice{Mode: ir.ChoiceNone}
	default: // AUTO
		return &ir.ToolChoice{Mode: ir.ChoiceAuto}
	}
}

// ensureThoughtSignature 保证返回给 Gemini 客户端的 functionCall 带签名。
func ensureThoughtSignature(c *content) {
	hasCall, hasSig := false, false
	for _, p := range c.Parts {
		if p.FunctionCall != nil {
			hasCall = true
		}
		if p.ThoughtSignature != "" {
			hasSig = true
		}
	}
	if hasCall && !hasSig {
		for i := range c.Parts {
			if c.Parts[i].FunctionCall != nil {
				c.Parts[i].ThoughtSignature = dummyThoughtSignature
				return
			}
		}
	}
}

// ---- 非流式响应 ----

// EncodeResponse IR 响应 -> 非流式响应体。
func (codec) EncodeResponse(resp *ir.Response) ([]byte, error) {
	c := content{Role: "model"}
	for _, b := range resp.Content {
		switch b.Type {
		case ir.BlockText:
			c.Parts = append(c.Parts, part{Text: b.Text})
		case ir.BlockThinking:
			if b.Thinking != nil {
				c.Parts = append(c.Parts, part{Text: b.Thinking.Text, Thought: true, ThoughtSignature: b.Thinking.Signature})
			}
		case ir.BlockToolUse:
			if b.ToolUse != nil {
				args := b.ToolUse.Input
				if len(args) == 0 {
					args = json.RawMessage(`{}`)
				}
				c.Parts = append(c.Parts, part{FunctionCall: &functionCall{Name: b.ToolUse.Name, Args: args, ID: b.ToolUse.ID}})
			}
		case ir.BlockImage:
			if b.Image != nil && b.Image.Data != "" {
				c.Parts = append(c.Parts, part{InlineData: &blob{MimeType: b.Image.MediaType, Data: b.Image.Data}})
			}
		}
	}
	ensureThoughtSignature(&c)
	return json.Marshal(generateResponse{
		Candidates:    []candidate{{Content: &c, FinishReason: UnmapFinishReason(resp.StopReason)}},
		UsageMetadata: encodeUsage(resp.Usage),
		ModelVersion:  resp.Model,
		ResponseID:    resp.ID,
	})
}

// ---- 错误渲染 ----

func (codec) RenderError(e *ir.Error) (int, []byte) {
	status := e.StatusCode
	if status == 0 {
		status = 500
	}
	return status, marshal(errorResponse{Error: &geminiError{
		Code: status, Message: e.Message, Status: rpcStatus(status),
	}})
}

func (codec) RenderStreamError(e *ir.Error) []byte {
	status := e.StatusCode
	if status == 0 {
		status = 500
	}
	return sseFrame(marshal(errorResponse{Error: &geminiError{
		Code: status, Message: e.Message, Status: rpcStatus(status),
	}}))
}

// rpcStatus HTTP 状态码 -> Google RPC status 名。
func rpcStatus(status int) string {
	switch status {
	case 400:
		return "INVALID_ARGUMENT"
	case 401:
		return "UNAUTHENTICATED"
	case 403:
		return "PERMISSION_DENIED"
	case 404:
		return "NOT_FOUND"
	case 429:
		return "RESOURCE_EXHAUSTED"
	case 503:
		return "UNAVAILABLE"
	default:
		if status >= 500 {
			return "INTERNAL"
		}
		return "UNKNOWN"
	}
}
