package anthropic

import "github.com/aceaura/ModelSurge/agent/ir"

func orEmptyCitation(c *citation) *citation {
	if c == nil {
		return &citation{}
	}
	return c
}

func decodeCitations(cs []citation) []ir.Citation {
	if len(cs) == 0 {
		return nil
	}
	out := make([]ir.Citation, 0, len(cs))
	for _, c := range cs {
		// 空 URL 由 DedupeCitations 统一丢弃，此处不重复判
		out = append(out, ir.Citation{
			URL: c.URL, Title: c.Title, CitedText: c.CitedText,
			Start: c.StartCharIndex, End: c.EndCharIndex,
			EncryptedIndex: c.EncryptedIndex,
		})
	}
	return ir.DedupeCitations(out)
}

// encodeCitations IR -> Anthropic。cited_text 是官方必填字段，缺失时按索引从
// 正文反推；反推不出来就整条丢弃——带空 cited_text 发出去上游会 400，
// 丢一条引用好过整轮被拒。
func encodeCitations(text string, cs []ir.Citation) []citation {
	if len(cs) == 0 {
		return nil
	}
	out := make([]citation, 0, len(cs))
	for _, c := range cs {
		cited := ir.ResolveCitedText(text, c)
		if cited == "" {
			continue
		}
		start, end, ok := ir.ResolveRange(text, c)
		if !ok {
			continue
		}
		out = append(out, citation{
			Type: "web_search_result_location", URL: c.URL, Title: c.Title,
			CitedText: cited, EncryptedIndex: c.EncryptedIndex,
			StartCharIndex: start, EndCharIndex: end,
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
