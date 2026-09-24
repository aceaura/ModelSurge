package proto_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
	"github.com/aceaura/ModelSurge/agent/proto"
)

// R96c：模型产出的附件块（image / document）跨族处置。
// anthropic 是原生形态（image / document 块，含文件名）；gemini 用
// inlineData / fileData 装得下本体（文件名带不回，本仓 blob 无 displayName）；
// OpenAI 两系的助手回合没有附件形态，整块消失。
// 消失必须报出，且不得留下伪造痕迹——responses 此前把附件块落进 blockStart 的
// default(text) 分支，凭空多出一个 content 为 [{"type":"output_text"}] 的空
// message item：客户端读到一条空助手消息，它还占掉一个 output_index。
// gemini 此前流式一律静默丢掉附件，同一份响应按 stream=true/false 内容不同。

const (
	r96cImgData = "SU1H" // base64("IMG")
	r96cDocData = "UEQ=" // base64("PD")
	r96cDocName = "DOC-NAME.pdf"
	r96cText    = "Here is the file."
)

// mediaSlotlessInbound 助手回合没有任何附件形态的入站协议。gemini 不在此列：
// 它把本体编成 inlineData / fileData 投得出去。anthropic 在此列：SDK 响应侧
// ContentBlock 联合（stable 与 beta 均然）没有 image/document 成员，助手回合
// 带不回模型产出的附件本体。按纪律硬编码，不动态取协议清单。
var mediaSlotlessInbound = []string{"openai-chat", "openai-responses", "anthropic"}

func r96cImage() ir.Block {
	return ir.Block{Type: ir.BlockImage, Image: &ir.Image{MediaType: "image/png", Data: r96cImgData}}
}

func r96cDoc() ir.Block {
	return ir.Block{Type: ir.BlockMedia, Media: &ir.Media{
		Kind: ir.MediaDocument, MediaType: "application/pdf",
		Data: r96cDocData, Filename: r96cDocName,
	}}
}

func r96cResp() *ir.Response {
	return &ir.Response{ID: "r", Model: "m", StopReason: ir.StopEndTurn,
		Content: []ir.Block{r96cImage(), r96cDoc(), {Type: ir.BlockText, Text: r96cText}}}
}

// r96cStream 附件在块开始时整块到达（没有增量形态），正文随后。
func r96cStream() []ir.Event {
	img, doc := r96cImage(), r96cDoc()
	return []ir.Event{
		{Type: ir.EvMessageStart, MessageID: "msg", Model: "m"},
		{Type: ir.EvBlockStart, Index: 0, Block: &img},
		{Type: ir.EvBlockStop, Index: 0},
		{Type: ir.EvBlockStart, Index: 1, Block: &doc},
		{Type: ir.EvBlockStop, Index: 1},
		{Type: ir.EvBlockStart, Index: 2, Block: &ir.Block{Type: ir.BlockText}},
		{Type: ir.EvTextDelta, Index: 2, Text: r96cText},
		{Type: ir.EvBlockStop, Index: 2},
		{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn, Usage: &ir.Usage{OutputTokens: 3}},
		{Type: ir.EvMessageStop},
	}
}

// 注记措辞对两个计数必须不对称，否则计数接反测不出来。
func TestMediaOutputDropNoteIsAsymmetric(t *testing.T) {
	if proto.MediaOutputDropNote(2, 1) == proto.MediaOutputDropNote(1, 2) {
		t.Fatal("注记措辞对图片/附件两个计数对称，测不出计数接反")
	}
	if got := proto.MediaOutputDropNote(2, 0); !strings.Contains(got, "2 image(s)") || strings.Contains(got, "attachment(s)") {
		t.Errorf("只丢图片时主语不对：%q", got)
	}
	if got := proto.MediaOutputDropNote(0, 3); !strings.Contains(got, "3 non-image attachment(s)") || strings.Contains(got, "image(s) and") {
		t.Errorf("只丢附件时主语不对：%q", got)
	}
	if got := proto.MediaOutputDropNote(1, 1); !strings.Contains(got, "1 image(s)") || !strings.Contains(got, "1 non-image attachment(s)") {
		t.Errorf("两者都丢时主语不全：%q", got)
	}
}

// OpenAI 两系流式：附件本体与文件名都不上线，正文不受影响。
func TestModelAttachmentsNotLeakedIntoForeignStream(t *testing.T) {
	for _, name := range mediaSlotlessInbound {
		t.Run(name, func(t *testing.T) {
			out := streamOut(t, name, r96cStream())
			for _, leak := range []string{r96cImgData, r96cDocData, r96cDocName, "image/png", "application/pdf"} {
				if strings.Contains(out, leak) {
					t.Errorf("附件内容 %q 泄漏进流：\n%s", leak, out)
				}
			}
			if !strings.Contains(out, r96cText) {
				t.Errorf("正文被误删：\n%s", out)
			}
		})
	}
}

// responses 的 blockStart 有 default(text) 兜底：不显式拦住附件块，客户端会凭空
// 多出两条空 message item（增量帧与 response.completed 的全量 output 里都有），
// 看起来像模型说了两句空话，output_index 也被它们推后。
func TestModelAttachmentsAbsentFromResponsesOutput(t *testing.T) {
	out := streamOut(t, "openai-responses", r96cStream())
	if n := strings.Count(out, `"type":"response.output_item.added"`); n != 1 {
		t.Errorf("output_item.added 应只有正文一条，实得 %d（多出的是幽灵空 message）：\n%s", n, out)
	}
	if strings.Contains(out, `"content":[{"type":"output_text"}]`) {
		t.Errorf("凭空多出空 output_text 壳：\n%s", out)
	}
	// 全量 output 与帧里出现的 output_index 必须一一对应且稠密。
	const done = `data: {"type":"response.completed"`
	idx := strings.LastIndex(out, done)
	if idx < 0 {
		t.Fatalf("没有 response.completed：\n%s", out)
	}
	frame := out[idx+len("data: "):]
	if end := strings.Index(frame, "\n\n"); end >= 0 {
		frame = frame[:end]
	}
	var ev struct {
		Response struct {
			Output []json.RawMessage `json:"output"`
		} `json:"response"`
	}
	if err := json.Unmarshal([]byte(frame), &ev); err != nil {
		t.Fatalf("终止帧不是合法 JSON：%q（%v）", frame, err)
	}
	if len(ev.Response.Output) != 1 {
		t.Fatalf("全量 output 应只有正文一条，实得 %d：%s", len(ev.Response.Output), frame)
	}
	if !strings.Contains(out, `"output_index":0`) {
		t.Errorf("正文块应占 output_index 0（附件不占序号）：\n%s", out)
	}
	if strings.Contains(out, `"output_index":1`) || strings.Contains(out, `"output_index":2`) {
		t.Errorf("附件块烧掉了 output_index，客户端按它索引全量 output 会越界：\n%s", out)
	}
}

// chat 侧：被跳过的块不得留下任何分片。整条流应只剩「开场 role + 正文 + 终止」
// 三个 chunk。
func TestModelAttachmentsNoGhostChatDelta(t *testing.T) {
	out := streamOut(t, "openai-chat", r96cStream())
	if n := strings.Count(out, `"object":"chat.completion.chunk"`); n != 3 {
		t.Errorf("chunk 数应为 3（role/正文/终止），实得 %d：\n%s", n, out)
	}
	if strings.Contains(out, `"content":""`) {
		t.Errorf("凭空多出空 content 分片：\n%s", out)
	}
	if strings.Count(out, r96cText) != 1 {
		t.Errorf("正文分片数不对：\n%s", out)
	}
}

// gemini 流式必须把本体投出去：inlineData 装 base64，与它自己的非流式编码同源。
// 此前流式一律静默丢掉，同一份响应 stream=true/false 内容不同。
func TestGeminiStreamDeliversAttachments(t *testing.T) {
	out, notes := streamOutNotes(t, "gemini", r96cStream())
	for _, want := range []string{`"inlineData"`, `"mimeType":"image/png"`, r96cImgData, `"mimeType":"application/pdf"`, r96cDocData, r96cText} {
		if !strings.Contains(out, want) {
			t.Errorf("gemini 流式缺 %q：\n%s", want, out)
		}
	}
	if len(notes) != 0 {
		t.Errorf("本体投得出去却报了损耗：%v", notes)
	}
	// 文件名带不回：本仓 blob 没有 displayName 字段。这条丢失当前不报，
	// 但不得凭空伪造一个槽位把它塞进去。
	if strings.Contains(out, r96cDocName) {
		t.Errorf("gemini 出现了非本族形态的文件名：%s", out)
	}
	// 与非流式同一判定：两种模式都得带上本体。
	body, err := proto.MustInbound("gemini").EncodeResponse(r96cResp())
	if err != nil {
		t.Fatalf("EncodeResponse: %v", err)
	}
	for _, want := range []string{r96cImgData, r96cDocData} {
		if !strings.Contains(string(body), want) {
			t.Errorf("gemini 非流式缺 %q：%s", want, body)
		}
	}
}

// 只有远端 URI 的附件走 fileData（inlineData 需要 base64 本体）。
func TestGeminiStreamDeliversRemoteAttachments(t *testing.T) {
	events := []ir.Event{
		{Type: ir.EvMessageStart, MessageID: "msg", Model: "m"},
		{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockImage,
			Image: &ir.Image{MediaType: "image/png", URL: "https://example.com/a.png"}}},
		{Type: ir.EvBlockStop, Index: 0},
		{Type: ir.EvBlockStart, Index: 1, Block: &ir.Block{Type: ir.BlockMedia,
			Media: &ir.Media{Kind: ir.MediaDocument, MediaType: "application/pdf", URL: "https://example.com/b.pdf"}}},
		{Type: ir.EvBlockStop, Index: 1},
		{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn},
		{Type: ir.EvMessageStop},
	}
	out, notes := streamOutNotes(t, "gemini", events)
	if n := strings.Count(out, `"fileData"`); n != 2 {
		t.Errorf("两个远端附件应各占一个 fileData part，实得 %d：\n%s", n, out)
	}
	for _, want := range []string{"https://example.com/a.png", "https://example.com/b.pdf"} {
		if !strings.Contains(out, want) {
			t.Errorf("缺 URI %q：\n%s", want, out)
		}
	}
	if len(notes) != 0 {
		t.Errorf("投得出去却报了损耗：%v", notes)
	}
}

// 纯 file_id 引用（Anthropic 的 document source.type=file）既无本体也无 URI，
// gemini 两个槽位都装不下：不得伪造空 inlineData，且必须报出。
func TestGeminiUndeliverableAttachmentReported(t *testing.T) {
	want := proto.MediaOutputDropNote(0, 1)
	refOnly := &ir.Block{Type: ir.BlockMedia, Media: &ir.Media{
		Kind: ir.MediaDocument, MediaType: "application/pdf", FileID: "file_abc123"}}
	events := []ir.Event{
		{Type: ir.EvMessageStart, MessageID: "msg", Model: "m"},
		{Type: ir.EvBlockStart, Index: 0, Block: refOnly},
		{Type: ir.EvBlockStop, Index: 0},
		{Type: ir.EvBlockStart, Index: 1, Block: &ir.Block{Type: ir.BlockText}},
		{Type: ir.EvTextDelta, Index: 1, Text: r96cText},
		{Type: ir.EvBlockStop, Index: 1},
		{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn},
		{Type: ir.EvMessageStop},
	}
	out, notes := streamOutNotes(t, "gemini", events)
	if !slices.Contains(notes, want) {
		t.Fatalf("流式未报附件损耗：notes=%q", notes)
	}
	if strings.Contains(out, "file_abc123") || strings.Contains(out, `"data":""`) {
		t.Errorf("伪造了空附件 part 或泄漏了 file_id：\n%s", out)
	}
	if !strings.Contains(out, r96cText) {
		t.Errorf("正文被误删：\n%s", out)
	}
	// 非流式同一判定：mediaParts 装不下就报，且正文里不出现空 inlineData。
	resp := &ir.Response{ID: "r", Model: "m", StopReason: ir.StopEndTurn,
		Content: []ir.Block{*refOnly, {Type: ir.BlockText, Text: r96cText}}}
	got := proto.MustInbound("gemini").ResponseNotes(resp)
	if !slices.Contains(got, want) {
		t.Errorf("非流式未报附件损耗：%q", got)
	}
	body, err := proto.MustInbound("gemini").EncodeResponse(resp)
	if err != nil {
		t.Fatalf("EncodeResponse: %v", err)
	}
	if strings.Contains(string(body), `"inlineData"`) || strings.Contains(string(body), "file_abc123") {
		t.Errorf("非流式伪造了附件 part：%s", body)
	}
	if !strings.Contains(string(body), r96cText) {
		t.Errorf("正文被误删：%s", body)
	}
}

// OpenAI 两系流式注记：两块合成一条，计数分列，会话内容不进注记。
func TestModelAttachmentsReportedInForeignStreamNotes(t *testing.T) {
	want := proto.MediaOutputDropNote(1, 1)
	for _, name := range mediaSlotlessInbound {
		t.Run(name, func(t *testing.T) {
			_, notes := streamOutNotes(t, name, r96cStream())
			if !slices.Contains(notes, want) {
				t.Fatalf("流式未报附件损耗：notes=%q", notes)
			}
			for _, leak := range []string{r96cImgData, r96cDocData, r96cDocName} {
				if strings.Contains(strings.Join(notes, "; "), leak) {
					t.Errorf("注记带出了会话内容 %q：%q", leak, notes)
				}
			}
		})
	}
	// 幂等排干：报过就不再报。
	enc := proto.MustInbound("openai-chat").NewStreamEncoder()
	for _, ev := range r96cStream() {
		if _, err := enc.Encode(ev); err != nil {
			t.Fatalf("Encode(%s): %v", ev.Type, err)
		}
	}
	if notes := enc.Notes(); len(notes) == 0 {
		t.Fatal("首次 Notes 应报出损耗")
	}
	if again := enc.Notes(); len(again) != 0 {
		t.Errorf("Notes 应幂等排干：%v", again)
	}
}

// 装得下本体的协议不得报：gemini 用 inlineData 投得出去。误报会让排障的人
// 去查一个并不存在的丢失。（anthropic 已归 mediaSlotlessInbound：其响应侧
// ContentBlock 联合没有 image/document 成员，带不回本体——见 TestModelAttachmentsResponseNotes。）
func TestModelAttachmentsSilentWhereDeliverable(t *testing.T) {
	for _, name := range []string{"gemini"} {
		t.Run(name, func(t *testing.T) {
			_, notes := streamOutNotes(t, name, r96cStream())
			if len(notes) != 0 {
				t.Errorf("%s 误报附件损耗：%v", name, notes)
			}
			if got := proto.MustInbound(name).ResponseNotes(r96cResp()); len(got) != 0 {
				t.Errorf("%s 非流式误报附件损耗：%v", name, got)
			}
		})
	}
}

// 非流式响应侧：ScanResponseLosses 报出，线上不出现本体，正文保留。
func TestModelAttachmentsResponseNotes(t *testing.T) {
	want := proto.MediaOutputDropNote(1, 1)
	for _, name := range mediaSlotlessInbound {
		t.Run(name, func(t *testing.T) {
			got := proto.MustInbound(name).ResponseNotes(r96cResp())
			if !slices.Contains(got, want) {
				t.Fatalf("%s 未报附件损耗：%q", name, got)
			}
			body, err := proto.MustInbound(name).EncodeResponse(r96cResp())
			if err != nil {
				t.Fatalf("EncodeResponse: %v", err)
			}
			for _, leak := range []string{r96cImgData, r96cDocData, r96cDocName} {
				if strings.Contains(string(body), leak) {
					t.Errorf("报了损耗却又把 %q 编进了响应：%s", leak, body)
				}
			}
			if !strings.Contains(string(body), r96cText) {
				t.Errorf("报了损耗却顺手删了正文：%s", body)
			}
		})
	}
}

// 没有附件块时四族全静默：注记描述的是实际没交付的东西，不是静态能力差。
func TestNoAttachmentsNoMediaNote(t *testing.T) {
	resp := &ir.Response{ID: "r", Model: "m", StopReason: ir.StopEndTurn,
		Content: []ir.Block{{Type: ir.BlockText, Text: r96cText}}}
	for _, name := range []string{"anthropic", "openai-chat", "openai-responses", "gemini"} {
		t.Run(name, func(t *testing.T) {
			for _, n := range proto.MustInbound(name).ResponseNotes(resp) {
				if strings.Contains(n, "from the model output") {
					t.Errorf("%s 无附件误报：%q", name, n)
				}
			}
			events := []ir.Event{
				{Type: ir.EvMessageStart, MessageID: "msg", Model: "m"},
				{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockText}},
				{Type: ir.EvTextDelta, Index: 0, Text: r96cText},
				{Type: ir.EvBlockStop, Index: 0},
				{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn},
				{Type: ir.EvMessageStop},
			}
			_, notes := streamOutNotes(t, name, events)
			for _, n := range notes {
				if strings.Contains(n, "from the model output") {
					t.Errorf("%s 流式无附件误报：%q", name, n)
				}
			}
		})
	}
}

// 聚合路径（上游流式 -> 客户端非流式）：附件块要活着走到 EncodeResponse，
// 否则注记报的是「编码器丢了」，实际是聚合器先丢了。
func TestModelAttachmentsSurviveAggregation(t *testing.T) {
	agg := ir.NewAggregator()
	for _, ev := range r96cStream() {
		agg.Feed(ev)
	}
	resp, rerr := agg.Finish()
	if rerr != nil {
		t.Fatalf("聚合报错：%v", rerr)
	}
	if len(resp.Content) != 3 {
		t.Fatalf("聚合后应剩 3 块（图片/文档/正文），实得 %d", len(resp.Content))
	}
	if resp.Content[0].Type != ir.BlockImage || resp.Content[0].Image == nil ||
		resp.Content[0].Image.Data != r96cImgData {
		t.Errorf("图片块聚合后失真：%+v", resp.Content[0])
	}
	if resp.Content[1].Type != ir.BlockMedia || resp.Content[1].Media == nil ||
		resp.Content[1].Media.Data != r96cDocData || resp.Content[1].Media.Filename != r96cDocName {
		t.Errorf("文档块聚合后失真：%+v", resp.Content[1])
	}
	if !slices.Contains(proto.MustInbound("openai-chat").ResponseNotes(resp), proto.MediaOutputDropNote(1, 1)) {
		t.Error("聚合后的响应没被扫描出附件损耗")
	}
}
