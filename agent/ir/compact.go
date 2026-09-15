// 压缩回退（第二档）的纯变换函数：K 轮切分、压缩 prompt 构造、
// summary 注入。全部无副作用，便于独立单测。
package ir

import (
	"fmt"
	"strings"
)

// CompactMaxTokens 压缩调用 max_tokens 上限（summary 有界是终止性的一部分）。
const CompactMaxTokens = 4096

// SplitForCompact K 轮切分：返回保留起始下标 keepFrom（keep = msgs[keepFrom:]，
// old = msgs[:keepFrom]）。规则：最后一条 user 消息（当前问题）永远保留；
// 从尾部数 K 个 user 边界轮；keep 首条若为含 tool_result 的 user 消息，
// 边界回退到最近的含 tool_use 的 assistant（配对不切断）。
func SplitForCompact(msgs []Message, k int) int {
	if len(msgs) == 0 || k < 0 {
		return 0
	}
	last := -1
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == RoleUser {
			last = i
			break
		}
	}
	if last < 0 {
		return 0 // 无 user 消息无从保留，全部保留（不压缩）
	}
	keepFrom := last
	rounds := 1
	for rounds < k && keepFrom > 0 {
		prev := keepFrom - 1
		for prev >= 0 && msgs[prev].Role != RoleUser {
			prev--
		}
		if prev < 0 {
			break
		}
		keepFrom = prev
		rounds++
	}
	// 工具配对边界回退：keep 首条含 tool_result 时，把最近含 tool_use 的
	// assistant 一起划入 keep（往前找，找不到则维持原边界）。
	for keepFrom > 0 && msgs[keepFrom].Role == RoleUser && hasToolResult(msgs[keepFrom]) {
		j := keepFrom - 1
		for j >= 0 && !(msgs[j].Role == RoleAssistant && hasToolUse(msgs[j])) {
			j--
		}
		if j < 0 {
			break
		}
		keepFrom = j
	}
	return keepFrom
}

func hasToolResult(m Message) bool {
	for _, b := range m.Content {
		if b.Type == BlockToolResult {
			return true
		}
	}
	return false
}

func hasToolUse(m Message) bool {
	for _, b := range m.Content {
		if b.Type == BlockToolUse {
			return true
		}
	}
	return false
}

// BuildCompactRequest 构造压缩调用请求：单条 user 消息（摘要指令 + 旧历史
// 原文渲染），max_tokens 4096、非流式、无工具。Model 由调用方填 compress_model。
func BuildCompactRequest(old []Message) *Request {
	var b strings.Builder
	b.WriteString("Summarize the following conversation history. Preserve: key decisions and their rationale, " +
		"important code context (file paths, function names, snippets that matter), tool calls and their outcomes, " +
		"and any pending tasks or open questions. Be concise but complete; the summary replaces the original " +
		"history in a continuing conversation, so later turns must make sense with only this summary.\n\n" +
		"<conversation_to_summarize>\n")
	for _, m := range old {
		b.WriteString(renderCompactMessage(m))
	}
	b.WriteString("</conversation_to_summarize>\n\nProduce only the summary, no preamble.")
	return &Request{
		Messages:  []Message{{Role: RoleUser, Content: []Block{{Type: BlockText, Text: b.String()}}}},
		MaxTokens: CompactMaxTokens,
	}
}

// renderCompactMessage 把一条 IR 消息渲染为压缩 prompt 里的纯文本形态
// （thinking 不参与摘要；图片以占位符说明）。
func renderCompactMessage(m Message) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[%s]\n", m.Role)
	for _, blk := range m.Content {
		switch blk.Type {
		case BlockText:
			b.WriteString(blk.Text)
			b.WriteString("\n")
		case BlockToolUse:
			if blk.ToolUse != nil {
				fmt.Fprintf(&b, "[tool_use %s(%s): %s]\n", blk.ToolUse.Name, blk.ToolUse.ID, string(blk.ToolUse.Input))
			}
		case BlockToolResult:
			if blk.ToolResult != nil {
				var parts []string
				for _, cb := range blk.ToolResult.Content {
					if cb.Type == BlockText {
						parts = append(parts, cb.Text)
					}
				}
				fmt.Fprintf(&b, "[tool_result %s: %s]\n", blk.ToolResult.ToolUseID, strings.Join(parts, " "))
			}
		case BlockImage:
			b.WriteString("[image omitted]\n")
		}
	}
	b.WriteString("\n")
	return b.String()
}

// BuildCompactedHistory 构造压缩后的新历史：system 与 tools/thinking 等顶层
// 字段保留，消息序列 = 合成 summary 消息（带来源说明提示）+ keep 轮原文。
func BuildCompactedHistory(orig *Request, summary string, keepFrom int) *Request {
	r := orig.Clone()
	notice := "The following is a summary of the earlier conversation (not the original messages), " +
		"produced to fit the context window:\n\n" + summary
	synth := Message{Role: RoleUser, Content: []Block{{Type: BlockText, Text: notice}}}
	r.Messages = append([]Message{synth}, orig.Messages[keepFrom:]...)
	return r
}
