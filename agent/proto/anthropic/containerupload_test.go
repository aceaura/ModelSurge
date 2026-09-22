package anthropic

import (
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// R69：anthropic container_upload 内容块全链路贯通。块形
// {type:"container_upload", file_id}：请求侧把已上传文件送进代码执行容器，
// 响应侧是模型运行代码后产出的文件引用。官方 SDK 对照：messages.ts
// ContainerUploadBlockParam（请求 ContentBlockParam 联合）与
// ContainerUploadBlock（响应 ContentBlock 联合与流式块联合）。
// 独立块型而非并入 BlockMedia：它只有 file_id 没有内容本体，语义是
// 「进容器输入目录」，当成普通附件投递会被上游按内容解码而 400。

func TestContainerUploadDecodeRequest(t *testing.T) {
	body := `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":[
		{"type":"text","text":"run this"},
		{"type":"container_upload","file_id":"file_abc123"}]}]}`
	r, err := codec{}.DecodeRequest([]byte(body))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	blocks := r.Messages[0].Content
	if len(blocks) != 2 {
		t.Fatalf("块数 = %d", len(blocks))
	}
	b := blocks[1]
	if b.Type != ir.BlockContainerUpload || b.ContainerUpload == nil ||
		b.ContainerUpload.FileID != "file_abc123" {
		t.Fatalf("container_upload 块 = %+v", b)
	}
}

func TestContainerUploadEncodeRequest(t *testing.T) {
	req := &ir.Request{Model: "m", MaxTokens: 10,
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{
			{Type: ir.BlockText, Text: "run this"},
			{Type: ir.BlockContainerUpload, ContainerUpload: &ir.ContainerUploadRef{FileID: "file_abc123"}},
		}}}}
	out, err := codec{}.EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if !strings.Contains(string(out), `{"type":"container_upload","file_id":"file_abc123"}`) {
		t.Errorf("container_upload 块回写错：%s", out)
	}
	// 同族往返不漂移。
	back, err := codec{}.DecodeRequest(out)
	if err != nil {
		t.Fatalf("DecodeRequest(往返): %v", err)
	}
	b := back.Messages[0].Content[1]
	if b.Type != ir.BlockContainerUpload || b.ContainerUpload.FileID != "file_abc123" {
		t.Errorf("往返漂移：%+v", b)
	}
}

// 多轮历史：assistant 消息里的 container_upload（模型产出的文件引用）也要保真。
func TestContainerUploadAssistantHistory(t *testing.T) {
	body := `{"model":"m","max_tokens":10,"messages":[
		{"role":"user","content":"make a chart"},
		{"role":"assistant","content":[
			{"type":"text","text":"done"},
			{"type":"container_upload","file_id":"file_out1"}]},
		{"role":"user","content":"thanks"}]}`
	r, err := codec{}.DecodeRequest([]byte(body))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	b := r.Messages[1].Content[1]
	if b.Type != ir.BlockContainerUpload || b.ContainerUpload.FileID != "file_out1" {
		t.Fatalf("assistant 历史块 = %+v", b)
	}
	out, err := codec{}.EncodeRequest(r)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if !strings.Contains(string(out), `"file_id":"file_out1"`) {
		t.Errorf("历史块回写丢失：%s", out)
	}
}

// ---- 响应侧 ----

func TestContainerUploadResponseRoundTrip(t *testing.T) {
	body := `{"id":"msg_1","type":"message","role":"assistant","model":"m",
		"content":[{"type":"text","text":"chart ready"},
		{"type":"container_upload","file_id":"file_chart"}],
		"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
	resp, err := codec{}.DecodeResponse([]byte(body))
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	b := resp.Content[1]
	if b.Type != ir.BlockContainerUpload || b.ContainerUpload == nil ||
		b.ContainerUpload.FileID != "file_chart" {
		t.Fatalf("响应块 = %+v", b)
	}
	out, err := codec{}.EncodeResponse(resp)
	if err != nil {
		t.Fatalf("EncodeResponse: %v", err)
	}
	if !strings.Contains(string(out), `{"type":"container_upload","file_id":"file_chart"}`) {
		t.Errorf("响应回写错：%s", out)
	}
}

// 流式：content_block_start 的 content_block 直接带全形（该块无增量）。
func TestContainerUploadStreamDecode(t *testing.T) {
	d := codec{}.NewStreamDecoder()
	if _, err := d.Feed("message_start", `{"type":"message_start","message":{"id":"msg_1","model":"m","usage":{"input_tokens":1,"output_tokens":1}}}`); err != nil {
		t.Fatalf("Feed start: %v", err)
	}
	evs, err := d.Feed("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"container_upload","file_id":"file_s1"}}`)
	if err != nil || len(evs) != 1 {
		t.Fatalf("Feed block_start: %v %d", err, len(evs))
	}
	ev := evs[0]
	if ev.Type != ir.EvBlockStart || ev.Block == nil ||
		ev.Block.Type != ir.BlockContainerUpload || ev.Block.ContainerUpload.FileID != "file_s1" {
		t.Fatalf("block_start 事件 = %+v", ev)
	}
	if _, err := d.Feed("content_block_stop", `{"type":"content_block_stop","index":0}`); err != nil {
		t.Fatalf("Feed block_stop: %v", err)
	}
	// 聚合收得到块。
	a := ir.NewAggregator()
	a.Feed(ir.Event{Type: ir.EvMessageStart, MessageID: "m"})
	a.Feed(ev)
	a.Feed(ir.Event{Type: ir.EvBlockStop, Index: 0})
	a.Feed(ir.Event{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn})
	got, _ := a.Finish()
	if len(got.Content) != 1 || got.Content[0].Type != ir.BlockContainerUpload ||
		got.Content[0].ContainerUpload.FileID != "file_s1" {
		t.Errorf("聚合丢失：%+v", got.Content)
	}
}

func TestContainerUploadStreamEncode(t *testing.T) {
	e := codec{}.NewStreamEncoder()
	if _, err := e.Encode(ir.Event{Type: ir.EvMessageStart, MessageID: "msg_1", Model: "m"}); err != nil {
		t.Fatalf("Encode start: %v", err)
	}
	frames, err := e.Encode(ir.Event{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{
		Type: ir.BlockContainerUpload, ContainerUpload: &ir.ContainerUploadRef{FileID: "file_s1"},
	}})
	if err != nil || len(frames) != 1 {
		t.Fatalf("Encode block_start: %v %d", err, len(frames))
	}
	if !strings.Contains(string(frames[0]), `"content_block":{"type":"container_upload","file_id":"file_s1"}`) {
		t.Errorf("block_start 帧：%s", frames[0])
	}
	frames, err = e.Encode(ir.Event{Type: ir.EvBlockStop, Index: 0})
	if err != nil || len(frames) != 1 {
		t.Fatalf("Encode block_stop: %v %d", err, len(frames))
	}
	if !strings.Contains(string(frames[0]), `"type":"content_block_stop"`) {
		t.Errorf("block_stop 帧：%s", frames[0])
	}
}
