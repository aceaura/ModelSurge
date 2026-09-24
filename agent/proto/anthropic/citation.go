package anthropic

import (
	"encoding/json"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// decodeCitations 解析 text 块的 citations 数组。非数组形态一律返回 nil：
// Anthropic 在 document / search_result 块上复用同一个键名承载
// {"enabled":bool} 配置对象，那不是引用。
func decodeCitations(raw json.RawMessage) []ir.Citation {
	if len(raw) == 0 || raw[0] != '[' {
		return nil
	}
	var elems []json.RawMessage
	if err := json.Unmarshal(raw, &elems); err != nil {
		return nil
	}
	return citationsToIR(elems)
}

// decodeCitationsConfig 解析 document / search_result 块上 citations 键承载的
// {"enabled":bool} 配置对象。返回 nil 表示客户端根本没给这个键——与显式给了
// false 语义不同，故用指针保留三态。数组形态（text 块的引用）与非对象形态一律
// 返回 nil：那是另一种东西，误当配置会在编码时写出一个假的开关。
func decodeCitationsConfig(raw json.RawMessage) *bool {
	if len(raw) == 0 || raw[0] != '{' {
		return nil
	}
	var cfg citationsConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil
	}
	// enabled 缺键时官方语义为关闭，但「给了对象却没给 enabled」仍是一次显式
	// 表态，按 false 原样带回，不吞掉整个配置对象。
	return &cfg.Enabled
}

// citationsToIR 逐条以原文收，再把可跨协议的字段投影进 IR。
//
// 官方 union 有五种形态，其中只有 web_search_result_location 带 url、
// search_result_location 带 source；char_location / page_location /
// content_block_location 三种文档类引用靠 document_index 与页号/块下标/字符
// 下标定位，根本没有 URL。此前整个数组只按 web_search 一种形态解，四种没有 url
// 的在 DedupeCitations 里被当成「空 URL 的废引用」静默丢光——五种进去只剩一种
// 出来，既没有错误也没有损耗注记，客户端看不到模型引了哪份文档的哪一段。
//
// 非对象元素（null、字符串、数字）直接跳过：它们不可能是引用，而 Unmarshal
// 进 struct 会成功并留下全零值，那样会凭空多出一条空引用。
func citationsToIR(elems []json.RawMessage) []ir.Citation {
	if len(elems) == 0 {
		return nil
	}
	out := make([]ir.Citation, 0, len(elems))
	for _, raw := range elems {
		if len(raw) == 0 || raw[0] != '{' {
			continue
		}
		var c citationIn
		if err := json.Unmarshal(raw, &c); err != nil {
			continue
		}
		url, title := c.URL, c.Title
		switch c.Type {
		case "search_result_location":
			url = c.Source
		case "char_location", "page_location", "content_block_location":
			title = c.DocumentTitle
		}
		out = append(out, ir.Citation{
			URL: url, Title: title, CitedText: c.CitedText,
			Start: c.StartCharIndex, End: c.EndCharIndex,
			EncryptedIndex: c.EncryptedIndex,
			WireType:       c.Type, Raw: raw,
		})
	}
	return ir.DedupeCitations(out)
}

// encodeCitations IR -> Anthropic。
//
// 带 Raw 的一律原样带回：那是上游自己下发的形状，同族往返没有任何理由改写它，
// 而文档类引用的定位字段（document_index、页号、块下标、file_id）IR 之外无处可放，
// 逐字段重建等于伪造。
//
// 没有 Raw 的（跨协议投影来的引用）只能落进 web_search_result_location。
// cited_text 是该形态的必填字段，缺失时按范围从正文反推；反推不出来就整条丢弃
// ——带空 cited_text 发出去上游会 400，丢一条引用好过整轮被拒。
// dropped 记反推失败的条数：丢弃本身是对的，不报出来就是静默丢失（R95 判据），
// 调用方必须把计数并进损耗注记。
func encodeCitations(text string, cs []ir.Citation) (out []json.RawMessage, dropped int) {
	if len(cs) == 0 {
		return nil, 0
	}
	out = make([]json.RawMessage, 0, len(cs))
	for _, c := range cs {
		if len(c.Raw) > 0 {
			out = append(out, c.Raw)
			continue
		}
		cited := ir.ResolveCitedText(text, c)
		if cited == "" {
			dropped++
			continue
		}
		// encrypted_index 与 url 是该形态的 Required 字段（官方
		// citation_web_search_result_location_param.py）：外来投影（responses
		// 的 url_citation、chat 的 annotations）拿不到加密下标，文档类投影
		// 没有 URL，缺键发出去上游必 400，整条丢弃并计数，好过整轮被拒。
		if c.EncryptedIndex == "" || c.URL == "" {
			dropped++
			continue
		}
		b, err := json.Marshal(citationOut{
			Type: "web_search_result_location", URL: c.URL, Title: c.Title,
			CitedText: cited, EncryptedIndex: c.EncryptedIndex,
		})
		if err != nil {
			continue
		}
		out = append(out, b)
	}
	if len(out) == 0 {
		return nil, dropped
	}
	return out, dropped
}
