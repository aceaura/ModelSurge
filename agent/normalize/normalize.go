// Package normalize 提供 IR 请求的消息规整流水线。
// 各 codec 在 EncodeRequest 时按需选用步骤，把"任意客户端历史"
// 规整为满足目标上游结构约束的形态。
// 步骤设计参考成熟网关的多步规整流水线。
package normalize

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// Placeholder 空内容消息的占位文本。Anthropic 要求每条消息 content 非空，规整
// 流水线与各 codec 的编码兜底共用同一个字面量：流水线跑在编码之前，看不到编码器
// 随后整块丢掉的内容（外族来源的不透明块）。
const Placeholder = "(empty)"

// Options 控制启用哪些规整步骤。零值表示全部启用（最严格）。
type Options struct {
	MergeAdjacentRoles   bool // 合并相邻同角色消息
	EnsureFirstUser      bool // 首条必须是 user，否则插占位
	EnsureAlternating    bool // 强制 user/assistant 交替
	FillEmptyContent     bool // 空内容补占位文本
	FixOrphanToolResults bool // 孤儿 tool_result 降级为文本
	RequireToolPairing   bool // tool_use 后必须紧跟含对应 tool_result 的 user 消息，缺失则补占位结果
	StripToolsIfNoTools  bool // 请求无 tools 时把 tool 痕迹渲染为文本
	SanitizeSchemas      bool // 清洗工具 schema（去 additionalProperties、空 required）
	MaxToolNameLength    int  // 工具名长度上限，0 不限制
}

// Strict 返回全部启用的选项（Anthropic 上游适用）。
func Strict() Options {
	return Options{
		MergeAdjacentRoles:   true,
		EnsureFirstUser:      true,
		EnsureAlternating:    true,
		FillEmptyContent:     true,
		FixOrphanToolResults: true,
		RequireToolPairing:   true,
		StripToolsIfNoTools:  true,
		SanitizeSchemas:      true,
	}
}

// Request 按选项规整请求。返回错误仅用于硬性约束（如工具名超长）。
func Request(req *ir.Request, o Options) error {
	// 工具白名单收窄跑在最前面：被白名单排除的工具压根不会发给上游，它的名字
	// 长度、schema 形状都不再是这一轮的问题，后面的硬性校验不该为它失败。
	EnforceToolAllowlist(req)
	RelaxUndeclaredForcedTool(req)
	DropToolChoiceWithoutTools(req)
	if o.MaxToolNameLength > 0 {
		for _, t := range req.Tools {
			if len(t.Name) > o.MaxToolNameLength {
				return fmt.Errorf("normalize: tool name %q exceeds %d chars", t.Name, o.MaxToolNameLength)
			}
		}
	}
	if o.SanitizeSchemas {
		for i := range req.Tools {
			req.Tools[i].InputSchema = SanitizeSchema(req.Tools[i].InputSchema)
		}
	}
	if o.StripToolsIfNoTools && len(req.Tools) == 0 {
		stripToolContent(req)
	}
	if o.FixOrphanToolResults {
		fixOrphanToolResults(req)
	}
	if o.MergeAdjacentRoles {
		mergeAdjacent(req)
	}
	if o.RequireToolPairing {
		requireToolPairing(req)
	}
	if o.EnsureAlternating {
		ensureAlternating(req)
	}
	if o.EnsureFirstUser {
		ensureFirstUser(req)
	}
	if o.FillEmptyContent {
		fillEmptyContent(req)
	}
	return nil
}

// EnforceToolAllowlist 把「只能调这些」的工具白名单落成目标协议能表达的形式。
//
// 四个出站都没有白名单槽位（Anthropic 官方 tool_choice 只有 auto/any/tool/none
// 四个变体，OpenAI 两系只能指名一个工具），但这一维可以被等价实现：把声明的
// 工具收窄成白名单与已声明工具的交集——上游看不见别的工具，就调不到。不收窄
// 而照原样声明，等于把客户端明令禁止的工具又递了回去：模型随时可能调它，
// 请求里看不出任何异常，客户端也拿不到注记可循。
//
// 收窄规则与「无从收窄」的判据都在 ir.Request.AllowlistNarrow 里，relay.Diagnose
// 报损耗用的是同一个函数——两处各写一遍必然漂移。
func EnforceToolAllowlist(req *ir.Request) {
	if kept, ok := req.AllowlistNarrow(); ok {
		req.Tools = kept
	}
}

// RelaxUndeclaredForcedTool 指名调用的名字必须落在已声明的工具里。
//
// 不满足时回落成 auto。四个出站都会把 ChoiceTool 原样写成 tool_choice，指着
// 一个不在 tools 里的名字是上游必 400 的形状（「tool_choice 必须是已声明工具
// 之一」），而报错只说 tool_choice 无效，读者看不出真正的起因是白名单里写了
// 个没声明的名字。回落是成熟网关的同一处置：客户端要的限制无从实现，但至少
// 这一轮能跑完，落差由 relay.Diagnose 报出。
func RelaxUndeclaredForcedTool(req *ir.Request) {
	if req.ForcedToolUndeclared() {
		req.ToolChoice.Mode = ir.ChoiceAuto
		req.ToolChoice.ToolName = ""
	}
}

// DropToolChoiceWithoutTools 一个工具都没声明时，要求有工具的 tool_choice 整个删掉。
//
// 判据与后果都写在 ir.Request.ToolChoiceWithoutTools 上，relay.Diagnose 报损耗用
// 的是同一个函数。这一步不看 StripToolsIfNoTools 开关——空 tools 是既成事实，与
// 用哪套选项无关。
//
// 删掉而不是回落成 none：没有工具可选时，「禁止调用」「自动决定」「压根不提」
// 三者对上游完全同义，留着一个必然被拒的键没有任何好处。DisableParallel 随之
// 一起没了，同理——零个工具无从并行。
func DropToolChoiceWithoutTools(req *ir.Request) {
	if req.ToolChoiceWithoutTools() {
		req.ToolChoice = nil
	}
}

// SanitizeSchema 递归删除 additionalProperties 与空 required 数组。
// 部分上游（Gemini 一类严格校验 schema 的）对这些字段敏感。
func SanitizeSchema(schema json.RawMessage) json.RawMessage {
	if len(schema) == 0 {
		return schema
	}
	var v any
	if err := json.Unmarshal(schema, &v); err != nil {
		return schema
	}
	clean := sanitizeNode(v)
	out, err := json.Marshal(clean)
	if err != nil {
		return schema
	}
	return out
}

func sanitizeNode(v any) any {
	m, ok := v.(map[string]any)
	if !ok {
		if arr, ok := v.([]any); ok {
			for i := range arr {
				arr[i] = sanitizeNode(arr[i])
			}
			return arr
		}
		return v
	}
	delete(m, "additionalProperties")
	if req, ok := m["required"].([]any); ok && len(req) == 0 {
		delete(m, "required")
	}
	for k, val := range m {
		m[k] = sanitizeNode(val)
	}
	return m
}

// stripToolContent 无 tools 时把 tool_use/tool_result 渲染为文本。
func stripToolContent(req *ir.Request) {
	for i := range req.Messages {
		m := &req.Messages[i]
		// 不能复用 m.Content 的底层数组：一个 tool_result 会展开成「文本 +
		// 抽出的媒体」多个块，写头越过读游标后会覆盖同一条消息里尚未读到的
		// 块。客户端常在同一条 user 消息里先放 tool_result 再放新指令，指令
		// 会整块消失且上游返回 200。
		out := make([]ir.Block, 0, len(m.Content))
		for _, b := range m.Content {
			switch b.Type {
			case ir.BlockToolUse:
				out = append(out, ir.Block{Type: ir.BlockText, Text: renderToolUse(b.ToolUse)})
			case ir.BlockToolResult:
				out = append(out, ir.Block{Type: ir.BlockText, Text: renderToolResult(b.ToolResult)})
				if b.ToolResult != nil {
					out = append(out, mediaOf(b.ToolResult.Content)...)
				}
			default:
				out = append(out, b)
			}
		}
		m.Content = out
	}
}

// mediaOf 抽出图片与附件块。tool 结果降级成文本时 renderToolResult 只拼文本，
// 媒体块会整块消失——降级的目的是绕开上游的工具配对约束，不是丢附件。
func mediaOf(blocks []ir.Block) []ir.Block {
	var out []ir.Block
	for _, b := range blocks {
		if b.Type == ir.BlockImage || b.Type == ir.BlockMedia {
			out = append(out, b)
		}
	}
	return out
}

func renderToolUse(tu *ir.ToolUse) string {
	if tu == nil {
		return ""
	}
	args := string(tu.Input)
	if args == "" {
		// 无参调用的规范形态是 {}（与 ir.ToolUse.ObjectInput 同口径）。留一个
		// 悬空换行等于把半截结构塞进提示词，模型看到的是「参数被截断了」。
		args = "{}"
	}
	return fmt.Sprintf("[Tool: %s (%s)]\n%s", tu.Name, tu.ID, args)
}

func renderToolResult(tr *ir.ToolResult) string {
	if tr == nil {
		return ""
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "[Tool Result (%s)]\n", tr.ToolUseID)
	for _, b := range tr.Content {
		if b.Type == ir.BlockText {
			sb.WriteString(b.Text)
		}
	}
	return sb.String()
}

// fixOrphanToolResults 前面没有带匹配 tool_use 的 assistant 消息时，
// 把 tool_result 降级为文本，避免上游 400。
func fixOrphanToolResults(req *ir.Request) {
	seen := map[string]bool{}
	for i := range req.Messages {
		m := &req.Messages[i]
		if m.Role == ir.RoleAssistant {
			for _, b := range m.Content {
				if b.Type == ir.BlockToolUse && b.ToolUse != nil {
					seen[b.ToolUse.ID] = true
				}
			}
			continue
		}
		// 同 stripToolContent：降级是 1->N 展开，复用底层数组会覆盖后续块。
		out := make([]ir.Block, 0, len(m.Content))
		for _, b := range m.Content {
			if b.Type == ir.BlockToolResult && b.ToolResult != nil && !seen[b.ToolResult.ToolUseID] {
				out = append(out, ir.Block{Type: ir.BlockText, Text: renderToolResult(b.ToolResult)})
				out = append(out, mediaOf(b.ToolResult.Content)...)
			} else {
				out = append(out, b)
			}
		}
		m.Content = out
	}
}

// requireToolPairing 保证每个 tool_use 在下一条 user 消息中有对应 tool_result，
// 缺失则插入占位结果（Anthropic 硬性约束）。
func requireToolPairing(req *ir.Request) {
	var out []ir.Message
	for i := 0; i < len(req.Messages); i++ {
		m := req.Messages[i]
		out = append(out, m)
		if m.Role != ir.RoleAssistant {
			continue
		}
		var wants []string
		for _, b := range m.Content {
			if b.Type == ir.BlockToolUse && b.ToolUse != nil {
				wants = append(wants, b.ToolUse.ID)
			}
		}
		if len(wants) == 0 {
			continue
		}
		have := map[string]bool{}
		if i+1 < len(req.Messages) && req.Messages[i+1].Role == ir.RoleUser {
			for _, b := range req.Messages[i+1].Content {
				if b.Type == ir.BlockToolResult && b.ToolResult != nil {
					have[b.ToolResult.ToolUseID] = true
				}
			}
		}
		var missing []ir.Block
		for _, id := range wants {
			if !have[id] {
				missing = append(missing, ir.Block{Type: ir.BlockToolResult, ToolResult: &ir.ToolResult{
					ToolUseID: id,
					Content:   []ir.Block{{Type: ir.BlockText, Text: Placeholder}},
				}})
			}
		}
		if len(missing) > 0 {
			if i+1 < len(req.Messages) && req.Messages[i+1].Role == ir.RoleUser {
				req.Messages[i+1].Content = append(missing, req.Messages[i+1].Content...)
			} else {
				out = append(out, ir.Message{Role: ir.RoleUser, Content: missing})
			}
		}
	}
	req.Messages = out
}

// mergeAdjacent 合并相邻同角色消息。
func mergeAdjacent(req *ir.Request) {
	if len(req.Messages) == 0 {
		return
	}
	out := req.Messages[:0]
	for _, m := range req.Messages {
		if n := len(out); n > 0 && out[n-1].Role == m.Role && out[n-1].AudioID == "" && m.AudioID == "" {
			out[n-1].Content = append(out[n-1].Content, m.Content...)
		} else {
			out = append(out, m)
		}
	}
	req.Messages = out
}

// ensureAlternating 在连续同角色消息间插入占位消息（user/assistant 交替）。
func ensureAlternating(req *ir.Request) {
	if len(req.Messages) == 0 {
		return
	}
	out := make([]ir.Message, 0, len(req.Messages))
	for _, m := range req.Messages {
		if n := len(out); n > 0 && out[n-1].Role == m.Role {
			other := ir.RoleUser
			if m.Role == ir.RoleUser {
				other = ir.RoleAssistant
			}
			out = append(out, ir.Message{Role: other, Content: []ir.Block{{Type: ir.BlockText, Text: Placeholder}}})
		}
		out = append(out, m)
	}
	req.Messages = out
}

// ensureFirstUser 首条消息必须是 user。
func ensureFirstUser(req *ir.Request) {
	if len(req.Messages) == 0 || req.Messages[0].Role == ir.RoleUser {
		return
	}
	req.Messages = append([]ir.Message{{
		Role:    ir.RoleUser,
		Content: []ir.Block{{Type: ir.BlockText, Text: Placeholder}},
	}}, req.Messages...)
}

// fillEmptyContent 空内容消息补占位文本。
func fillEmptyContent(req *ir.Request) {
	for i := range req.Messages {
		m := &req.Messages[i]
		hasContent := m.AudioID != ""
		for _, b := range m.Content {
			if b.Type != ir.BlockText || strings.TrimSpace(b.Text) != "" {
				hasContent = true
				break
			}
		}
		if !hasContent {
			m.Content = append(m.Content, ir.Block{Type: ir.BlockText, Text: Placeholder})
		}
	}
}
