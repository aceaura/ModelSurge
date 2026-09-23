package anthropic

import (
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// R96：document 块的同族往返保真。
//
// 官方 DocumentBlockParam（anthropic/types/document_block_param.py）= source /
// type / cache_control / citations / context / title，其中 source 是四形态
// union：base64(application/pdf) | text(text/plain) | content | url。
// 此前 context 在 block 结构里没有字段、citations 配置对象解进 RawMessage 后
// 从未入 IR、source.type=content 落进 decodeDocument 的 default 被当 base64——
// 三者都是同族往返就丢，客户端连注记都看不到。

func roundTripRequest(t *testing.T, body string) (string, *ir.Request) {
	t.Helper()
	req, err := codec{}.DecodeRequest([]byte(body))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	out, err := codec{}.EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	return string(out), req
}

func TestDocumentContextRoundTrips(t *testing.T) {
	const body = `{"model":"m","max_tokens":64,"messages":[{"role":"user","content":[` +
		`{"type":"document","source":{"type":"text","media_type":"text/plain","data":"DOC-BODY"},` +
		`"title":"DOC-TITLE","context":"DOC-CONTEXT-HERE"},{"type":"text","text":"cite it"}]}]}`
	s, req := roundTripRequest(t, body)
	if got := req.Messages[0].Content[0].Media.Context; got != "DOC-CONTEXT-HERE" {
		t.Errorf("IR Media.Context = %q，want DOC-CONTEXT-HERE", got)
	}
	for _, want := range []string{"DOC-BODY", "DOC-TITLE", `"context":"DOC-CONTEXT-HERE"`} {
		if !strings.Contains(s, want) {
			t.Errorf("往返丢了 %s：%s", want, s)
		}
	}
}

// 显式关闭与「没给这个键」不是一回事：压成缺失就把决定权交回上游默认值，
// 同族往返也不再逐字。
func TestDocumentCitationsSwitchRoundTrips(t *testing.T) {
	for _, enabled := range []string{"true", "false"} {
		t.Run("enabled="+enabled, func(t *testing.T) {
			body := `{"model":"m","max_tokens":64,"messages":[{"role":"user","content":[` +
				`{"type":"document","source":{"type":"text","media_type":"text/plain","data":"D"},` +
				`"citations":{"enabled":` + enabled + `}}]}]}`
			s, req := roundTripRequest(t, body)
			got := req.Messages[0].Content[0].Media.CitationsEnabled
			if got == nil {
				t.Fatalf("IR CitationsEnabled = nil，want 指向 %s", enabled)
			}
			if want := enabled == "true"; *got != want {
				t.Errorf("IR CitationsEnabled = %v，want %v", *got, want)
			}
			if want := `"citations":{"enabled":` + enabled + `}`; !strings.Contains(s, want) {
				t.Errorf("往返丢了 %s：%s", want, s)
			}
		})
	}
}

// 没给 citations 键时不得凭空造一个开关：那会把「客户端没表态」变成「关闭引用」。
func TestDocumentWithoutCitationsStaysWithout(t *testing.T) {
	const body = `{"model":"m","max_tokens":64,"messages":[{"role":"user","content":[` +
		`{"type":"document","source":{"type":"text","media_type":"text/plain","data":"D"}}]}]}`
	s, req := roundTripRequest(t, body)
	if got := req.Messages[0].Content[0].Media.CitationsEnabled; got != nil {
		t.Errorf("IR CitationsEnabled = %v，want nil（键根本没给）", *got)
	}
	if strings.Contains(s, `"citations"`) {
		t.Errorf("凭空多出 citations 键：%s", s)
	}
	if strings.Contains(s, `"context"`) {
		t.Errorf("凭空多出 context 键：%s", s)
	}
}

// text 块上的 citations 是引用数组，与 document 上的配置对象同名不同形。
// 数组不得被当成配置解出一个假开关。
func TestTextCitationsArrayIsNotADocumentSwitch(t *testing.T) {
	const body = `{"model":"m","max_tokens":64,"messages":[{"role":"user","content":[` +
		`{"type":"text","text":"晴","citations":[{"type":"char_location","cited_text":"晴",` +
		`"document_index":0,"start_char_index":0,"end_char_index":1}]}]}]}`
	_, req := roundTripRequest(t, body)
	b := req.Messages[0].Content[0]
	if b.Type != ir.BlockText {
		t.Fatalf("块型 = %q，want text", b.Type)
	}
	if len(b.Citations) != 1 {
		t.Errorf("引用条数 = %d，want 1", len(b.Citations))
	}
}

// source.type=content 是官方 union 的第四种形态（正文是字符串或 text/image 块
// 数组）。IR 的 Media 只有 base64 / URL / file_id 三种载体，装不下块数组：
// 此前落进 default 被当 base64，Data 取空、MIME 兜底成 application/pdf，
// 重新编码后写出一个连 data 键都没有的 base64 PDF source——正文全丢，形状还
// 非法（官方 Base64PDFSourceParam.data 是 Required），上游直接 400。
func TestDocumentContentSourceStaysVerbatim(t *testing.T) {
	for _, tc := range []struct {
		name  string
		src   string
		keeps []string
		bad   []string
	}{
		{
			name:  "字符串正文",
			src:   `{"type":"content","content":"CONTENT-BODY-HERE"}`,
			keeps: []string{`"type":"content"`, "CONTENT-BODY-HERE"},
			bad:   []string{`"type":"base64"`},
		},
		{
			name: "text+image 块数组",
			src: `{"type":"content","content":[{"type":"text","text":"ARRAY-BODY-HERE"},` +
				`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aW1n"}}]}`,
			keeps: []string{`"type":"content"`, "ARRAY-BODY-HERE", "aW1n"},
			// 内嵌 image 子块本来就带 base64 source，那一份是合法的，故这一例
			// 不能像字符串正文那样断言整条输出里不出现 "type":"base64"。
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"model":"m","max_tokens":64,"messages":[{"role":"user","content":[` +
				`{"type":"document","source":` + tc.src + `}]}]}`
			s, req := roundTripRequest(t, body)
			b := req.Messages[0].Content[0]
			if b.Type != ir.BlockOpaque {
				t.Fatalf("块型 = %q，want opaque（投进 media 会伪造出一个空 PDF）", b.Type)
			}
			if b.Opaque == nil || b.Opaque.WireType != "document" {
				t.Fatalf("不透明载荷 = %+v，want WireType=document", b.Opaque)
			}
			for _, want := range tc.keeps {
				if !strings.Contains(s, want) {
					t.Errorf("往返丢了 %s：%s", want, s)
				}
			}
			if !strings.Contains(s, `"source":{"type":"content"`) {
				t.Errorf("document 的 source 不再是 content 形态：%s", s)
			}
			// 伪造形状的特征：兜底 MIME。两个夹具都不可能合法出现这个值。
			bad := append([]string{`"application/pdf"`}, tc.bad...)
			for _, no := range bad {
				if strings.Contains(s, no) {
					t.Errorf("仍在伪造 %s：%s", no, s)
				}
			}
		})
	}
}

// 归不透明块不得牵连同消息里的兄弟块——那正是 R91 修的那类缺陷。
func TestDocumentContentSourceDoesNotDestroySiblings(t *testing.T) {
	const body = `{"model":"m","max_tokens":64,"messages":[{"role":"user","content":[` +
		`{"type":"document","source":{"type":"content","content":"DOC-BODY-HERE"}},` +
		`{"type":"text","text":"the actual question"}]}]}`
	s, req := roundTripRequest(t, body)
	if n := len(req.Messages[0].Content); n != 2 {
		t.Fatalf("块数 = %d，want 2", n)
	}
	if got := req.Messages[0].Content[1]; got.Type != ir.BlockText || got.Text != "the actual question" {
		t.Errorf("兄弟文本块被牵连：%+v", got)
	}
	for _, want := range []string{"DOC-BODY-HERE", "the actual question"} {
		if !strings.Contains(s, want) {
			t.Errorf("往返丢了 %s：%s", want, s)
		}
	}
	if strings.Contains(s, `"text":""`) {
		t.Errorf("凭空多出空文本块：%s", s)
	}
}

// 另外三种 source 形态仍要正常投进 Media：归不透明块只针对装不下的 content。
func TestDocumentOtherSourcesStillProject(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  string
		want func(*ir.Media) bool
	}{
		{"base64 PDF", `{"type":"base64","media_type":"application/pdf","data":"UEQ="}`,
			func(m *ir.Media) bool { return m.Kind == ir.MediaDocument && m.Data == "UEQ=" }},
		{"内联纯文本", `{"type":"text","media_type":"text/plain","data":"hello"}`,
			func(m *ir.Media) bool { return m.MediaType == "text/plain" && m.Data == "hello" }},
		{"远程 URL", `{"type":"url","url":"https://example.com/a.pdf"}`,
			func(m *ir.Media) bool { return m.URL == "https://example.com/a.pdf" }},
		{"已上传文件", `{"type":"file","file_id":"file_1"}`,
			func(m *ir.Media) bool { return m.FileID == "file_1" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"model":"m","max_tokens":64,"messages":[{"role":"user","content":[` +
				`{"type":"document","source":` + tc.src + `}]}]}`
			s, req := roundTripRequest(t, body)
			b := req.Messages[0].Content[0]
			if b.Type != ir.BlockMedia || b.Media == nil {
				t.Fatalf("块型 = %q media=%+v，want media", b.Type, b.Media)
			}
			if !tc.want(b.Media) {
				t.Errorf("投影不对：%+v", b.Media)
			}
			if !strings.Contains(s, `"type":"document"`) {
				t.Errorf("往返没写回 document 块：%s", s)
			}
		})
	}
}

// 文档块上的 cache_control 断点与配置同时存在时两者都要留住。
func TestDocumentCacheControlSurvivesWithConfig(t *testing.T) {
	const body = `{"model":"m","max_tokens":64,"messages":[{"role":"user","content":[` +
		`{"type":"document","source":{"type":"text","media_type":"text/plain","data":"D"},` +
		`"context":"CTX-HERE","citations":{"enabled":true},` +
		`"cache_control":{"type":"ephemeral","ttl":"1h"}}]}]}`
	s, req := roundTripRequest(t, body)
	b := req.Messages[0].Content[0]
	if b.CacheCtl != "ephemeral" || b.CacheTTL != "1h" {
		t.Errorf("缓存断点 = %q/%q，want ephemeral/1h", b.CacheCtl, b.CacheTTL)
	}
	for _, want := range []string{`"context":"CTX-HERE"`, `"citations":{"enabled":true}`, `"ttl":"1h"`} {
		if !strings.Contains(s, want) {
			t.Errorf("往返丢了 %s：%s", want, s)
		}
	}
}

// 流式方向共用 decodeBlock / encodeBlock，配置同样要带得回来。
func TestStreamDocumentCarriesConfig(t *testing.T) {
	frame := `{"type":"content_block_start","index":0,"content_block":{"type":"document",` +
		`"title":"a.pdf","context":"CTX-HERE","citations":{"enabled":true},` +
		`"source":{"type":"text","media_type":"text/plain","data":"DOC-BODY"}}}`
	evs, err := (&streamDecoder{}).Feed("content_block_start", frame)
	if err != nil {
		t.Fatalf("Feed: %v", err)
	}
	if len(evs) != 1 || evs[0].Block == nil || evs[0].Block.Type != ir.BlockMedia {
		t.Fatalf("解出的事件不对：%+v", evs)
	}
	var sb strings.Builder
	enc := codec{}.NewStreamEncoder()
	for _, ev := range evs {
		frames, err := enc.Encode(ev)
		if err != nil {
			t.Fatalf("Encode: %v", err)
		}
		for _, f := range frames {
			sb.Write(f)
		}
	}
	s := sb.String()
	for _, want := range []string{`"context":"CTX-HERE"`, `"citations":{"enabled":true}`, "DOC-BODY"} {
		if !strings.Contains(s, want) {
			t.Errorf("流式往返丢了 %s：%s", want, s)
		}
	}
}

// 流式里的 content-source 文档同样归不透明块，且原样吐回。
func TestStreamDocumentContentSourceStaysVerbatim(t *testing.T) {
	frame := `{"type":"content_block_start","index":0,"content_block":{"type":"document",` +
		`"source":{"type":"content","content":"STREAM-DOC-BODY"}}}`
	evs, err := (&streamDecoder{}).Feed("content_block_start", frame)
	if err != nil {
		t.Fatalf("Feed: %v", err)
	}
	if len(evs) != 1 || evs[0].Block == nil || evs[0].Block.Type != ir.BlockOpaque {
		t.Fatalf("块型不对：%+v", evs)
	}
	var sb strings.Builder
	enc := codec{}.NewStreamEncoder()
	for _, ev := range evs {
		frames, err := enc.Encode(ev)
		if err != nil {
			t.Fatalf("Encode: %v", err)
		}
		for _, f := range frames {
			sb.Write(f)
		}
	}
	s := sb.String()
	if !strings.Contains(s, "STREAM-DOC-BODY") {
		t.Errorf("流式往返丢了文档正文：%s", s)
	}
	if strings.Contains(s, `"application/pdf"`) {
		t.Errorf("流式仍在伪造 base64 PDF 形状：%s", s)
	}
}
