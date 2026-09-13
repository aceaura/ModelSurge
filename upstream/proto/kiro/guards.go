// guards.go Kiro 载荷体积守卫（KiroaaS payload_guards.py 的 Go 翻译）。
// Kiro API 对超过 ~615KB 的载荷报误导性的 "Improperly formed request."，
// 这里做预检 + 裁剪最老历史（按 user/assistant 对）。
package kiro

import (
	"bytes"
	"encoding/json"
	"log"
)

// MaxPayloadBytes 载荷体积上限（~600KB，留余量）。
const MaxPayloadBytes = 600 * 1024

// PayloadTrimStats 裁剪统计。
type PayloadTrimStats struct {
	OriginalBytes   int
	FinalBytes      int
	OriginalEntries int
	FinalEntries    int
	Trimmed         bool
}

// checkPayloadSize 序列化字节体积。
func checkPayloadSize(payload map[string]any) int {
	b, err := json.Marshal(payload)
	if err != nil {
		return 0
	}
	return len(b)
}

// asMap / asSlice 对 any 的安全取用。
func asMap(v any) (map[string]any, bool) {
	m, ok := v.(map[string]any)
	return m, ok
}

func asSlice(v any) ([]any, bool) {
	s, ok := v.([]any)
	return s, ok
}

// TrimPayloadToLimit 裁掉最老的历史条目（成对裁剪，至少保留 2 条），
// 对齐 userInputMessage 起点，修复孤儿 toolResults。
// 就地修改 payload（history 为引用）。
func TrimPayloadToLimit(payload map[string]any, maxBytes int) PayloadTrimStats {
	originalBytes := checkPayloadSize(payload)
	cs, ok := asMap(payload["conversationState"])
	if !ok {
		return PayloadTrimStats{OriginalBytes: originalBytes, FinalBytes: originalBytes}
	}
	history, ok := asSlice(cs["history"])
	if !ok || len(history) == 0 {
		return PayloadTrimStats{OriginalBytes: originalBytes, FinalBytes: originalBytes}
	}
	originalEntries := len(history)

	stripEmptyToolUses(history)

	for len(history) > 2 && checkPayloadSize(payload) > maxBytes {
		history = history[2:] // 一对 user/assistant
	}

	// 对齐到 userInputMessage 起点（裁剪可能让 assistant 打头）
	for len(history) > 0 {
		if m, ok := asMap(history[0]); ok {
			if _, ok := m["userInputMessage"]; ok {
				break
			}
		}
		history = history[1:]
	}
	cs["history"] = history

	repairOrphanedToolResults(history)

	finalBytes := checkPayloadSize(payload)
	return PayloadTrimStats{
		OriginalBytes: originalBytes, FinalBytes: finalBytes,
		OriginalEntries: originalEntries, FinalEntries: len(history),
		Trimmed: originalEntries != len(history),
	}
}

// stripEmptyToolUses 删除空的 toolUses 数组（Kiro 怪癖）。
func stripEmptyToolUses(history []any) {
	for _, entry := range history {
		m, ok := asMap(entry)
		if !ok {
			continue
		}
		assistant, ok := asMap(m["assistantResponseMessage"])
		if !ok {
			continue
		}
		if tus, ok := asSlice(assistant["toolUses"]); ok && len(tus) == 0 {
			delete(assistant, "toolUses")
		}
	}
}

// repairOrphanedToolResults 裁剪后修复：移除引用不存在 toolUseId 的
// toolResults，孤儿文本以标记内联保留。
func repairOrphanedToolResults(history []any) {
	for i, entry := range history {
		m, ok := asMap(entry)
		if !ok {
			continue
		}
		userMsg, ok := asMap(m["userInputMessage"])
		if !ok {
			continue
		}
		ctx, ok := asMap(userMsg["userInputMessageContext"])
		if !ok {
			continue
		}
		results, ok := asSlice(ctx["toolResults"])
		if !ok {
			continue
		}

		// 前一条 assistant 的 toolUseId 集合
		validIDs := map[string]bool{}
		if i > 0 {
			if pm, ok := asMap(history[i-1]); ok {
				if pa, ok := asMap(pm["assistantResponseMessage"]); ok {
					if tus, ok := asSlice(pa["toolUses"]); ok {
						for _, tu := range tus {
							if tum, ok := asMap(tu); ok {
								if id, _ := tum["toolUseId"].(string); id != "" {
									validIDs[id] = true
								}
							}
						}
					}
				}
			}
		}

		var kept []any
		var orphanedTexts []string
		for _, tr := range results {
			trm, ok := asMap(tr)
			if !ok {
				continue
			}
			id, _ := trm["toolUseId"].(string)
			if validIDs[id] {
				kept = append(kept, tr)
				continue
			}
			switch c := trm["content"].(type) {
			case []any:
				for _, part := range c {
					if pm, ok := asMap(part); ok {
						if txt, _ := pm["text"].(string); txt != "" {
							orphanedTexts = append(orphanedTexts, txt)
						}
					}
				}
			case string:
				if c != "" {
					orphanedTexts = append(orphanedTexts, c)
				}
			}
		}

		if len(kept) != len(results) {
			if len(kept) > 0 {
				ctx["toolResults"] = kept
			} else {
				delete(ctx, "toolResults")
				if len(ctx) == 0 {
					delete(userMsg, "userInputMessageContext")
				}
			}
			if len(orphanedTexts) > 0 {
				cur, _ := userMsg["content"].(string)
				userMsg["content"] = cur + "\n[trimmed tool result] " + joinStrings(orphanedTexts, "; ")
			}
		}
	}
}

func joinStrings(parts []string, sep string) string {
	var buf bytes.Buffer
	for i, p := range parts {
		if i > 0 {
			buf.WriteString(sep)
		}
		buf.WriteString(p)
	}
	return buf.String()
}

// enforcePayloadLimit 体积守卫入口：超限自动裁剪并记日志。
func enforcePayloadLimit(payload map[string]any) {
	if checkPayloadSize(payload) <= MaxPayloadBytes {
		return
	}
	stats := TrimPayloadToLimit(payload, MaxPayloadBytes)
	log.Printf("kiro codec: trimmed history %d -> %d entries (%d -> %d bytes)",
		stats.OriginalEntries, stats.FinalEntries, stats.OriginalBytes, stats.FinalBytes)
}
