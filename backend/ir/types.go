// Package ir 定义协议无关的统一中间表示（canonical IR）。
// 所有协议 codec 的唯一职责是 协议格式 <-> IR 的互相转换；
// 跨协议转换一律经由 IR 中转，不存在协议两两直转。
package ir

import "encoding/json"

// Role 消息角色。tool 结果不作为独立角色，而是 user 消息中的 BlockToolResult 块
// （采用 Anthropic 模型作为超集）。
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// BlockType 内容块类型。
type BlockType string

const (
	BlockText       BlockType = "text"
	BlockImage      BlockType = "image"
	BlockToolUse    BlockType = "tool_use"
	BlockToolResult BlockType = "tool_result"
	BlockThinking   BlockType = "thinking"
)

// Block 消息内容块。按 Type 取用对应字段，其余字段为零值。
type Block struct {
	Type        BlockType
	Text        string      // BlockText
	Image       *Image      // BlockImage
	ToolUse     *ToolUse    // BlockToolUse
	ToolResult  *ToolResult // BlockToolResult
	Thinking    *Thinking   // BlockThinking
	CacheCtl    string      // 如 "ephemeral"，仅 Anthropic 方向保留
}

// Image 图片内容。Data 为 base64 编码；URL 与 Data 二选一。
type Image struct {
	MediaType string // 如 "image/png"
	Data      string // base64
	URL       string
}

// ToolUse 一次工具调用。Input 为 JSON 对象。
type ToolUse struct {
	ID    string
	Name  string
	Input json.RawMessage
}

// ToolResult 工具结果，通过 ToolUseID 关联调用。
type ToolResult struct {
	ToolUseID string
	Content   []Block // 通常为 text，可含 image
	IsError   bool
}

// Thinking 推理内容。Signature 为账号绑定的加密签名（Anthropic），
// 跨协议转换时按目标协议形态映射或丢弃。
type Thinking struct {
	Text      string
	Signature string
}

// Message 一条对话消息。
type Message struct {
	Role    Role
	Content []Block
}

// Text 返回消息中所有 text 块拼接的纯文本，便于日志与测试断言。
func (m Message) Text() string {
	var out string
	for _, b := range m.Content {
		if b.Type == BlockText {
			out += b.Text
		}
	}
	return out
}

// Tool 工具定义。Hosted 为非空时表示服务端托管工具（如 "web_search"）。
type Tool struct {
	Name        string
	Description string
	InputSchema json.RawMessage
	Hosted      string
}

// ChoiceMode 工具选择模式。
type ChoiceMode string

const (
	ChoiceAuto     ChoiceMode = "auto"
	ChoiceAny      ChoiceMode = "any"  // 必须调用某个工具（OpenAI required）
	ChoiceNone     ChoiceMode = "none" // 禁止工具
	ChoiceTool     ChoiceMode = "tool" // 指定工具
)

// ToolChoice 工具选择。
type ToolChoice struct {
	Mode           ChoiceMode
	ToolName       string // Mode == ChoiceTool 时有效
	DisableParallel bool
}

// ThinkingConfig 推理配置。Effort 为 OpenAI 风格的等级
// （minimal/low/medium/high/xhigh），BudgetTokens 为 Anthropic 风格预算，
// 两者可并存，codec 按目标协议取用。
type ThinkingConfig struct {
	Enabled      bool
	Effort       string
	BudgetTokens int
}

// Request 统一请求模型。
type Request struct {
	Model         string
	Messages      []Message
	System        []Block // 顶层 system（text 块），Anthropic 形态
	Tools         []Tool
	ToolChoice    *ToolChoice
	MaxTokens     int
	Temperature   *float64
	TopP          *float64
	TopK          *int
	StopSequences []string
	Stream        bool
	Thinking      *ThinkingConfig
	Metadata      map[string]string
}

// Clone 深拷贝请求，用于重试隔离（参考 new-api 的 DeepCopy 惯例）。
func (r *Request) Clone() *Request {
	b, err := json.Marshal(r)
	if err != nil {
		return r
	}
	var c Request
	if err := json.Unmarshal(b, &c); err != nil {
		return r
	}
	return &c
}
