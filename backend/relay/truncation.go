// truncation.go 上游截断恢复（truncation_state.py / truncation_recovery.py 翻译）。
// Kiro 上游会截断大工具参数与超长正文。网关在响应侧探测截断并记录；
// 客户端下次请求携带被截断的 tool_use_id / 回显被截断正文时，
// 注入合成提示告知模型是上游限制而非其过错（可开关，默认开）。
package relay

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"log"
	"sync"

	"relayd/backend/codec"
	"relayd/backend/ir"
)

// truncationNoticeTool 工具参数被截断时前置到 tool_result 的提示
// （措辞刻意不给具体步骤，避免模型机械拆步）。
const truncationNoticeTool = "[API Limitation] Your tool call was truncated by the upstream API due to output size limits.\n\n" +
	"If the tool result below shows an error or unexpected behavior, this is likely a CONSEQUENCE of the truncation, " +
	"not the root cause. The tool call itself was cut off before it could be fully transmitted.\n\n" +
	"Repeating the exact same operation will be truncated again. Consider adapting your approach."

// truncationNoticeContent 正文被截断时追加的合成 user 消息。
const truncationNoticeContent = "[System Notice] Your previous response was truncated by the API due to " +
	"output size limitations. This is not an error on your part. " +
	"If you need to continue, please adapt your approach rather than repeating the same output."

// truncationMaxEntries 状态表容量上限（LRU 驱逐最旧；条目一次性取用，
// 1024 足以覆盖活跃会话的最近截断）。
const truncationMaxEntries = 1024

// truncationEntry 状态表条目：工具截断（按 tool_use_id 键）或
// 正文截断（按内容哈希键），二选一。
type truncationEntry struct {
	element *list.Element // LRU 链表节点（front = 最近）
	name    string        // 工具名（日志用）
	reason  string        // 截断诊断（日志用）
}

// TruncationTracker 截断恢复状态表。线程安全；
// 键与 Python 同为值键（tool_use_id / 内容哈希，跨请求稳定），
// 一次性取用（pop），LRU 有界防泄漏。
type TruncationTracker struct {
	mu      sync.Mutex
	enabled bool
	order   *list.List
	entries map[string]*truncationEntry
}

// NewTruncationTracker 构造状态表。
func NewTruncationTracker(enabled bool) *TruncationTracker {
	return &TruncationTracker{
		enabled: enabled,
		order:   list.New(),
		entries: map[string]*truncationEntry{},
	}
}

// Record 流结束后上报上游截断（探测自 codec.TruncationReporter 缝）。
func (t *TruncationTracker) Record(upName string, tools []codec.TruncatedTool, contentTruncated bool, content string) {
	if t == nil || !t.enabled {
		return
	}
	if len(tools) == 0 && !contentTruncated {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, tl := range tools {
		t.saveLocked("t:"+tl.ID, tl.Name, tl.Reason)
		log.Printf("relay: upstream %s truncated tool %s (%s), model will be notified on next request",
			upName, tl.Name, tl.Reason)
	}
	if contentTruncated {
		t.saveLocked("c:"+contentHash(content), "", "")
		log.Printf("relay: upstream %s truncated content (%d chars), model will be notified on next request",
			upName, len(content))
	}
}

// saveLocked 写入并置顶；超容量驱逐最旧（须持锁）。
func (t *TruncationTracker) saveLocked(key, name, reason string) {
	if e, ok := t.entries[key]; ok {
		e.name, e.reason = name, reason
		t.order.MoveToFront(e.element)
		return
	}
	el := t.order.PushFront(key)
	t.entries[key] = &truncationEntry{element: el, name: name, reason: reason}
	for t.order.Len() > truncationMaxEntries {
		oldest := t.order.Back()
		delete(t.entries, oldest.Value.(string))
		t.order.Remove(oldest)
	}
}

// popLocked 命中即取用（一次性；须持锁）。
func (t *TruncationTracker) popLocked(key string) (truncationEntry, bool) {
	e, ok := t.entries[key]
	if !ok {
		return truncationEntry{}, false
	}
	t.order.Remove(e.element)
	delete(t.entries, key)
	return *e, true
}

// InjectNotices 就地注入恢复提示：命中的 tool_result 前置截断提示；
// 命中的 assistant 正文后追加合成 user 提示。返回是否修改。
// 匹配即取用（同一条目不重复通知）。
func (t *TruncationTracker) InjectNotices(req *ir.Request) bool {
	if t == nil || !t.enabled || len(req.Messages) == 0 {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	modified := false
	var out []ir.Message
	for _, m := range req.Messages {
		out = append(out, m)
		switch m.Role {
		case ir.RoleUser:
			for i := range m.Content {
				b := &m.Content[i]
				if b.Type != ir.BlockToolResult || b.ToolResult == nil {
					continue
				}
				if _, hit := t.popLocked("t:" + b.ToolResult.ToolUseID); hit {
					notice := append([]ir.Block{{Type: ir.BlockText,
						Text: truncationNoticeTool + "\n\n---\n\nOriginal tool result:\n"}}, b.ToolResult.Content...)
					b.ToolResult.Content = notice
					modified = true
				}
			}
		case ir.RoleAssistant:
			if text := m.Text(); text != "" {
				if _, hit := t.popLocked("c:" + contentHash(text)); hit {
					out = append(out, ir.Message{Role: ir.RoleUser, Content: []ir.Block{
						{Type: ir.BlockText, Text: truncationNoticeContent},
					}})
					modified = true
				}
			}
		}
	}
	if modified {
		req.Messages = out
	}
	return modified
}

// contentHash 正文关联键：前 500 字节 sha256 前 16 hex
// （truncation_state.py save/get_content_truncation 对齐）。
func contentHash(content string) string {
	if len(content) > 500 {
		content = content[:500]
	}
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:8])
}
