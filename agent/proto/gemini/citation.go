package gemini

import (
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
