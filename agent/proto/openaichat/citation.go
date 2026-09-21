package openaichat

import (
	"unicode/utf8"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// attachCitations 把消息级标注挂到最后一个文本块上。
// Chat 的 annotations 挂在 message 上而 IR 挂在块上：索引口径是整条 content，
// 而多部分 content 的文本部分在实际响应里只有一个（部分形态只出现在请求侧的
// 多模态输入）。没有文本块时整批丢弃——挂到图片块上偏移量无意义。
func attachCitations(blocks []ir.Block, cs []ir.Citation) []ir.Block {
	if len(cs) == 0 {
		return blocks
	}
	for i := len(blocks) - 1; i >= 0; i-- {
		if blocks[i].Type == ir.BlockText {
			blocks[i].Citations = cs
			return blocks
		}
	}
	return blocks
}

// shiftCitations 把块内引用平移到拼接后正文中的位置。
// prefix 是已拼好的前缀，own 是本块正文；先在本块内定位再加前缀长度。
func shiftCitations(cs []ir.Citation, prefix, own string) []ir.Citation {
	if len(cs) == 0 {
		return nil
	}
	off := utf8.RuneCountInString(prefix)
	out := make([]ir.Citation, 0, len(cs))
	for _, c := range cs {
		shifted := c
		// 定位不出来就不动范围。此时 c 本来就没有范围（有范围的话 ResolveRange
		// 一定成功），留着零值即可，不需要额外清理。
		if start, end, ok := ir.ResolveRange(own, c); ok {
			shifted.Start, shifted.End = start+off, end+off
		}
		if shifted.CitedText == "" {
			shifted.CitedText = ir.ResolveCitedText(own, c)
		}
		out = append(out, shifted)
	}
	return out
}

func decodeAnnotations(as []annotation) []ir.Citation {
	if len(as) == 0 {
		return nil
	}
	out := make([]ir.Citation, 0, len(as))
	for _, a := range as {
		// 只认 url_citation：别的种类（file_citation 等）载荷在同名子对象里，
		// url_citation 为空即非本类；空 URL 由 DedupeCitations 统一丢弃。
		c := a.URLCitation
		if c == nil {
			continue
		}
		out = append(out, ir.Citation{
			URL: c.URL, Title: c.Title, CitedText: c.CitedText,
			Start: c.StartIndex, End: c.EndIndex,
		})
	}
	return ir.DedupeCitations(out)
}

// encodeAnnotations IR -> Chat。索引是官方的主要定位手段，反推不出范围时仍然
// 写出该条（URL 与标题本身就有价值），只是范围留零——与 Anthropic 不同，
// Chat 不把 cited_text 当必填，带零范围发出去不会被拒。
func encodeAnnotations(text string, cs []ir.Citation) []annotation {
	if len(cs) == 0 {
		return nil
	}
	out := make([]annotation, 0, len(cs))
	for _, c := range cs {
		uc := &urlCitation{URL: c.URL, Title: c.Title, CitedText: ir.ResolveCitedText(text, c)}
		if start, end, ok := ir.ResolveRange(text, c); ok {
			uc.StartIndex, uc.EndIndex = start, end
		}
		out = append(out, annotation{Type: "url_citation", URLCitation: uc})
	}
	return out
}
