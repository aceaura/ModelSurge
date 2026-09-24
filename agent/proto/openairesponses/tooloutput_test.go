package openairesponses

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// R103-4 function_call_output.output 双形态。官方允许字符串或 content part
// 数组；声明成 string 时数组形态让 item 级 Unmarshal 失败——请求侧整单 400，
// 响应侧整条 item 静默蒸发。

// 数组形态（output_text + input_image）进请求：不再 400，文本与图片都进
// ToolResult.Content。
func TestDecodeRequestToolCallOutputArrayForm(t *testing.T) {
	body := []byte(`{"model":"m","input":[` +
		`{"type":"function_call","call_id":"c1","name":"snap","arguments":"{}"},` +
		`{"type":"function_call_output","call_id":"c1","output":[` +
		`{"type":"output_text","text":"shot taken"},` +
		`{"type":"input_image","image_url":"data:image/png;base64,QUJD"}]}]}`)
	req, err := New().DecodeRequest(body)
	if err != nil {
		t.Fatalf("数组形态 output 不应再整单 400：%v", err)
	}
	var tr *ir.ToolResult
	for _, m := range req.Messages {
		for _, b := range m.Content {
			if b.Type == ir.BlockToolResult {
				tr = b.ToolResult
			}
		}
	}
	if tr == nil || tr.ToolUseID != "c1" {
		t.Fatalf("工具结果块丢失：%+v", req.Messages)
	}
	var text string
	var img *ir.Image
	for _, b := range tr.Content {
		switch b.Type {
		case ir.BlockText:
			text += b.Text
		case ir.BlockImage:
			img = b.Image
		}
	}
	if text != "shot taken" {
		t.Errorf("数组形态的文本 part 丢失：%+v", tr.Content)
	}
	if img == nil || img.Data != "QUJD" {
		t.Errorf("数组形态的图片 part 丢失：%+v", tr.Content)
	}
}

// 字符串形态保持原样：单文本块。
func TestDecodeRequestToolCallOutputStringForm(t *testing.T) {
	body := []byte(`{"model":"m","input":[` +
		`{"type":"function_call_output","call_id":"c1","output":"plain"}]}`)
	req, err := New().DecodeRequest(body)
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	var tr *ir.ToolResult
	for _, m := range req.Messages {
		for _, b := range m.Content {
			if b.Type == ir.BlockToolResult {
				tr = b.ToolResult
			}
		}
	}
	if tr == nil || len(tr.Content) != 1 || tr.Content[0].Type != ir.BlockText ||
		tr.Content[0].Text != "plain" {
		t.Fatalf("字符串形态结果块变形：%+v", req.Messages)
	}
}

// 非流式响应 output 里混一条数组形态 function_call_output：兄弟 item（正文）
// 不再被它拖下水，结果块也照常解出。
func TestDecodeResponseToolCallOutputArrayKeepsSiblings(t *testing.T) {
	body := []byte(`{"id":"r1","model":"m","status":"completed","output":[` +
		`{"type":"function_call_output","call_id":"c1","output":[{"type":"output_text","text":"ok"}]},` +
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}]}`)
	resp, err := New().DecodeResponse(body)
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	var sawResult, sawText bool
	for _, b := range resp.Content {
		if b.Type == ir.BlockToolResult && b.ToolResult != nil && b.ToolResult.ToolUseID == "c1" {
			sawResult = true
		}
		if b.Type == ir.BlockText && b.Text == "done" {
			sawText = true
		}
	}
	if !sawResult {
		t.Errorf("数组形态工具结果丢失：%+v", resp.Content)
	}
	if !sawText {
		t.Errorf("兄弟正文 item 被拖下水：%+v", resp.Content)
	}
}

// 编码方向：工具结果文本仍按字符串形态出站（图片另起 user 消息，口径不变）。
func TestEncodeToolCallOutputStringForm(t *testing.T) {
	body, err := New().EncodeRequest(&ir.Request{
		Model: "m", MaxTokens: 64,
		Tools: []ir.Tool{{Name: "f", InputSchema: json.RawMessage(`{"type":"object"}`)}},
		Messages: []ir.Message{
			{Role: ir.RoleAssistant, Content: []ir.Block{
				{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: "c1", Name: "f", Input: json.RawMessage(`{}`)}},
			}},
			{Role: ir.RoleUser, Content: []ir.Block{
				{Type: ir.BlockToolResult, ToolResult: &ir.ToolResult{
					ToolUseID: "c1", Content: []ir.Block{{Type: ir.BlockText, Text: "plain"}}}},
			}},
		},
	})
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if !strings.Contains(string(body), `"type":"function_call_output"`) ||
		!strings.Contains(string(body), `"output":"plain"`) {
		t.Errorf("工具结果没按字符串形态出站：%s", body)
	}
}
