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
	case ir.StopPauseTurn, ir.StopAborted:
		// Gemini 既没有续跑也没有中断语义。与 chat 侧同理取 MAX_TOKENS 而非
		// STOP：宁可让客户端知道输出不完整，也别让它把半截结果当成说完了。
		return "MAX_TOKENS"
	default: // end_turn / tool_use / stop_sequence 都以 STOP 收尾
		return "STOP"
	}
}

// encodeUsage 规范 Usage -> Gemini。
func encodeUsage(u ir.Usage) *usageMetadata {
	prompt := u.TotalInput()
	// 口径差：IR 与 OpenAI 两系把思考算作输出的子集，Gemini 把
	// thoughtsTokenCount 与 candidatesTokenCount 并列（totalTokenCount 是三者
	// 之和）。直接把 OutputTokens 填进 candidates 再补 thoughts 会把思考算两遍，
	// 所以 candidates 要先减掉思考部分。
	candidates := u.OutputTokens - u.ReasoningTokens
	if candidates < 0 {
		candidates = 0
	}
	return &usageMetadata{
		PromptTokenCount:        prompt,
		CachedContentTokenCount: u.CacheReadTokens,
		CandidatesTokenCount:    candidates,
		ThoughtsTokenCount:      u.ReasoningTokens,
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
		out.PresencePenalty = gc.PresencePenalty
		out.FrequencyPenalty = gc.FrequencyPenalty
		out.Seed = gc.Seed
		out.Candidates = gc.CandidateCount
		// Gemini 的 responseLogprobs 是开关、logprobs 是档位，
		// 分别对应 IR 的 LogProbs 与 TopLogProbs。
		out.LogProbs = gc.ResponseLogprobs
		out.TopLogProbs = gc.Logprobs
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
		// responseSchema 单独出现（没写 mimeType）也是结构化输出诉求：
		// 只看 mimeType 会把带 schema 的请求整条漏掉。
		if gc.ResponseMimeType == "application/json" || len(gc.ResponseSchema) > 0 {
			// Gemini 的 responseSchema 恒为严格语义，没有 strict 开关也没有名称。
			out.ResponseFormat = &ir.ResponseFormat{Schema: gc.ResponseSchema, Strict: true}
		}
	}
	if req.SystemInstruction != nil {
		for _, p := range req.SystemInstruction.Parts {
			if p.Text != "" {
				out.System = append(out.System, ir.Block{Type: ir.BlockText, Text: p.Text})
			}
		}
	}
	out.CachedContent = req.CachedContent
	for _, s := range req.SafetySettings {
		out.SafetySettings = append(out.SafetySettings, ir.SafetySetting{Category: s.Category, Threshold: s.Threshold})
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
					IsError:   funcResponseIsError(p.FunctionResponse.Response),
					Content:   []ir.Block{{Type: ir.BlockText, Text: decodeFuncResponseText(p.FunctionResponse.Response)}},
				}})
			case p.InlineData != nil:
				msg.Content = append(msg.Content, mediaBlock(p.InlineData.MimeType, p.InlineData.Data, ""))
			case p.FileData != nil:
				msg.Content = append(msg.Content, mediaBlock(p.FileData.MimeType, "", p.FileData.FileURI))
			case p.Thought || p.ThoughtSignature != "":
				sig, from := p.ThoughtSignature, ir.SigFrom(Name, p.ThoughtSignature)
				if sig == dummyThoughtSignature {
					// 这是我们自己塞的占位签名被客户端原样回传。认成 gemini 真签名
					// 就等于给占位符洗白，它会被当作有效凭据一路透传下去。
					from = ir.SigSynthetic
				}
				// 只有签名没有正文的 part：Gemini 原生把签名单独放一个 part，
				// 它属于前一个思考块。另起一块会让上游多收到一个空 thinking。
				if p.Text == "" && sig != "" && len(msg.Content) > 0 {
					if prev := &msg.Content[len(msg.Content)-1]; prev.Type == ir.BlockThinking &&
						prev.Thinking != nil && prev.Thinking.Signature == "" {
						prev.Thinking.Signature, prev.Thinking.SignatureFrom = sig, from
						continue
					}
				}
				msg.Content = append(msg.Content, ir.Block{Type: ir.BlockThinking, Thinking: &ir.Thinking{
					Text: p.Text, Signature: sig, SignatureFrom: from,
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
// funcResponseIsError Gemini 没有 is_error 标志位，官方示例约定把失败写成
// response 里的 error 键。识别它才能让下游（Anthropic 的 is_error、
// kiro 的 status=error）把「工具失败」如实传下去——否则模型会把失败读成成功。
func funcResponseIsError(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil {
		return false
	}
	v, ok := obj["error"]
	if !ok {
		return false
	}
	// error: null 与 error: false 是「没出错」，不能当成出错。
	return string(v) != "null" && string(v) != "false"
}

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
// 判据落在 functionCall 那个 part 自己身上：校验是逐 part 做的，思考 part 上
// 的真签名不能替 functionCall 顶账——按整条 content 有无签名来判断，会让
// 「思考带签名 + functionCall」这种最常见形态里的 functionCall 一个签名都没有。
func ensureThoughtSignature(c *content) {
	for i := range c.Parts {
		if c.Parts[i].FunctionCall != nil && c.Parts[i].ThoughtSignature == "" {
			c.Parts[i].ThoughtSignature = dummyThoughtSignature
		}
	}
}

// ---- 非流式响应 ----

// EncodeResponse IR 响应 -> 非流式响应体。
func (codec) EncodeResponse(resp *ir.Response) ([]byte, error) {
	c := content{Role: "model"}
	// partIndexOf 块序号 -> parts 下标：groundingSupports 用 partIndex 定位，
	// 而块与 part 不是一一对应（thinking 也占 part，媒体块可能被跳过）。
	partIndexOf := map[int]int{}
	for bi, b := range resp.Content {
		if b.Type == ir.BlockText {
			partIndexOf[bi] = len(c.Parts)
		}
		switch b.Type {
		case ir.BlockText, ir.BlockRefusal:
			// Gemini 没有 refusal part；正文并入文本，拒绝这件事由
			// finishReason=SAFETY 承载。丢正文会让客户端只看到一个空 candidate。
			c.Parts = append(c.Parts, part{Text: b.Text})
		case ir.BlockThinking:
			if b.Thinking != nil {
				// 只回本族真签名：外族/合成签名写进 thoughtSignature 会被客户端
				// 当成可回传的凭据，下一轮 Gemini 校验必拒。
				p := part{Text: b.Thinking.Text, Thought: true}
				if b.Thinking.SignatureGenuineFor(Name) {
					p.ThoughtSignature = b.Thinking.Signature
				}
				c.Parts = append(c.Parts, p)
			}
		case ir.BlockToolUse:
			if b.ToolUse != nil {
				// args 是 RawMessage 槽位：非法/非对象参数直接放进去会让
				// 整个响应 marshal 失败或违反 API 对象约束，原文挪进
				// RawArgsKey 键位保真。
				args, _ := ir.NormalizeToolInput(b.ToolUse.Input)
				c.Parts = append(c.Parts, part{FunctionCall: &functionCall{Name: b.ToolUse.Name, Args: args, ID: b.ToolUse.ID}})
			}
		case ir.BlockImage:
			if b.Image != nil && b.Image.Data != "" {
				c.Parts = append(c.Parts, part{InlineData: &blob{MimeType: b.Image.MediaType, Data: b.Image.Data}})
			}
		case ir.BlockMedia:
			// inlineData 能装任意 MIME，媒体块原样带回；只有远端 URI 形态走
			// fileData（Gemini 不接受内联 URL）。
			if b.Media != nil {
				switch {
				case b.Media.Data != "":
					c.Parts = append(c.Parts, part{InlineData: &blob{MimeType: b.Media.MediaType, Data: b.Media.Data}})
				case b.Media.URL != "":
					c.Parts = append(c.Parts, part{FileData: &fileData{MimeType: b.Media.MediaType, FileURI: b.Media.URL}})
				}
			}
		}
	}
	ensureThoughtSignature(&c)
	return json.Marshal(generateResponse{
		Candidates: []candidate{{Content: &c, FinishReason: UnmapFinishReason(resp.StopReason),
			GroundingMetadata: encodeGrounding(resp.Content, partIndexOf)}},
		UsageMetadata: encodeUsage(resp.Usage),
		ModelVersion:  resp.Model,
		ResponseID:    resp.ID,
	})
}

// ResponseNotes 非流式编码损耗扫描：外族签名丢弃 + 对象槽位的畸形参数挪键。
func (codec) ResponseNotes(resp *ir.Response) []string {
	return proto.ScanResponseLosses(resp, Name, false, true)
}

// mediaBlock 按 MIME 分流 inlineData / fileData。Gemini 的这两个字段能装
// 任意 MIME（音频、PDF、视频），此前一律解成 BlockImage：音频会被写进目标协议的
// 图片槽位，上游按图片解码后 400。空 MIME 也不猜图片——它在 Gemini 里是可选字段。
func mediaBlock(mime, data, uri string) ir.Block {
	if strings.HasPrefix(mime, "image/") {
		return ir.Block{Type: ir.BlockImage, Image: &ir.Image{MediaType: mime, Data: data, URL: uri}}
	}
	return ir.Block{Type: ir.BlockMedia, Media: &ir.Media{
		Kind: ir.MediaKindOf(mime), MediaType: mime, Data: data, URL: uri,
	}}
}

// ---- 错误渲染 ----

func (codec) RenderError(e *ir.Error) (int, []byte) {
	status := e.HTTPStatus()
	return status, marshal(errorResponse{Error: geminiErrorOf(e, status)})
}

func (codec) RenderStreamError(e *ir.Error) []byte {
	return sseFrame(marshal(errorResponse{Error: geminiErrorOf(e, e.HTTPStatus())}))
}

// geminiErrorOf Gemini 的错误体只有 code/message/status 三个字段，没有放
// 规范类型与上游错误码的位置。两者进 details：Google 的错误契约就是把
// 额外结构塞在这里，客户端（尤其重试逻辑）需要它们区分「过滤拒绝」与
// 「上游抖动」，塞不进去就只能看 status，而 status 是从状态码反推的粗粒度值。
func geminiErrorOf(e *ir.Error, status int) *geminiError {
	out := &geminiError{Code: status, Message: e.Message, Status: rpcStatus(status)}
	if e.Type == "" && e.Code == "" {
		return out
	}
	d := errorDetail{Type: "type.googleapis.com/google.rpc.ErrorInfo", Domain: "modelsurge.agent", Reason: e.Type}
	if e.Code != "" {
		d.Metadata = map[string]string{"upstream_code": e.Code}
	}
	out.Details = []errorDetail{d}
	return out
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
