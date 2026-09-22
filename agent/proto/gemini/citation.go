package gemini

import (
	"strings"
	"unicode/utf8"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// encodeGrounding 把 IR 块上的引用铺成 candidate 级 groundingMetadata。
// partIndex 指向该块在 parts 数组里的位置；区间按**字节**换算（Gemini 口径），
// 而 IR 用字符下标，这一步不能省。
func encodeGrounding(blocks []ir.Block, partIndexOf map[int]int) *groundingMetadata {
	var chunks []groundingChunk
	var supports []groundingSupport
	// chunkOf URL -> chunk 下标，让指向同一来源的多段正文共用一个 chunk
	// （Gemini 的 chunk 数组就是来源清单，重复写会让客户端看到重复来源）。
	chunkOf := map[string]int{}
	for bi, b := range blocks {
		pi, ok := partIndexOf[bi]
		if !ok {
			continue
		}
		for _, c := range b.Citations {
			if c.URL == "" {
				continue
			}
			ci, seen := chunkOf[c.URL]
			if !seen {
				ci = len(chunks)
				chunkOf[c.URL] = ci
				chunks = append(chunks, groundingChunk{Web: &groundingWeb{URI: c.URL, Title: c.Title}})
			}
			start, end, ok := ir.ResolveRange(b.Text, c)
			if !ok {
				// 没有区间就没有 support：Gemini 的 support 必须带 segment，
				// 而 chunk 已经写进来源清单，来源本身不会丢。
				continue
			}
			supports = append(supports, groundingSupport{
				Segment: groundingSegment{
					PartIndex:  pi,
					StartIndex: byteOffset(b.Text, start),
					EndIndex:   byteOffset(b.Text, end),
					Text:       ir.ResolveCitedText(b.Text, c),
				},
				GroundingChunkIndices: []int{ci},
			})
		}
	}
	if len(chunks) == 0 {
		return nil
	}
	return &groundingMetadata{GroundingChunks: chunks, GroundingSupports: supports}
}

// byteOffset 字符下标 -> 字节下标。
func byteOffset(s string, runeIdx int) int {
	if runeIdx <= 0 {
		return 0
	}
	n := 0
	for i := 0; i < runeIdx && n < len(s); i++ {
		_, size := utf8.DecodeRuneInString(s[n:])
		n += size
	}
	return n
}

// encodeGroundingStreamed 流式路径的引用铺法。与非流式的差别：正文是逐增量
// 作为独立 part 下发的，客户端拼接后 part 序号全局递增，一条引用的区间可能
// 横跨多个增量 part——按 part 边界切成多条 support，各用本 part 内的字节偏移。
// 区间落在尚未下发的正文上时跳过该段（引用按约定在正文之后到达，此分支是防御）。
func encodeGroundingStreamed(t *encText, citations []ir.Citation) *groundingMetadata {
	var chunks []groundingChunk
	var supports []groundingSupport
	chunkOf := map[string]int{}
	var whole string
	if t != nil {
		whole = strings.Join(t.deltas, "")
	}
	for _, c := range citations {
		if c.URL == "" {
			continue
		}
		ci, seen := chunkOf[c.URL]
		if !seen {
			ci = len(chunks)
			chunkOf[c.URL] = ci
			chunks = append(chunks, groundingChunk{Web: &groundingWeb{URI: c.URL, Title: c.Title}})
		}
		if t == nil {
			continue
		}
		start, end, ok := ir.ResolveRange(whole, c)
		if !ok {
			// 没有区间就没有 support（同非流式）：来源已进清单，不丢。
			continue
		}
		runePos := 0
		for i, d := range t.deltas {
			dRunes := utf8.RuneCountInString(d)
			ps, pe := runePos, runePos+dRunes
			runePos = pe
			s, e := max(start, ps), min(end, pe)
			if s >= e {
				continue
			}
			supports = append(supports, groundingSupport{
				Segment: groundingSegment{
					PartIndex:  t.parts[i],
					StartIndex: byteOffset(d, s-ps),
					EndIndex:   byteOffset(d, e-ps),
					Text:       string([]rune(d)[s-ps : e-ps]),
				},
				GroundingChunkIndices: []int{ci},
			})
		}
	}
	if len(chunks) == 0 {
		return nil
	}
	return &groundingMetadata{GroundingChunks: chunks, GroundingSupports: supports}
}
