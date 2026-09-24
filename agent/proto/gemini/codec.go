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
			// 一律 Enabled=true 会把显式关闭翻成开启。thinkingLevel 是
			// Gemini 3 的新代表达：只给 level 时 budget 缺省为 0，不能据此
			// 判关——level 本身就是「要思考」的表态。
			out.Thinking = &ir.ThinkingConfig{
				Enabled:      tc.ThinkingBudget != 0 || tc.ThinkingLevel != "",
				BudgetTokens: tc.ThinkingBudget,
				// includeThoughts=false 是「照常思考但别把思考内容给我」。
				// 上游无法据此少想，所以只能在回客户端的方向上抑制。
				HideThoughts: tc.IncludeThoughts != nil && !*tc.IncludeThoughts,
			}
			if tc.ThinkingLevel != "" {
				// 档位原值小写进 effort：跨族出站按目标协议档位集映射，
				// 认不出的档位由出站编码器按既有口径回落。
				out.Thinking.Effort = strings.ToLower(tc.ThinkingLevel)
			}
		}
		// responseSchema 单独出现（没写 mimeType）也是结构化输出诉求：
		// 只看 mimeType 会把带 schema 的请求整条漏掉。responseJsonSchema
		// 是接受完整 JSON Schema 的替代槽位（官方标注互斥，两槽都给时取
		// responseSchema——它是先存在的老槽位，客户端两槽同值是常态）。
		schema := gc.ResponseSchema
		if len(schema) == 0 {
			schema = gc.ResponseJsonSchema
		}
		if gc.ResponseMimeType == "application/json" || len(schema) > 0 {
			// Gemini 的 responseSchema 恒为严格语义，没有 strict 开关也没有名称。
			out.ResponseFormat = &ir.ResponseFormat{Schema: schema, Strict: true}
		} else if gc.ResponseMimeType != "" && gc.ResponseMimeType != "text/plain" {
			// text/x.enum 等非 JSON MIME：约束输出类型，四个出站（gemini 只入
			// 不出）没有一个接得住——收进 IR 只为诊断报得出。text/plain 是显式
			// 缺省，与不给同义，报出来是假阳性。
			out.ResponseMimeType = gc.ResponseMimeType
		}
		// 输出模态归一成 OpenAI 风格小写值进 IR：TEXT/AUDIO 跨族可达 chat 的
		// modalities，IMAGE 没有出站接得住，过滤由诊断报出。
		for _, m := range gc.ResponseModalities {
			out.Modalities = append(out.Modalities, strings.ToLower(m))
		}
		// serviceTier 服务质量档位原值进 IR，跨族映射是出站的事
		// （proto.MapServiceTier）。"unspecified" 是显式缺省，与不给同义，
		// 收进来会让出站发明一个客户端没要的档位。
		if gc.ServiceTier != "" && gc.ServiceTier != "unspecified" {
			out.ServiceTier = gc.ServiceTier
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
	// 不建模值的专属声明键：记下键名让 Diagnose 报得出，值本身不解释。
	if len(req.Labels) > 0 && string(req.Labels) != "null" {
		out.GeminiExtras = append(out.GeminiExtras, "labels")
	}
	if gc := req.GenerationConfig; gc != nil {
		if len(gc.SpeechConfig) > 0 && string(gc.SpeechConfig) != "null" {
			out.GeminiExtras = append(out.GeminiExtras, "speechConfig")
		}
		if gc.MediaResolution != "" {
			out.GeminiExtras = append(out.GeminiExtras, "mediaResolution")
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
			if len(p.PartMetadata) > 0 && string(p.PartMetadata) != "null" {
				out.PartMetaParts++
			}
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
				msg.Content = append(msg.Content, mediaBlock(p.InlineData.MimeType, p.InlineData.Data, "", p.VideoMetadata))
			case p.FileData != nil:
				msg.Content = append(msg.Content, mediaBlock(p.FileData.MimeType, "", p.FileData.FileURI, p.VideoMetadata))
			case len(p.ExecutableCode) > 0 && string(p.ExecutableCode) != "null":
				msg.Content = append(msg.Content, ir.Block{Type: ir.BlockOpaque,
					Opaque: &ir.Opaque{WireType: "executableCode", Body: p.ExecutableCode, From: Name}})
			case len(p.CodeExecutionResult) > 0 && string(p.CodeExecutionResult) != "null":
				msg.Content = append(msg.Content, ir.Block{Type: ir.BlockOpaque,
					Opaque: &ir.Opaque{WireType: "codeExecutionResult", Body: p.CodeExecutionResult, From: Name}})
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
		if t.GoogleSearch != nil || t.GoogleSearchRetrieval != nil {
			// 原生名进 IR：跨族出站按 HostedTypeFamily 回落目标族默认名，
			// 不会把 google_search 写进别族的 type 槽位。retrieval 是旧版
			// 声明形态，语义相同，归一到同一 canonical。
			out.Tools = append(out.Tools, ir.Tool{Name: "google_search", Hosted: ir.HostedWebSearch, HostedType: "google_search"})
		}
		if t.CodeExecution != nil {
			out.Tools = append(out.Tools, ir.Tool{Name: "code_execution", Hosted: ir.HostedCodeExecution, HostedType: "code_execution"})
		}
		// 无跨族映射的托管声明：以未识别 canonical 进 IR，出站按「未映射
		// 托管工具」丢弃并由 Diagnose 报出——比解码即蒸发诚实。
		if t.URLContext != nil {
			out.Tools = append(out.Tools, ir.Tool{Name: "url_context", Hosted: "url_context", HostedType: "urlContext"})
		}
		if t.FileSearch != nil {
			out.Tools = append(out.Tools, ir.Tool{Name: "file_search", Hosted: "file_search", HostedType: "fileSearch"})
		}
		if t.GoogleMaps != nil {
			out.Tools = append(out.Tools, ir.Tool{Name: "google_maps", Hosted: "google_maps", HostedType: "googleMaps"})
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
// response 里的 error 键。识别它才能让下游（Anthropic 的 is_error）把
// 「工具失败」如实传下去——否则模型会把失败读成成功。
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

// decodeToolChoice functionCallingConfig -> IR。
//
// allowedFunctionNames 是独立于 mode 的一维（mode 说要不要必须调，白名单说能调
// 哪些），此前只被用来把 ANY+单项折成指名调用，其余形态一律解出即丢：客户端写明
// 「只能调 alpha、beta」，出站却把 gamma 一并声明出去，模型调到 gamma 时客户端
// 根本没有那个函数可执行，而请求与注记里都看不出任何异常。现在四个分支都把它带进
// IR，由 normalize.EnforceToolAllowlist 收窄已声明工具来落地。
//
// ANY+单项仍折成指名调用：这一折是等价的（必须调 ∧ 只能调 alpha ⇒ 必须调 alpha），
// 且降级路径更温和——名字没声明时回落成 auto（模型可以不调），若保留 any 则是
// 「必须调一个被禁的工具」。
func decodeToolChoice(cfg *functionCallingConfig) *ir.ToolChoice {
	var out *ir.ToolChoice
	switch strings.ToUpper(cfg.Mode) {
	case "ANY":
		if len(cfg.AllowedFunctionNames) == 1 {
			out = &ir.ToolChoice{Mode: ir.ChoiceTool, ToolName: cfg.AllowedFunctionNames[0]}
		} else {
			out = &ir.ToolChoice{Mode: ir.ChoiceAny}
		}
	case "NONE":
		out = &ir.ToolChoice{Mode: ir.ChoiceNone}
	default: // AUTO
		out = &ir.ToolChoice{Mode: ir.ChoiceAuto}
	}
	if len(cfg.AllowedFunctionNames) > 0 {
		out.AllowedTools = append([]string(nil), cfg.AllowedFunctionNames...)
	}
	return out
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
				args := b.ToolUse.ObjectInput()
				c.Parts = append(c.Parts, part{FunctionCall: &functionCall{Name: b.ToolUse.Name, Args: args, ID: b.ToolUse.ID}})
			}
		case ir.BlockImage, ir.BlockMedia:
			// inlineData 能装任意 MIME，图片与媒体块原样带回；只有远端 URI 形态
			// 走 fileData（Gemini 不接受内联 URL）。与流式编码器共用 mediaParts，
			// 保证同一份响应按 stream=true/false 编码出的附件形态一致。
			c.Parts = append(c.Parts, mediaParts(&b)...)
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
// 附件本体走 inlineData / fileData 投得出去（只有文件名带不回：本仓的 gemini
// blob 结构没有 displayName 字段，Gemini 官方是否有该字段未经权威来源核对，
// 故不擅自补），所以不算「助手回合无附件形态」；但纯引用形态的附件装不下，
// 单独报出。
func (codec) ResponseNotes(resp *ir.Response) []string {
	notes := proto.ScanResponseLosses(resp, Name, false, true, false)
	if images, files := undeliverableMedia(resp.Content); images > 0 || files > 0 {
		notes = append(notes, proto.MediaOutputDropNote(images, files))
	}
	return notes
}

// undeliverableMedia 数出 mediaParts 装不下的附件块：既没有 base64 本体也没有
// 远端 URI（例如 Anthropic 的 file_id 文档引用）。与流式编码器同一判定。
func undeliverableMedia(blocks []ir.Block) (images, files int) {
	for i := range blocks {
		b := &blocks[i]
		if b.Type != ir.BlockImage && b.Type != ir.BlockMedia {
			continue
		}
		if len(mediaParts(b)) > 0 {
			continue
		}
		if b.Type == ir.BlockImage {
			images++
		} else {
			files++
		}
	}
	return images, files
}

// mediaBlock 按 MIME 分流 inlineData / fileData。Gemini 的这两个字段能装
// 任意 MIME（音频、PDF、视频），此前一律解成 BlockImage：音频会被写进目标协议的
// 图片槽位，上游按图片解码后 400。空 MIME 也不猜图片——它在 Gemini 里是可选字段。
func mediaBlock(mime, data, uri string, videoMeta json.RawMessage) ir.Block {
	if strings.HasPrefix(mime, "image/") {
		return ir.Block{Type: ir.BlockImage, Image: &ir.Image{MediaType: mime, Data: data, URL: uri}}
	}
	return ir.Block{Type: ir.BlockMedia, Media: &ir.Media{
		Kind: ir.MediaKindOf(mime), MediaType: mime, Data: data, URL: uri,
		// 官方要求 videoMetadata 只随视频数据出现；挂在非视频附件上是客户端
		// 错误，但原样带上比静默丢弃更可诊断（跨族损耗由 Diagnose 报出）。
		VideoMeta: videoMeta,
	}}
}

// mediaParts 附件块（image / media）-> inlineData 或 fileData part。
// 有 base64 本体走 inlineData，否则有远端 URI 走 fileData。两者都没有（例如只有
// Anthropic 的 file_id 文档引用）时返回 nil：本族这两个槽位都装不下一个纯引用，
// 由调用方计数并报损耗，不伪造空 inlineData。
// 非流式编码与流式编码共用，保证两种模式的附件形态一致。
func mediaParts(b *ir.Block) []part {
	var mime, data, uri string
	switch b.Type {
	case ir.BlockImage:
		if b.Image == nil {
			return nil
		}
		mime, data, uri = b.Image.MediaType, b.Image.Data, b.Image.URL
	case ir.BlockMedia:
		if b.Media == nil {
			return nil
		}
		mime, data, uri = b.Media.MediaType, b.Media.Data, b.Media.URL
	default:
		return nil
	}
	switch {
	case data != "":
		return []part{{InlineData: &blob{MimeType: mime, Data: data}}}
	case uri != "":
		return []part{{FileData: &fileData{MimeType: mime, FileURI: uri}}}
	}
	return nil
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
