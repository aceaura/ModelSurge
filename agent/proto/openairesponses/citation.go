package openairesponses

import "github.com/aceaura/ModelSurge/agent/ir"

func orEmptyAnnotation(a *annotation) *annotation {
	if a == nil {
		return &annotation{}
	}
	return a
}

func decodeAnnotations(as []annotation) []ir.Citation {
	if len(as) == 0 {
		return nil
	}
	out := make([]ir.Citation, 0, len(as))
	for _, a := range as {
		if a.Type != "" && a.Type != "url_citation" {
			continue
		}
		// Responses 的标注同样以 URL 为来源身份，空 URL 的那条连自己协议里都
		// 无从渲染，收下只会往下游传一条空壳。
		if a.URL == "" {
			continue
		}
		out = append(out, ir.Citation{URL: a.URL, Title: a.Title, Start: a.StartIndex, End: a.EndIndex})
	}
	return ir.DedupeCitations(out)
}

// encodeAnnotations IR -> Responses。该协议的形态里没有 cited_text 字段，
// 只能靠索引定位；定位不出来仍写出该条（URL 与标题本身有价值）。
// 没有 URL 的引用跳过：url_citation 以 URL 为来源身份，空 url 的标注会让
// 客户端渲染出一个跳不动的引用。丢弃条数由调用方计入损耗注记。
func encodeAnnotations(text string, cs []ir.Citation) []annotation {
	if len(cs) == 0 {
		return nil
	}
	out := make([]annotation, 0, len(cs))
	for _, c := range cs {
		if !c.Portable() {
			continue
		}
		a := annotation{Type: "url_citation", URL: c.URL, Title: c.Title}
		if start, end, ok := ir.ResolveRange(text, c); ok {
			a.StartIndex, a.EndIndex = start, end
		}
		out = append(out, a)
	}
	return out
}
