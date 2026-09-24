package gemini

import (
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// R110-D3 服务端工具历史部件 toolCall / toolResponse 归不透明块。
//
// genai Part.toolCall / Part.toolResponse 承载 google_search / url_context /
// google_maps / file_search / media_processing 这类服务端工具的历史回传。
// toolType 值域异构、args/response 是各工具专属的泛型 map，没有任何 IR 块型
// 能无损接住。此前 DTO 根本没有这两个字段，整段历史在 unmarshal 阶段静默蒸发，
// 客户端的上下文被无声截断——收进不透明块（From=gemini）后，跨族出站跳过并报
// 损耗，把静默蒸发变成可见损耗。
func TestR110ServerToolPartsGoOpaque(t *testing.T) {
	body := []byte(`{"contents":[{"role":"model","parts":[` +
		`{"toolCall":{"toolType":"GOOGLE_SEARCH_WEB","args":{"query":"weather"}}},` +
		`{"toolResponse":{"toolType":"GOOGLE_SEARCH_WEB","response":{"results":[{"title":"t"}]}}}` +
		`]}]}`)
	req, err := New().DecodeRequest(body)
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if len(req.Messages) != 1 || len(req.Messages[0].Content) != 2 {
		t.Fatalf("部件数量不对（toolCall/toolResponse 可能蒸发了）：%+v", req.Messages)
	}
	for i, wt := range []string{"toolCall", "toolResponse"} {
		b := req.Messages[0].Content[i]
		if b.Type != ir.BlockOpaque || b.Opaque == nil || b.Opaque.WireType != wt || b.Opaque.From != Name {
			t.Errorf("%s 没归本族不透明块：%+v", wt, b)
		}
	}
	// 原文逐字保留：args / response 的泛型 map 一个键都不能丢。
	if !strings.Contains(string(req.Messages[0].Content[0].Opaque.Body), "weather") {
		t.Errorf("toolCall args 原文没逐字保留：%s", req.Messages[0].Content[0].Opaque.Body)
	}
	if !strings.Contains(string(req.Messages[0].Content[1].Opaque.Body), `"title":"t"`) {
		t.Errorf("toolResponse 原文没逐字保留：%s", req.Messages[0].Content[1].Opaque.Body)
	}
}

// 不透明块经 Clone（JSON 往返）必须存活：请求侧 EncodeRequest 会 Clone，
// 丢了 From 就会被当成来源不明、跨族处置全乱。
func TestR110ServerToolOpaqueSurvivesClone(t *testing.T) {
	body := []byte(`{"contents":[{"role":"model","parts":[` +
		`{"toolCall":{"toolType":"FILE_SEARCH","args":{"q":1}}` + `}]}]}`)
	req, err := New().DecodeRequest(body)
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	cloned := req.Clone()
	b := cloned.Messages[0].Content[0]
	if b.Type != ir.BlockOpaque || b.Opaque == nil ||
		b.Opaque.WireType != "toolCall" || b.Opaque.From != Name {
		t.Fatalf("toolCall 不透明块没熬过 Clone：%+v", b)
	}
	if !strings.Contains(string(b.Opaque.Body), "FILE_SEARCH") {
		t.Errorf("Clone 后原文丢了：%s", b.Opaque.Body)
	}
}

// 显式 null 不当成有部件：`"toolCall":null` 不该凭空多出一个不透明块。
func TestR110ServerToolNullNotABlock(t *testing.T) {
	req, err := New().DecodeRequest([]byte(
		`{"contents":[{"role":"model","parts":[{"text":"hi"},{"toolCall":null},{"toolResponse":null}]}]}`))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	for _, b := range req.Messages[0].Content {
		if b.Type == ir.BlockOpaque {
			t.Fatalf("null 被当成了不透明块：%+v", b)
		}
	}
}
