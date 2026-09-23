package proto_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
	"github.com/aceaura/ModelSurge/agent/proto"
)

// multimodal_test.go 非图片附件（PDF / 音频 / 视频）的跨协议投影。
//
// 此前 IR 只有 BlockImage 一种媒体块，于是：Chat 的 input_audio/file 与
// Responses 的 input_file/input_audio 整块消失（part switch 无 default），
// Anthropic 的 document 落到 default 变成空文本块，Gemini 的 inlineData 无论
// 什么 MIME 都解成图片——音频被写进目标协议的图片槽位，上游按图片解码后 400。
//
// 降级判据（参考 cc-switch 的 UNSUPPORTED_IMAGE_MARKER，不取 new-api 的硬报错，
// 后者违背本仓「只报有损，从不拒请求」）：目标协议有对应槽位就原样带过去，
// 装不下则换成描述被丢内容的占位文本块，并由 relay 侧诊断告知客户端。

const (
	pdfB64 = "JVBERi0xLjQK"
	audB64 = "UklGRgABAABXQVZF"
)

// ---- 入站解码：四种入站形态都必须解成 BlockMedia 且大类正确 ----

func TestInboundDecodesMedia(t *testing.T) {
	for _, c := range []struct {
		name, proto, body string
		wantKind          ir.MediaKind
		wantMIME          string
		wantData          string
	}{
		{
			name: "anthropic/document-base64", proto: "anthropic",
			body:     `{"model":"m","messages":[{"role":"user","content":[{"type":"document","title":"spec.pdf","source":{"type":"base64","media_type":"application/pdf","data":"` + pdfB64 + `"}}]}]}`,
			wantKind: ir.MediaDocument, wantMIME: "application/pdf", wantData: pdfB64,
		},
		{
			name: "openai-chat/input_audio", proto: "openai-chat",
			body:     `{"model":"m","messages":[{"role":"user","content":[{"type":"input_audio","input_audio":{"data":"` + audB64 + `","format":"wav"}}]}]}`,
			wantKind: ir.MediaAudio, wantMIME: "audio/wav", wantData: audB64,
		},
		{
			name: "openai-chat/file", proto: "openai-chat",
			body:     `{"model":"m","messages":[{"role":"user","content":[{"type":"file","file":{"filename":"spec.pdf","file_data":"data:application/pdf;base64,` + pdfB64 + `"}}]}]}`,
			wantKind: ir.MediaDocument, wantMIME: "application/pdf", wantData: pdfB64,
		},
		{
			name: "openai-responses/input_file", proto: "openai-responses",
			body:     `{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_file","filename":"spec.pdf","file_data":"data:application/pdf;base64,` + pdfB64 + `"}]}]}`,
			wantKind: ir.MediaDocument, wantMIME: "application/pdf", wantData: pdfB64,
		},
		{
			name: "openai-responses/input_audio", proto: "openai-responses",
			body:     `{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_audio","input_audio":{"data":"` + audB64 + `","format":"mp3"}}]}]}`,
			wantKind: ir.MediaAudio, wantMIME: "audio/mp3", wantData: audB64,
		},
		{
			// 最要紧的一格：Gemini 的 inlineData 能装任意 MIME。
			name: "gemini/inlineData-pdf", proto: "gemini",
			body:     `{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"application/pdf","data":"` + pdfB64 + `"}}]}]}`,
			wantKind: ir.MediaDocument, wantMIME: "application/pdf", wantData: pdfB64,
		},
		{
			name: "gemini/inlineData-audio", proto: "gemini",
			body:     `{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"audio/mpeg","data":"` + audB64 + `"}}]}]}`,
			wantKind: ir.MediaAudio, wantMIME: "audio/mpeg", wantData: audB64,
		},
		{
			name: "gemini/inlineData-video", proto: "gemini",
			body:     `{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"video/mp4","data":"` + audB64 + `"}}]}]}`,
			wantKind: ir.MediaVideo, wantMIME: "video/mp4", wantData: audB64,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := onlyMedia(t, c.proto, c.body)
			if m.Kind != c.wantKind {
				t.Errorf("Kind = %q, want %q", m.Kind, c.wantKind)
			}
			if m.MediaType != c.wantMIME {
				t.Errorf("MediaType = %q, want %q", m.MediaType, c.wantMIME)
			}
			if m.Data != c.wantData {
				t.Errorf("Data = %q, want %q", m.Data, c.wantData)
			}
		})
	}
}

// 图片仍须解成 BlockImage：把它一并卷进 BlockMedia 会让既有图片路径
// （四个出站的图片槽位）全部失效。
func TestGeminiImageStillDecodesAsImage(t *testing.T) {
	body := `{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"image/png","data":"AAA="}}]}]}`
	req, err := proto.MustInbound("gemini").DecodeRequest([]byte(body))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	b := req.Messages[0].Content[0]
	if b.Type != ir.BlockImage {
		t.Fatalf("块类型 = %q, want image", b.Type)
	}
	if b.Image == nil || b.Image.MediaType != "image/png" {
		t.Errorf("图片信息丢失：%+v", b.Image)
	}
}

// 空 MIME 不得猜成图片：Gemini 的 mimeType 是可选字段，猜错会把附件
// 投进图片槽位。归 MediaOther 让诊断按「认不出类型」报。
func TestGeminiEmptyMimeIsNotImage(t *testing.T) {
	body := `{"contents":[{"role":"user","parts":[{"inlineData":{"data":"AAA="}}]}]}`
	m := onlyMedia(t, "gemini", body)
	if m.Kind != ir.MediaOther {
		t.Errorf("Kind = %q, want other", m.Kind)
	}
}

// fileData（远端 URI）走 URL 而不是 Data：两者不可互相换算，混用会让
// 出站写出空 base64。
func TestGeminiFileDataKeepsURI(t *testing.T) {
	body := `{"contents":[{"role":"user","parts":[{"fileData":{"mimeType":"application/pdf","fileUri":"gs://b/spec.pdf"}}]}]}`
	m := onlyMedia(t, "gemini", body)
	if m.URL != "gs://b/spec.pdf" || m.Data != "" {
		t.Errorf("URI 未原样保留：URL=%q Data=%q", m.URL, m.Data)
	}
}

// Anthropic document 的四种 source 形态各自有独立落点，混同会丢信息：
// url 与 file 形态没有 base64 可填，text 形态的 MIME 缺省是 text/plain 而非 PDF。
func TestAnthropicDocumentSourceShapes(t *testing.T) {
	for _, c := range []struct {
		name, source              string
		wantMIME, wantData, wantU string
		wantFileID                string
	}{
		{
			name: "url", source: `{"type":"url","url":"https://x/spec.pdf"}`,
			wantMIME: "application/pdf", wantU: "https://x/spec.pdf",
		},
		{
			name: "file", source: `{"type":"file","file_id":"file_123"}`,
			wantMIME: "application/pdf", wantFileID: "file_123",
		},
		{
			name: "text", source: `{"type":"text","data":"hello"}`,
			wantMIME: "text/plain", wantData: "hello",
		},
		{
			// media_type 缺失时缺省 PDF（对齐 cc-switch 的同款兜底）。
			// 留空会让 MediaKindOf 判成 other，转出时投错槽位。
			name: "base64-no-mime", source: `{"type":"base64","data":"` + pdfB64 + `"}`,
			wantMIME: "application/pdf", wantData: pdfB64,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			body := `{"model":"m","messages":[{"role":"user","content":[{"type":"document","source":` + c.source + `}]}]}`
			m := onlyMedia(t, "anthropic", body)
			if m.MediaType != c.wantMIME {
				t.Errorf("MediaType = %q, want %q", m.MediaType, c.wantMIME)
			}
			if m.Data != c.wantData {
				t.Errorf("Data = %q, want %q", m.Data, c.wantData)
			}
			if m.URL != c.wantU {
				t.Errorf("URL = %q, want %q", m.URL, c.wantU)
			}
			if m.FileID != c.wantFileID {
				t.Errorf("FileID = %q, want %q", m.FileID, c.wantFileID)
			}
		})
	}
}

// 只给 file_id / 只给 URL 时 MIME 不可知，不得猜成 PDF：猜错会让附件
// 投进目标协议的文档槽位被按 PDF 解析。
func TestUnknownMimeStaysOther(t *testing.T) {
	for _, c := range []struct{ name, proto, body string }{
		{"openai-chat/file_id-only", "openai-chat",
			`{"model":"m","messages":[{"role":"user","content":[{"type":"file","file":{"file_id":"file_1"}}]}]}`},
		{"openai-responses/file_id-only", "openai-responses",
			`{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_file","file_id":"file_1"}]}]}`},
		{"openai-responses/file_url-only", "openai-responses",
			`{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_file","file_url":"https://x/a.bin"}]}]}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := onlyMedia(t, c.proto, c.body)
			if m.Kind != ir.MediaOther {
				t.Errorf("Kind = %q, want other（MIME 不可知不得猜）", m.Kind)
			}
			// 大类判对了不代表引用还在：只断言 Kind 的话，把 file_id / file_url
			// 丢掉仍然全绿，而那两个字段是上游取到内容的唯一途径。
			if m.FileID == "" && m.URL == "" {
				t.Errorf("文件引用整个丢了：%+v", m)
			}
		})
	}
}

// ---- 出站编码：有槽位的照原样送达 ----

func TestOutboundCarriesDocument(t *testing.T) {
	req := mediaReq(&ir.Media{
		Kind: ir.MediaDocument, MediaType: "application/pdf", Data: pdfB64, Filename: "spec.pdf",
	})
	for _, c := range []struct{ name, want string }{
		{"anthropic", `"type":"document"`},
		{"openai-chat", `"type":"file"`},
		{"openai-responses", `"type":"input_file"`},
	} {
		t.Run(c.name, func(t *testing.T) {
			wire := wireOf(t, c.name, req)
			if !strings.Contains(wire, c.want) {
				t.Errorf("缺 %s：\n%s", c.want, wire)
			}
			if !strings.Contains(wire, pdfB64) {
				t.Errorf("附件内容丢失：\n%s", wire)
			}
		})
	}
}

// 音频有专属槽位的协议必须用它，不得塞进文件槽位：OpenAI 的 input_audio
// 需要 format 字段，塞进 file 会让上游拿不到解码格式。
func TestOutboundCarriesAudioInDedicatedSlot(t *testing.T) {
	req := mediaReq(&ir.Media{Kind: ir.MediaAudio, MediaType: "audio/wav", Data: audB64, Format: "wav"})
	for _, name := range []string{"openai-chat", "openai-responses"} {
		t.Run(name, func(t *testing.T) {
			wire := wireOf(t, name, req)
			if !strings.Contains(wire, `"type":"input_audio"`) {
				t.Errorf("音频未走专属槽位：\n%s", wire)
			}
			if !strings.Contains(wire, `"format":"wav"`) {
				t.Errorf("缺 format，上游无法解码：\n%s", wire)
			}
		})
	}
}

// Format 缺失时从 MIME 反推：IR 里两者可互推，但 Chat 协议只认 format，
// 留空会让上游 400。
func TestAudioFormatDerivedFromMime(t *testing.T) {
	req := mediaReq(&ir.Media{Kind: ir.MediaAudio, MediaType: "audio/mp3", Data: audB64})
	for _, name := range []string{"openai-chat", "openai-responses"} {
		t.Run(name, func(t *testing.T) {
			wire := wireOf(t, name, req)
			if !strings.Contains(wire, `"format":"mp3"`) {
				t.Errorf("未从 MIME 反推 format：\n%s", wire)
			}
		})
	}
}

// 远端 URL 形态不得写进 base64 槽位：Chat 的 file_data 与 Responses 的
// file_url 是不同字段，混用上游取不到内容。
func TestOutboundCarriesRemoteURL(t *testing.T) {
	req := mediaReq(&ir.Media{Kind: ir.MediaDocument, MediaType: "application/pdf", URL: "https://x/spec.pdf"})
	for _, c := range []struct{ name, want string }{
		{"anthropic", `"url":"https://x/spec.pdf"`},
		{"openai-chat", `"file_data":"https://x/spec.pdf"`},
		{"openai-responses", `"file_url":"https://x/spec.pdf"`},
	} {
		t.Run(c.name, func(t *testing.T) {
			wire := wireOf(t, c.name, req)
			if !strings.Contains(wire, c.want) {
				t.Errorf("缺 %s：\n%s", c.want, wire)
			}
		})
	}
}

// file_id 形态原样透传：它是上游侧已上传文件的引用，本层无法换算成内容。
func TestOutboundCarriesFileID(t *testing.T) {
	req := mediaReq(&ir.Media{Kind: ir.MediaDocument, MediaType: "application/pdf", FileID: "file_9"})
	for _, c := range []struct{ name, want string }{
		{"anthropic", `"file_id":"file_9"`},
		{"openai-chat", `"file_id":"file_9"`},
		{"openai-responses", `"file_id":"file_9"`},
	} {
		t.Run(c.name, func(t *testing.T) {
			wire := wireOf(t, c.name, req)
			if !strings.Contains(wire, c.want) {
				t.Errorf("缺 %s：\n%s", c.want, wire)
			}
		})
	}
}

// 文件名必须落到 wire 上。只做往返断言抓不住它：解码丢掉 title 后编码也不写，
// 两侧一致往返仍然相等——必须对 wire 直接断言。文件名是模型理解「这是什么
// 附件」的主要线索，丢了会让引用型提问（"按 spec.pdf 第三节"）失效。
func TestOutboundKeepsFilename(t *testing.T) {
	req := mediaReq(&ir.Media{
		Kind: ir.MediaDocument, MediaType: "application/pdf", Data: pdfB64, Filename: "spec.pdf",
	})
	for _, c := range []struct{ name, want string }{
		{"anthropic", `"title":"spec.pdf"`},
		{"openai-chat", `"filename":"spec.pdf"`},
		{"openai-responses", `"filename":"spec.pdf"`},
	} {
		t.Run(c.name, func(t *testing.T) {
			wire := wireOf(t, c.name, req)
			if !strings.Contains(wire, c.want) {
				t.Errorf("缺 %s：\n%s", c.want, wire)
			}
		})
	}
}

// 入站也要读到文件名，否则出站无从写出。
func TestInboundKeepsFilename(t *testing.T) {
	for _, c := range []struct{ name, proto, body string }{
		{"anthropic", "anthropic",
			`{"model":"m","messages":[{"role":"user","content":[{"type":"document","title":"spec.pdf","source":{"type":"base64","media_type":"application/pdf","data":"` + pdfB64 + `"}}]}]}`},
		{"openai-chat", "openai-chat",
			`{"model":"m","messages":[{"role":"user","content":[{"type":"file","file":{"filename":"spec.pdf","file_data":"data:application/pdf;base64,` + pdfB64 + `"}}]}]}`},
		{"openai-responses", "openai-responses",
			`{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_file","filename":"spec.pdf","file_data":"data:application/pdf;base64,` + pdfB64 + `"}]}]}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			if m := onlyMedia(t, c.proto, c.body); m.Filename != "spec.pdf" {
				t.Errorf("Filename = %q, want spec.pdf", m.Filename)
			}
		})
	}
}

// Anthropic 的纯文本文档走 source.type=text：上游对 text/plain 不接受
// base64 形态。
func TestAnthropicTextDocumentUsesTextSource(t *testing.T) {
	req := mediaReq(&ir.Media{Kind: ir.MediaDocument, MediaType: "text/plain", Data: "hello"})
	wire := wireOf(t, "anthropic", req)
	if !strings.Contains(wire, `"type":"text"`) || !strings.Contains(wire, `"data":"hello"`) {
		t.Errorf("纯文本文档未走 text source：\n%s", wire)
	}
}

// ---- 出站降级：装不下时换占位文本，绝不静默丢弃、绝不投错槽位 ----

func TestAudioDegradesToPlaceholderOnAnthropic(t *testing.T) {
	req := mediaReq(&ir.Media{Kind: ir.MediaAudio, MediaType: "audio/wav", Data: audB64, Filename: "clip.wav"})
	wire := wireOf(t, "anthropic", req)
	// new-api 正是在这里把音频标成 image 块（to_claude_messages_req.go:261-284），
	// 上游按图片解码后 400。本仓不得复刻。
	if strings.Contains(wire, `"type":"image"`) || strings.Contains(wire, `"type":"document"`) {
		t.Errorf("音频被投进了图片/文档槽位：\n%s", wire)
	}
	if strings.Contains(wire, audB64) {
		t.Errorf("装不下却把 base64 写上了 wire：\n%s", wire)
	}
	if !strings.Contains(wire, "attachment dropped") {
		t.Errorf("静默丢弃，模型会以为用户没给附件：\n%s", wire)
	}
	// 占位文本要说清丢了什么，否则模型无法向用户解释。
	if !strings.Contains(wire, "audio/wav") || !strings.Contains(wire, "clip.wav") {
		t.Errorf("占位文本没说清丢了什么：\n%s", wire)
	}
}

// anthropic 只认 document：其余大类都没有槽位，必须降级为占位文本而不是
// 整块消失。占位文本走的是同一段代码，逐类跑一遍防止某一类漏进 switch 的
// default 之外。
func TestAnthropicDegradesUnsupportedMediaKinds(t *testing.T) {
	for _, k := range []ir.MediaKind{ir.MediaAudio, ir.MediaVideo, ir.MediaOther} {
		t.Run(string(k), func(t *testing.T) {
			req := mediaReq(&ir.Media{Kind: k, MediaType: "application/x-" + string(k), Data: pdfB64})
			wire := wireOf(t, "anthropic", req)
			if strings.Contains(wire, pdfB64) {
				t.Errorf("装不下的附件内容被塞进了 anthropic 载荷：\n%s", wire)
			}
			if !strings.Contains(wire, "attachment dropped") {
				t.Errorf("整块消失：\n%s", wire)
			}
		})
	}
}

// 视频在 OpenAI 两系借 input_file / file 透传而不是降级：它们是不透明容器，
// 原样带过去比换占位文本保留更多信息。
func TestVideoPassesThroughOpenAIFileSlot(t *testing.T) {
	req := mediaReq(&ir.Media{Kind: ir.MediaVideo, MediaType: "video/mp4", Data: audB64})
	for _, name := range []string{"openai-chat", "openai-responses"} {
		t.Run(name, func(t *testing.T) {
			wire := wireOf(t, name, req)
			if strings.Contains(wire, "attachment dropped") {
				t.Errorf("不透明容器装得下却降级了：\n%s", wire)
			}
			if !strings.Contains(wire, audB64) {
				t.Errorf("内容丢失：\n%s", wire)
			}
		})
	}
}

// ---- 往返与跨协议 ----

// 同协议往返必须无损。本仓没有 passthrough 快路径，同协议也走 IR，
// IR 少一维在同协议往返上同样丢。
func TestMediaRoundTripsSameProtocol(t *testing.T) {
	for _, c := range []struct{ name, body string }{
		{"anthropic", `{"model":"m","messages":[{"role":"user","content":[{"type":"document","title":"spec.pdf","source":{"type":"base64","media_type":"application/pdf","data":"` + pdfB64 + `"}}]}]}`},
		{"openai-chat", `{"model":"m","messages":[{"role":"user","content":[{"type":"input_audio","input_audio":{"data":"` + audB64 + `","format":"wav"}}]}]}`},
		{"openai-responses", `{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_file","filename":"spec.pdf","file_data":"data:application/pdf;base64,` + pdfB64 + `"}]}]}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			req, err := proto.MustInbound(c.name).DecodeRequest([]byte(c.body))
			if err != nil {
				t.Fatalf("DecodeRequest: %v", err)
			}
			in := onlyMediaOf(t, req)
			wire := wireOf(t, c.name, req)
			back, err := proto.MustInbound(c.name).DecodeRequest([]byte(wire))
			if err != nil {
				t.Fatalf("二次 DecodeRequest: %v", err)
			}
			out := onlyMediaOf(t, back)
			if *out != *in {
				t.Errorf("往返后变了：\n got %+v\nwant %+v", *out, *in)
			}
		})
	}
}

// 跨协议：Gemini 入站的 PDF 必须到 Anthropic 的 document 槽位，
// 音频必须降级而不是变成 image。这是入站唯一能同时给出两种 MIME 的协议。
func TestGeminiMediaReachesAnthropic(t *testing.T) {
	for _, c := range []struct {
		name, mime string
		wantSlot   string
	}{
		{"pdf", "application/pdf", `"type":"document"`},
		{"audio", "audio/mpeg", "attachment dropped"},
	} {
		t.Run(c.name, func(t *testing.T) {
			body := `{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"` + c.mime + `","data":"` + pdfB64 + `"}}]}]}`
			req, err := proto.MustInbound("gemini").DecodeRequest([]byte(body))
			if err != nil {
				t.Fatalf("DecodeRequest: %v", err)
			}
			wire := wireOf(t, "anthropic", req)
			if !strings.Contains(wire, c.wantSlot) {
				t.Errorf("缺 %s：\n%s", c.wantSlot, wire)
			}
			if c.name == "audio" && strings.Contains(wire, `"type":"image"`) {
				t.Errorf("音频到了图片槽位：\n%s", wire)
			}
		})
	}
}

// tool 结果内嵌的附件同样要投进槽位。OpenAI 两系的 tool 结果只能是字符串，
// 附件必须另起一条 user 消息——漏掉这层会让「工具返回了 PDF」整块消失。
func TestToolResultMediaExtracted(t *testing.T) {
	// 必须给配对的 tool_use，否则 normalize 会把结果当孤儿降级成文本，
	// 测到的就不是这里要测的那条路径了。
	req := &ir.Request{Model: "m",
		Tools: []ir.Tool{{Name: "read", InputSchema: json.RawMessage(`{"type":"object"}`)}},
		Messages: []ir.Message{
			{Role: ir.RoleAssistant, Content: []ir.Block{{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{
				ID: "t1", Name: "read", Input: json.RawMessage(`{}`),
			}}}},
			{Role: ir.RoleUser, Content: []ir.Block{
				{Type: ir.BlockToolResult, ToolResult: &ir.ToolResult{
					ToolUseID: "t1",
					Content: []ir.Block{
						{Type: ir.BlockText, Text: "see attached"},
						{Type: ir.BlockMedia, Media: &ir.Media{
							Kind: ir.MediaDocument, MediaType: "application/pdf", Data: pdfB64,
						}},
					},
				}},
			}},
		}}
	for _, c := range []struct{ name, want string }{
		{"openai-chat", `"type":"file"`},
		{"openai-responses", `"type":"input_file"`},
	} {
		t.Run(c.name, func(t *testing.T) {
			wire := wireOf(t, c.name, req)
			if !strings.Contains(wire, c.want) {
				t.Errorf("tool 结果里的附件消失了，缺 %s：\n%s", c.want, wire)
			}
			if !strings.Contains(wire, pdfB64) {
				t.Errorf("附件内容丢失：\n%s", wire)
			}
		})
	}
}

// 孤儿 tool_result（前面没有配对 tool_use）会被 normalize 降级成纯文本，
// 而 renderToolResult 只拼文本——附件在这条降级路径上整块消失。降级的目的
// 是绕开上游的工具配对约束，不是丢附件。
func TestOrphanToolResultKeepsMedia(t *testing.T) {
	// 必须声明 Tools：否则 StripToolsIfNoTools 先命中，结果在 stripToolContent
	// 里就被转成文本了，fixOrphanToolResults 那条分支根本走不到——这正是
	// 「夹具没走到被测分支」类的假通过。
	req := &ir.Request{Model: "m",
		Tools: []ir.Tool{{Name: "read", InputSchema: json.RawMessage(`{"type":"object"}`)}},
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{
			{Type: ir.BlockToolResult, ToolResult: &ir.ToolResult{
				ToolUseID: "orphan",
				Content: []ir.Block{
					{Type: ir.BlockText, Text: "see attached"},
					{Type: ir.BlockMedia, Media: &ir.Media{
						Kind: ir.MediaDocument, MediaType: "application/pdf", Data: pdfB64,
					}},
				},
			}},
		}}}}
	for _, name := range []string{"anthropic", "openai-chat", "openai-responses"} {
		t.Run(name, func(t *testing.T) {
			wire := wireOf(t, name, req)
			if !strings.Contains(wire, pdfB64) {
				t.Errorf("孤儿结果降级后附件消失：\n%s", wire)
			}
		})
	}
}

// nil 媒体块不得崩、不得静默消失：块类型已是 media 而载荷为空，
// 本身就是该报的丢失。
func TestNilMediaDegradesSafely(t *testing.T) {
	req := mediaReq(nil)
	for _, name := range []string{"anthropic", "openai-chat", "openai-responses"} {
		t.Run(name, func(t *testing.T) {
			wire := wireOf(t, name, req)
			if !strings.Contains(wire, "attachment dropped") {
				t.Errorf("空媒体块静默消失：\n%s", wire)
			}
		})
	}
}

// ---- Clone 往返：Overrides 与重试隔离都走 JSON，媒体维度不能在这里丢 ----

func TestMediaSurvivesClone(t *testing.T) {
	in := &ir.Media{
		Kind: ir.MediaAudio, MediaType: "audio/wav", Data: audB64,
		Filename: "clip.wav", Format: "wav",
	}
	got := onlyMediaOf(t, mediaReq(in).Clone())
	if *got != *in {
		t.Errorf("Clone 后变了：\n got %+v\nwant %+v", *got, *in)
	}
}

// ---- MediaKindOf 分流表 ----

func TestMediaKindOf(t *testing.T) {
	for mime, want := range map[string]ir.MediaKind{
		"application/pdf": ir.MediaDocument,
		"text/plain":      ir.MediaDocument,
		"text/csv":        ir.MediaDocument,
		"audio/wav":       ir.MediaAudio,
		"audio/mpeg":      ir.MediaAudio,
		"video/mp4":       ir.MediaVideo,
		"image/png":       ir.MediaOther, // 图片走 BlockImage，不该进这里
		"":                ir.MediaOther,
		"application/zip": ir.MediaOther,
	} {
		if got := ir.MediaKindOf(mime); got != want {
			t.Errorf("MediaKindOf(%q) = %q, want %q", mime, got, want)
		}
	}
}

// ---- 辅助 ----

func mediaReq(m *ir.Media) *ir.Request {
	return &ir.Request{Model: "m", MaxTokens: 100, Messages: []ir.Message{{
		Role:    ir.RoleUser,
		Content: []ir.Block{{Type: ir.BlockMedia, Media: m}},
	}}}
}

// wireOf 编码后取 wire 字符串，顺带确认产出是合法 JSON——降级路径最容易
// 写出半截结构。
func wireOf(t *testing.T, name string, req *ir.Request) string {
	t.Helper()
	body := encodeReq(t, name, req)
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("%s 产出不是合法 JSON: %v", name, err)
	}
	return string(body)
}

// onlyMedia 解码后取唯一那个媒体块，顺带断言它确实是 BlockMedia
// （此前的失败形态正是「解成了别的块类型」而不是「没有块」）。
func onlyMedia(t *testing.T, protoName, body string) *ir.Media {
	t.Helper()
	req, err := proto.MustInbound(protoName).DecodeRequest([]byte(body))
	if err != nil {
		t.Fatalf("DecodeRequest(%s): %v", protoName, err)
	}
	return onlyMediaOf(t, req)
}

func onlyMediaOf(t *testing.T, req *ir.Request) *ir.Media {
	t.Helper()
	var found []*ir.Media
	for _, m := range req.Messages {
		for _, b := range m.Content {
			if b.Type == ir.BlockMedia {
				found = append(found, b.Media)
			}
		}
	}
	if len(found) != 1 {
		t.Fatalf("media 块数 = %d, want 1；messages=%+v", len(found), req.Messages)
	}
	if found[0] == nil {
		t.Fatal("media 块载荷为 nil")
	}
	return found[0]
}
