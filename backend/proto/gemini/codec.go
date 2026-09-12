package gemini

import (
	"encoding/json"
	"fmt"
	"strings"

	"relayd/backend/ir"
	"relayd/backend/normalize"
	"relayd/backend/proto"
)

func init() { proto.Register(codec{}) }

type codec struct{}

// New 返回 codec（便于测试直接构造）。
func New() proto.Codec { return codec{} }

func (codec) Name() string { return Name }

// Caps Gemini：thoughtSignature、图片、托管工具（google_search 等）均支持。
func (codec) Caps() proto.Capabilities {
	return proto.Capabilities{ThinkingSignature: true, Images: true, HostedTools: true}
}

// MapFinishReason Gemini finishReason -> 规范 StopReason。sawTool 表示
// 响应中已出现 functionCall：Gemini 没有独立的 tool_use 终止原因，
// 工具调用以 STOP 收尾，需按内容修正。
func MapFinishReason(s string, sawTool bool) ir.StopReason {
	var stop ir.StopReason
	switch s {
	case "STOP", "FINISH_REASON_UNSPECIFIED", "":
		stop = ir.StopEndTurn
	case "MAX_TOKENS":
		stop = ir.StopMaxTokens
	default: // SAFETY / RECITATION / PROHIBITED_CONTENT / BLOCKLIST / SPII / IMAGE_SAFETY ...
		stop = ir.StopRefusal
	}
	if sawTool && stop == ir.StopEndTurn {
		stop = ir.StopToolUse
	}
	return stop
}

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

// decodeUsage Gemini -> 规范 Usage：promptTokenCount 含 cachedContentTokenCount，
// 需拆出；output 计 candidates + thoughts（思考计入输出）。
func decodeUsage(u *usageMetadata) *ir.Usage {
	if u == nil {
		return nil
	}
	in := u.PromptTokenCount - u.CachedContentTokenCount
	if in < 0 {
		in = 0
	}
	return &ir.Usage{
		InputTokens:     in,
		OutputTokens:    u.CandidatesTokenCount + u.ThoughtsTokenCount,
		CacheReadTokens: u.CachedContentTokenCount,
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
			out.Thinking = &ir.ThinkingConfig{Enabled: true, BudgetTokens: tc.ThinkingBudget}
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

// ---- 请求编码：IR -> Gemini ----

func (codec) EncodeRequest(req *ir.Request) ([]byte, error) {
	r := req.Clone()
	// Gemini 与 Anthropic 的结构约束同构：首条 user、user/model 交替、非空。
	if err := normalize.Request(r, normalize.Strict()); err != nil {
		return nil, err
	}
	out := generateRequest{}

	// tool_use id -> name，供 functionResponse 回填 name。
	nameByID := map[string]string{}
	for _, m := range r.Messages {
		for _, b := range m.Content {
			if b.Type == ir.BlockToolUse && b.ToolUse != nil {
				nameByID[b.ToolUse.ID] = b.ToolUse.Name
			}
		}
	}

	if sys := joinSystem(r.System); sys != "" {
		out.SystemInstruction = &content{Parts: []part{{Text: sys}}}
	}
	for _, m := range r.Messages {
		out.Contents = append(out.Contents, encodeContent(m, nameByID))
	}
	if r.MaxTokens > 0 || r.Temperature != nil || r.TopP != nil || r.TopK != nil ||
		len(r.StopSequences) > 0 || (r.Thinking != nil && r.Thinking.Enabled) {
		gc := &generationConfig{
			Temperature:     r.Temperature,
			TopP:            r.TopP,
			TopK:            r.TopK,
			MaxOutputTokens: r.MaxTokens,
			StopSequences:   r.StopSequences,
		}
		if r.Thinking != nil && r.Thinking.Enabled {
			gc.ThinkingConfig = &thinkingConfig{ThinkingBudget: r.Thinking.BudgetTokens, IncludeThoughts: true}
		}
		out.GenerationConfig = gc
	}
	var decls []functionDecl
	search, codeExec := false, false
	for _, t := range r.Tools {
		switch t.Hosted {
		case "":
			decls = append(decls, functionDecl{Name: t.Name, Description: t.Description, Parameters: t.InputSchema})
		case ir.HostedWebSearch:
			search = true
		case ir.HostedCodeExecution:
			codeExec = true
		default:
			// 无映射的托管种类丢弃，diagnose 已记录
		}
	}
	if search {
		out.Tools = append(out.Tools, toolDef{GoogleSearch: &struct{}{}})
	}
	if codeExec {
		out.Tools = append(out.Tools, toolDef{CodeExecution: &struct{}{}})
	}
	if len(decls) > 0 {
		out.Tools = append(out.Tools, toolDef{FunctionDeclarations: decls})
	}
	if cfg := encodeToolChoice(r.ToolChoice); cfg != nil {
		out.ToolConfig = &toolConfig{FunctionCallingConfig: cfg}
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

// encodeContent 一条 IR 消息 -> Gemini content。
// user 的 tool_result 块转成 functionResponse part（name 由 id 反查）；
// assistant 的 tool_use 块转成 functionCall part。
func encodeContent(m ir.Message, nameByID map[string]string) content {
	out := content{Role: encodeRole(m.Role)}
	for _, b := range m.Content {
		switch b.Type {
		case ir.BlockText:
			out.Parts = append(out.Parts, part{Text: b.Text})
		case ir.BlockThinking:
			if b.Thinking != nil {
				// 外族形态签名对 Gemini 是非法值，置空防 400；
				// 置空后若同 content 含 functionCall，ensureThoughtSignature 会补占位。
				sig := b.Thinking.Signature
				if sig != "" && b.Thinking.SignatureFrom != Name {
					sig = ""
				}
				out.Parts = append(out.Parts, part{
					Text: b.Thinking.Text, Thought: true, ThoughtSignature: sig,
				})
			}
		case ir.BlockImage:
			if b.Image != nil {
				if b.Image.Data != "" {
					out.Parts = append(out.Parts, part{InlineData: &blob{MimeType: b.Image.MediaType, Data: b.Image.Data}})
				} else if b.Image.URL != "" {
					out.Parts = append(out.Parts, part{FileData: &fileData{MimeType: b.Image.MediaType, FileURI: b.Image.URL}})
				}
			}
		case ir.BlockToolUse:
			if b.ToolUse != nil {
				out.Parts = append(out.Parts, part{FunctionCall: &functionCall{
					Name: b.ToolUse.Name, Args: b.ToolUse.Input, ID: b.ToolUse.ID,
				}})
			}
		case ir.BlockToolResult:
			if b.ToolResult != nil {
				name := nameByID[b.ToolResult.ToolUseID]
				if name == "" {
					name = b.ToolResult.ToolUseID // 兜底：至少保持可关联
				}
				out.Parts = append(out.Parts, part{FunctionResponse: &functionResponse{
					Name:     name,
					ID:       b.ToolResult.ToolUseID,
					Response: marshal(map[string]string{"result": blocksText(b.ToolResult.Content)}),
				}})
				// tool_result 中的图片无法挂在 functionResponse 上，
				// 作为同 content 内的 inlineData 部件下发。
				for _, cb := range b.ToolResult.Content {
					if cb.Type == ir.BlockImage && cb.Image != nil && cb.Image.Data != "" {
						out.Parts = append(out.Parts, part{InlineData: &blob{MimeType: cb.Image.MediaType, Data: cb.Image.Data}})
					}
				}
			}
		}
	}
	ensureThoughtSignature(&out)
	return out
}

// ensureThoughtSignature model content 中含 functionCall 时，
// Gemini 3 要求至少一个 part 带 thoughtSignature；缺失则注入占位签名。
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

func encodeRole(r ir.Role) string {
	if r == ir.RoleAssistant {
		return "model"
	}
	return "user"
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

func encodeToolChoice(tc *ir.ToolChoice) *functionCallingConfig {
	if tc == nil {
		return nil
	}
	switch tc.Mode {
	case ir.ChoiceNone:
		return &functionCallingConfig{Mode: "NONE"}
	case ir.ChoiceAny:
		return &functionCallingConfig{Mode: "ANY"}
	case ir.ChoiceTool:
		return &functionCallingConfig{Mode: "ANY", AllowedFunctionNames: []string{tc.ToolName}}
	default:
		return &functionCallingConfig{Mode: "AUTO"}
	}
}

// ---- 非流式响应 ----

// DecodeResponse 上游非流式响应体 -> IR 响应。
func (codec) DecodeResponse(body []byte) (*ir.Response, error) {
	var gr generateResponse
	if err := json.Unmarshal(body, &gr); err != nil {
		return nil, fmt.Errorf("gemini: decode response: %w", err)
	}
	resp := &ir.Response{ID: gr.ResponseID, Model: gr.ModelVersion}
	if u := decodeUsage(gr.UsageMetadata); u != nil {
		resp.Usage = *u
	}
	c := firstCandidate(&gr)
	if c == nil || c.Content == nil {
		return resp, nil
	}
	var callSeq int
	sawTool := false
	for _, p := range c.Content.Parts {
		switch {
		case p.FunctionCall != nil:
			sawTool = true
			callSeq++
			id := p.FunctionCall.ID
			if id == "" {
				id = fmt.Sprintf("call_%d", callSeq)
			}
			args := p.FunctionCall.Args
			if len(args) == 0 {
				args = json.RawMessage(`{}`)
			}
			resp.Content = append(resp.Content, ir.Block{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: id, Name: p.FunctionCall.Name, Input: args}})
		case p.Thought || p.ThoughtSignature != "":
			resp.Content = append(resp.Content, ir.Block{Type: ir.BlockThinking, Thinking: &ir.Thinking{Text: p.Text, Signature: p.ThoughtSignature}})
		case p.InlineData != nil:
			resp.Content = append(resp.Content, ir.Block{Type: ir.BlockImage, Image: &ir.Image{MediaType: p.InlineData.MimeType, Data: p.InlineData.Data}})
		case p.Text != "":
			resp.Content = append(resp.Content, ir.Block{Type: ir.BlockText, Text: p.Text})
		}
	}
	resp.StopReason = MapFinishReason(c.FinishReason, sawTool)
	return resp, nil
}

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
