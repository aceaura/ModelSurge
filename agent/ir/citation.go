package ir

import (
	"strings"
	"unicode/utf8"
)

// ResolveCitedText 补齐缺失的 CitedText：上游只给了范围时按范围从正文切片。
// 范围越界一律返回空而不是截断——截出来的片段会与上游真正引用的文字不同，
// 客户端据此高亮会指向错误的位置，宁可不给。
func ResolveCitedText(text string, c Citation) string {
	if c.CitedText != "" {
		return c.CitedText
	}
	if !c.HasRange() || c.Start < 0 {
		return ""
	}
	runes := []rune(text)
	if c.End > len(runes) {
		return ""
	}
	return string(runes[c.Start:c.End])
}

// ResolveRange 补齐缺失的范围：上游只给了 CitedText 时在正文里定位。
// 只认唯一匹配。同一片段在正文里出现多次时返回 false：取首次出现会把高亮
// 落在错误的那一处，而协议没有任何字段能表达「我不确定是哪一处」。
func ResolveRange(text string, c Citation) (start, end int, ok bool) {
	if c.HasRange() && c.Start >= 0 {
		return c.Start, c.End, true
	}
	if c.CitedText == "" {
		return 0, 0, false
	}
	i := strings.Index(text, c.CitedText)
	if i < 0 {
		return 0, 0, false
	}
	if strings.Contains(text[i+len(c.CitedText):], c.CitedText) {
		return 0, 0, false
	}
	start = utf8.RuneCountInString(text[:i])
	return start, start + utf8.RuneCountInString(c.CitedText), true
}

// DedupeCitations 去重并保序。
// 流式路径上同一条引用会随多个 chunk 重复下发（Gemini 的 groundingChunks 是
// 跨 chunk 累积的），不去重会让客户端把一个来源渲染成很多条。
//
// 不做「空 URL 就丢」：Anthropic 的文档类引用（char_location 等）本来就没有
// URL，靠 document_index 与页/块/字符下标定位，丢掉等于把出处静默抹光。
// 能不能跨协议表达由 Portable 判，丢弃是各族编码器的职责，不是去重的职责。
// 带 Raw 的按原文比：文档类引用的 URL 与范围都可能全空，只靠投影字段区分
// 会把「同一段文字引自两个不同文档」误判成重复。
func DedupeCitations(cs []Citation) []Citation {
	if len(cs) == 0 {
		return nil
	}
	type key struct {
		raw, url   string
		start, end int
	}
	seen := make(map[key]struct{}, len(cs))
	out := make([]Citation, 0, len(cs))
	for _, c := range cs {
		k := key{string(c.Raw), c.URL, c.Start, c.End}
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, c)
	}
	return out
}

// CountCitations 统计请求里携带的引用条数（用于有损诊断）。
func CountCitations(r *Request) int {
	n := 0
	for _, m := range r.Messages {
		for _, b := range m.Content {
			n += len(b.Citations)
		}
	}
	return n
}

// CountNonPortableCitations 统计请求里目标协议装不下的引用条数（用于有损诊断）。
// 与 CountCitations 分开：后者只在目标协议根本没有标注槽位时才非零，
// 而文档类引用是「有槽位但槽位以 URL 为身份」，四个族里三个都装不下。
func CountNonPortableCitations(r *Request) int {
	n := 0
	for _, m := range r.Messages {
		for _, b := range m.Content {
			for _, c := range b.Citations {
				if !c.Portable() {
					n++
				}
			}
		}
	}
	return n
}

// CountAnthropicUnreplayable 统计目标是 anthropic 时编码侧会整条丢弃的引用
// 条数（用于有损诊断）。判据与 anthropic encodeCitations 逐条对齐：带 Raw 的
// 原文带回不算丢；外来投影（responses 的 url_citation、chat 的 annotations）
// 缺 encrypted_index（web_search_result_location 的 Required 字段，缺键上游
// 400）、缺 URL（同 Required）或 cited_text 反推不出来的，编码侧宁丢不伪造。
func CountAnthropicUnreplayable(r *Request) int {
	n := 0
	for _, m := range r.Messages {
		for _, b := range m.Content {
			for _, c := range b.Citations {
				if len(c.Raw) > 0 {
					continue
				}
				if c.EncryptedIndex == "" || c.URL == "" || ResolveCitedText(b.Text, c) == "" {
					n++
				}
			}
		}
	}
	return n
}
