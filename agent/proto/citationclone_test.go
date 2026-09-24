package proto_test

// R94：跨协议投影来的引用（没有 Raw）经 EncodeRequest 内部的 Clone 之后，
// 不得在出站 citations 数组里留下字面量 null。Clone 是 JSON 往返，空
// RawMessage 缺 omitempty 时会被写成 null 再读回成 4 字节，而 anthropic 的
// 「带 Raw 就原样带回」分支只看长度，于是把 null 当成上游原文发了出去。

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
	"github.com/aceaura/ModelSurge/agent/proto"
	_ "github.com/aceaura/ModelSurge/agent/proto/anthropic"
	_ "github.com/aceaura/ModelSurge/agent/proto/openaichat"
)

const chatRespURLCitation = `{
  "id":"c1","object":"chat.completion","model":"gpt","choices":[
    {"index":0,"finish_reason":"stop","message":{"role":"assistant",
      "content":"北京今天晴，明天有雨。",
      "annotations":[{"type":"url_citation","url_citation":{
         "url":"https://w","title":"天气","start_index":6,"end_index":10,
         "cited_text":"明天有雨"}}]}}],
  "usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`

// anthropicCitations 从出站请求体里挖出每个 text 块的 citations 元素原文。
func anthropicCitations(t *testing.T, body []byte) []string {
	t.Helper()
	var req struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("出站请求体不是合法 JSON：%v\n%s", err, body)
	}
	var out []string
	for _, m := range req.Messages {
		if len(m.Content) == 0 {
			continue
		}
		var blocks []struct {
			Citations json.RawMessage `json:"citations"`
		}
		if err := json.Unmarshal(m.Content, &blocks); err != nil {
			continue
		}
		for _, b := range blocks {
			if len(b.Citations) == 0 {
				continue
			}
			var elems []json.RawMessage
			if err := json.Unmarshal(b.Citations, &elems); err != nil {
				t.Fatalf("citations 不是数组：%v\n%s", err, body)
			}
			for _, e := range elems {
				out = append(out, string(e))
			}
		}
	}
	return out
}

func TestForeignCitationRequestHasNoNullElement(t *testing.T) {
	resp, err := proto.MustOutbound("openai-chat").DecodeResponse([]byte(chatRespURLCitation))
	if err != nil {
		t.Fatalf("chat DecodeResponse: %v", err)
	}
	if len(resp.Content) == 0 || len(resp.Content[0].Citations) == 0 {
		t.Fatalf("chat 侧没解出引用，测试前提不成立：%+v", resp.Content)
	}
	// 前提钉死：跨协议投影来的引用没有 Raw，也没有 Anthropic 的 encrypted_index。
	if c := resp.Content[0].Citations[0]; len(c.Raw) != 0 || c.EncryptedIndex != "" {
		t.Fatalf("前提变了：Raw=%q EncryptedIndex=%q", c.Raw, c.EncryptedIndex)
	}

	build := func(content []ir.Block) []byte {
		req := &ir.Request{Model: "claude", MaxTokens: 16, Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "天气"}}},
			{Role: ir.RoleAssistant, Content: content},
			{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "继续"}}},
		}}
		body, err := proto.MustOutbound("anthropic").EncodeRequest(req)
		if err != nil {
			t.Fatalf("anthropic EncodeRequest: %v", err)
		}
		return body
	}

	// R109 起：encrypted_index 是 web_search_result_location 的 Required 字段
	// （stable 与 beta 的 param 定义一致），投影引用拿不到它——缺键发出去整轮
	// 400，编码侧整条丢弃。出站数组必须为空：既不是 null 也不是缺键对象。
	if elems := anthropicCitations(t, build(resp.Content)); len(elems) != 0 {
		t.Fatalf("缺 encrypted_index 的投影引用应整条丢弃：%q", elems)
	}

	// 上游给了 encrypted_index 的投影（同族历史轮带回来的）正常合成，
	// 元素必须是完整 JSON 对象：不是 null，五个 Required 键一个不缺。
	withIndex := make([]ir.Block, len(resp.Content))
	copy(withIndex, resp.Content)
	withIndex[0].Citations = append([]ir.Citation(nil), resp.Content[0].Citations...)
	withIndex[0].Citations[0].EncryptedIndex = "enc_from_upstream"
	elems := anthropicCitations(t, build(withIndex))
	if len(elems) != 1 {
		t.Fatalf("citations 元素数 = %d, want 1", len(elems))
	}
	if elems[0] == "null" {
		t.Fatal("出站引用是字面量 null，上游必拒整轮")
	}
	if !strings.HasPrefix(strings.TrimSpace(elems[0]), "{") {
		t.Fatalf("出站引用不是 JSON 对象：%q", elems[0])
	}
	for _, want := range []string{`"type":"web_search_result_location"`, `"url":"https://w"`, `"cited_text":"明天有雨"`, `"encrypted_index":"enc_from_upstream"`} {
		if !strings.Contains(elems[0], want) {
			t.Errorf("合成的引用缺 %s：%q", want, elems[0])
		}
	}
}

// 同族往返不受影响：带 Raw 的引用经 Clone 之后仍须逐字节原样出站。
// omitempty 只该消掉空值，不能顺带打穿不透明往返。
func TestNativeCitationRequestSurvivesClone(t *testing.T) {
	const raw = `{"type":"char_location","cited_text":"晴","document_index":0,` +
		`"document_title":"天气报告","start_char_index":4,"end_char_index":5,"file_id":"file_abc"}`
	resp, err := proto.MustOutbound("anthropic").DecodeResponse([]byte(
		`{"id":"m","model":"claude","role":"assistant","content":[
		   {"type":"text","text":"北京今天晴","citations":[` + raw + `]}],
		 "stop_reason":"end_turn"}`))
	if err != nil {
		t.Fatalf("anthropic DecodeResponse: %v", err)
	}
	req := &ir.Request{Model: "claude", MaxTokens: 16, Messages: []ir.Message{
		{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "天气"}}},
		{Role: ir.RoleAssistant, Content: resp.Content},
	}}
	body, err := proto.MustOutbound("anthropic").EncodeRequest(req)
	if err != nil {
		t.Fatalf("anthropic EncodeRequest: %v", err)
	}
	elems := anthropicCitations(t, body)
	if len(elems) != 1 {
		t.Fatalf("citations 元素数 = %d, want 1：%s", len(elems), body)
	}
	var got, want any
	if err := json.Unmarshal([]byte(elems[0]), &got); err != nil {
		t.Fatalf("出站引用不是合法 JSON：%v\n%q", err, elems[0])
	}
	if err := json.Unmarshal([]byte(raw), &want); err != nil {
		t.Fatal(err)
	}
	gb, _ := json.Marshal(got)
	wb, _ := json.Marshal(want)
	if string(gb) != string(wb) {
		t.Errorf("原文没逐字节带回：\n got %s\nwant %s", gb, wb)
	}
}
