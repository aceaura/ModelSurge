package proto_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
	"github.com/aceaura/ModelSurge/agent/proto"
	_ "github.com/aceaura/ModelSurge/agent/proto/anthropic"
	_ "github.com/aceaura/ModelSurge/agent/proto/kiro"
	_ "github.com/aceaura/ModelSurge/agent/proto/openaichat"
	_ "github.com/aceaura/ModelSurge/agent/proto/openairesponses"
)

// 工具参数完整性的四协议横切。带一轮完整工具调用历史的请求，参数取
// 各档病态值，断言两条底线：
//  1. 工具调用块本身不能消失（此前 anthropic 在截断 JSON 下整条 content
//     marshal 失败，块连同正文一起蒸发，留下孤立 tool_result）；
//  2. 非法/非对象参数不得静默变 {}（那会让工具不带参数执行）。

func toolHistoryRequest(t *testing.T, input string) *ir.Request {
	t.Helper()
	return &ir.Request{Model: "m",
		Tools: []ir.Tool{{Name: "f", InputSchema: json.RawMessage(`{"type":"object"}`)}},
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}},
			{Role: ir.RoleAssistant, Content: []ir.Block{{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: "t1", Name: "f", Input: json.RawMessage(input)}}}},
			{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockToolResult, ToolResult: &ir.ToolResult{ToolUseID: "t1", Content: []ir.Block{{Type: ir.BlockText, Text: "ok"}}}}}},
		}}
}

func TestMalformedToolArgsNeverVanish(t *testing.T) {
	for _, in := range []string{`{"city": "Par`, `"juststring"`, `[1,2]`, `null`, `123`} {
		for _, name := range proto.OutboundNames() {
			c := proto.MustOutbound(name)
			body, err := c.EncodeRequest(toolHistoryRequest(t, in))
			if err != nil {
				t.Errorf("in=%s %s: EncodeRequest err=%v", in, name, err)
				continue
			}
			if !json.Valid(body) {
				t.Errorf("in=%s %s: 请求体不是合法 JSON", in, name)
			}
			// 调用与结果必须成对出现：id "t1" 在 assistant 侧与 tool 侧各一次。
			if n := strings.Count(string(body), `"t1"`); n < 2 {
				t.Errorf("in=%s %s: t1 只出现 %d 次，调用块疑似被丢: %s", in, name, n, body)
			}
			// 任何协议都不得把非法参数静默清空成空对象。
			if strings.Contains(string(body), `"input":{}`) || strings.Contains(string(body), `"args":{}`) ||
				strings.Contains(string(body), `"arguments":"{}"`) {
				t.Errorf("in=%s %s: 非法参数被静默清空: %s", in, name, body)
			}
		}
	}
}

// 对象槽位协议（anthropic/kiro）非法参数挪进 RawArgsKey 键位，原文可查；
// 字符串槽位协议（chat/responses）原样透传，协议允许任意字符串。
func TestMalformedToolArgsDirectionalHandling(t *testing.T) {
	const truncated = `{"city": "Par`
	for _, name := range []string{"anthropic", "kiro"} {
		body, _ := proto.MustOutbound(name).EncodeRequest(toolHistoryRequest(t, truncated))
		s := string(body)
		if !strings.Contains(s, ir.RawArgsKey) {
			t.Errorf("%s: 截断参数没挪进 %s: %s", name, ir.RawArgsKey, s)
		}
		if !strings.Contains(s, "city") {
			t.Errorf("%s: 原文片段丢了: %s", name, s)
		}
	}
	for _, name := range []string{"openai-chat", "openai-responses"} {
		body, _ := proto.MustOutbound(name).EncodeRequest(toolHistoryRequest(t, truncated))
		// arguments 是字符串槽位，原文逐字符（经 JSON 转义后）出现在值里。
		if !strings.Contains(string(body), `\"city\": \"Par`) {
			t.Errorf("%s: 字符串槽位应原样透传原文: %s", name, body)
		}
	}
}

// 合法对象必须保真到所有四个出站——规整只许动病态输入。
// 按语义断言（管线的 normalize 会做空白压缩，字节级断言是在测别人的职责）。
func TestValidToolArgsPassThroughVerbatim(t *testing.T) {
	const args = `{"city": "Paris", "n": 2}`
	for _, name := range proto.OutboundNames() {
		body, _ := proto.MustOutbound(name).EncodeRequest(toolHistoryRequest(t, args))
		// 字符串槽位里 JSON 会再转义一层，剥掉反斜杠后统一比对。
		plain := strings.ReplaceAll(string(body), `\`, "")
		if !strings.Contains(plain, `"Paris"`) || !strings.Contains(plain, `"n":2`) {
			t.Errorf("%s: 合法参数被改写: %s", name, body)
		}
		if strings.Contains(string(body), ir.RawArgsKey) {
			t.Errorf("%s: 合法参数被误挪键: %s", name, body)
		}
	}
}

// 聚合路径：流式收来的截断参数在 IR 响应里就带着 RawArgsKey，
// 客户端（非流式）能看到原文而不是拿到空参数。
func TestTruncatedStreamedArgsReachClient(t *testing.T) {
	agg := ir.NewAggregator()
	agg.Feed(ir.Event{Type: ir.EvMessageStart, MessageID: "m1"})
	agg.Feed(ir.Event{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: "t1", Name: "f"}}})
	agg.Feed(ir.Event{Type: ir.EvToolInput, Index: 0, Text: `{"city": "Par`})
	agg.Feed(ir.Event{Type: ir.EvBlockStop, Index: 0})
	agg.Feed(ir.Event{Type: ir.EvMessageDelta, StopReason: ir.StopMaxTokens})
	agg.Feed(ir.Event{Type: ir.EvMessageStop})
	resp, _ := agg.Finish()
	in := resp.Content[0].ToolUse.Input
	if !strings.Contains(string(in), ir.RawArgsKey) {
		t.Errorf("聚合后截断原文丢失: %s", in)
	}
	// 非流式编码给客户端：input 是对象且不塌空。kiro 的 EncodeResponse 是
	// 每行一条事件的 NDJSON，不是单个 JSON 文档，逐行校验。
	for _, name := range proto.InboundNames() {
		body, err := proto.MustInbound(name).EncodeResponse(resp)
		if err != nil {
			t.Errorf("%s: EncodeResponse err=%v", name, err)
			continue
		}
		if name == "kiro" {
			for line := range strings.Lines(string(body)) {
				if strings.TrimSpace(line) != "" && !json.Valid([]byte(line)) {
					t.Errorf("kiro: 事件行不是合法 JSON: %s", line)
				}
			}
			continue
		}
		if !json.Valid(body) {
			t.Errorf("%s: 响应体不是合法 JSON", name)
		}
	}
}

// 绕过聚合器直接构造病态响应（防御纵深）：EncodeResponse 不能只靠上游
// 规整兜底，自己拿到病态 input 也必须产出合法响应且原文挪键。
func TestEncodeResponseSanitizesDirectly(t *testing.T) {
	resp := &ir.Response{ID: "m1", Model: "m", StopReason: ir.StopMaxTokens,
		Content: []ir.Block{{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: "t1", Name: "f", Input: json.RawMessage(`{"city": "Par`)}}}}
	for _, name := range proto.InboundNames() {
		body, err := proto.MustInbound(name).EncodeResponse(resp)
		if err != nil {
			t.Errorf("%s: EncodeResponse err=%v", name, err)
			continue
		}
		if name == "kiro" {
			for line := range strings.Lines(string(body)) {
				if strings.TrimSpace(line) != "" && !json.Valid([]byte(line)) {
					t.Fatalf("kiro: 事件行不是合法 JSON: %s", line)
				}
			}
			if !strings.Contains(string(body), ir.RawArgsKey) {
				t.Errorf("kiro: 原文没挪键: %s", body)
			}
			continue
		}
		if !json.Valid(body) {
			t.Errorf("%s: 响应体不是合法 JSON: %s", name, body)
		}
		// 对象槽位挪键保真；字符串槽位（chat/responses）原样透传，原文逐字可查。
		if name == "anthropic" || name == "gemini" {
			if !strings.Contains(string(body), ir.RawArgsKey) {
				t.Errorf("%s: 原文没挪键: %s", name, body)
			}
		} else if !strings.Contains(string(body), `\"city\": \"Par`) {
			t.Errorf("%s: 字符串槽位原文没透传: %s", name, body)
		}
	}
}
