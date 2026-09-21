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

// DedupeCitations 按 (url, start, end) 去重并保序。
// 流式路径上同一条引用会随多个 chunk 重复下发（Gemini 的 groundingChunks 是
// 跨 chunk 累积的），不去重会让客户端把一个来源渲染成很多条。
func DedupeCitations(cs []Citation) []Citation {
	if len(cs) == 0 {
		return nil
	}
	type key struct {
		url        string
		start, end int
	}
	seen := make(map[key]struct{}, len(cs))
	out := make([]Citation, 0, len(cs))
	for _, c := range cs {
		if c.URL == "" {
			continue
		}
		k := key{c.URL, c.Start, c.End}
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil
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
