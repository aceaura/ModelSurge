package anthropic

import (
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// R91：Anthropic 未知块与同名字段形状冲突的全链路保真。两个缺陷同源：
//
//  1. 未知块降级成文本。Anthropic 有一整族服务端工具结果块（web_fetch /
//     code_execution / bash_code_execution / text_editor_code_execution /
//     tool_search）与 mid_conv_system、search_result，全都没有 text 字段。
//     降级过去等于把抓取的网页正文、stdout、文件内容换成一个空文本块，而兄弟
//     server_tool_use 块还留在原地——发给上游的 tool_use/tool_result 配平当场
//     断裂，下一轮可能被整轮拒掉。
//  2. content 数组一次性解析。Anthropic 在不同块型上复用同一个键名承载不同形状：
//     citations 在 text 块上是引用数组、在 document 块上是 {"enabled":bool}；
//     source 在 document 块上是对象、在 search_result 块上是字符串。任一块冲突
//     就让整个 Unmarshal 失败，同消息的其他块（包括用户真正在问的那句话）随之
//     全部蒸发，调用方只拿到一个 nil：既没有错误，也没有损耗注记。
//
// 官方 SDK 对照：types/content_block_param.py（17 类型）与 types/content_block.py
// （12 类型）、types/citations_config.py、types/search_result_block_param.py。

// r91ServerToolResults 五种服务端工具结果块的 wire 原文。紧凑书写：不透明块的
// Body 是 json.RawMessage，逐字节保留输入，断言可直接用 strings.Contains 比对。
var r91ServerToolResults = []struct{ name, body string }{
	{"web_fetch_tool_result", `{"type":"web_fetch_tool_result","tool_use_id":"tfu_1","content":{"type":"web_fetch_result","content":[{"type":"text","text":"fetched page body"}]}}`},
	{"code_execution_tool_result", `{"type":"code_execution_tool_result","tool_use_id":"tfu_2","content":{"type":"code_execution_output","content":[{"type":"text","text":"stdout here"}]}}`},
	{"bash_code_execution_tool_result", `{"type":"bash_code_execution_tool_result","tool_use_id":"tfu_3","content":{"type":"bash_code_execution_output","stdout":"ls -la\n","stderr":"","exit_code":0}}`},
	{"text_editor_code_execution_tool_result", `{"type":"text_editor_code_execution_tool_result","tool_use_id":"tfu_4","content":{"type":"text_editor_view","file":{"path":"a.go","content":"pkg main"}}}`},
	{"tool_search_tool_result", `{"type":"tool_search_tool_result","tool_use_id":"tfu_5","content":{"type":"tool_search_tool_search_result","tools":[]}}`},
}

func r91Opaque(wireType, body string) ir.Block {
	return ir.Block{Type: ir.BlockOpaque,
		Opaque: &ir.Opaque{WireType: wireType, Body: []byte(body)}}
}

// ---- 响应侧：未知块原样往返，不降级成空文本 ----

func TestOpaqueServerToolResultResponseRoundTrip(t *testing.T) {
	for _, tc := range r91ServerToolResults {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"id":"msg_1","type":"message","role":"assistant","model":"claude-x","content":[` + tc.body + `]}`
			resp, err := codec{}.DecodeResponse([]byte(body))
			if err != nil {
				t.Fatalf("DecodeResponse: %v", err)
			}
			if len(resp.Content) != 1 {
				t.Fatalf("块数 = %d，want 1", len(resp.Content))
			}
			b := resp.Content[0]
			if b.Type != ir.BlockOpaque {
				t.Fatalf("块型 = %q，want opaque", b.Type)
			}
			if b.Opaque == nil {
				t.Fatal("opaque 载荷为 nil")
			}
			if b.Opaque.WireType != tc.name {
				t.Errorf("wire 判别值 = %q，want %q", b.Opaque.WireType, tc.name)
			}
			if string(b.Opaque.Body) != tc.body {
				t.Errorf("块体未逐字节保留：\n got %s\nwant %s", b.Opaque.Body, tc.body)
			}
			// 降级形态是一个只有 type 的裸文本块：网页正文、stdout、文件内容全丢。
			if b.Text != "" {
				t.Errorf("不透明块不该带文本：%q", b.Text)
			}
			out, err := codec{}.EncodeResponse(resp)
			if err != nil {
				t.Fatalf("EncodeResponse: %v", err)
			}
			if !strings.Contains(string(out), tc.body) {
				t.Errorf("块体未原样回写：%s", out)
			}
			if strings.Contains(string(out), `"type":"text"}`) {
				t.Errorf("响应侧被降级成空文本块：%s", out)
			}
		})
	}
}

// 块体里 block 没建模的键必须跟着回来：web_fetch_tool_result 的 caller、
// search_result 的 citations 配置都是逐字段重建会丢掉的东西。
func TestOpaqueKeepsUnmodeledKeys(t *testing.T) {
	raw := `{"type":"web_fetch_tool_result","tool_use_id":"tfu_9","caller":{"type":"web_search_tool_result","tool_use_id":"tfu_8"},"content":{"type":"web_fetch_result","content":[{"type":"text","text":"body"}]}}`
	body := `{"id":"msg_1","type":"message","role":"assistant","model":"claude-x","content":[` + raw + `]}`
	resp, err := codec{}.DecodeResponse([]byte(body))
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	out, err := codec{}.EncodeResponse(resp)
	if err != nil {
		t.Fatalf("EncodeResponse: %v", err)
	}
	if !strings.Contains(string(out), `"caller"`) {
		t.Errorf("未建模的 caller 键被丢掉：%s", out)
	}
	if !strings.Contains(string(out), raw) {
		t.Errorf("块体漂移：\n got %s\nwant %s", out, raw)
	}
}

// ---- 请求侧：历史里的未知块回传，tool_use/tool_result 配平不断 ----

func TestOpaqueInHistoryKeepsServerToolUsePaired(t *testing.T) {
	fetch := r91ServerToolResults[0].body
	reqBody := `{"model":"claude-x","max_tokens":16,"messages":[
		{"role":"user","content":[{"type":"text","text":"fetch it"}]},
		{"role":"assistant","content":[
			{"type":"server_tool_use","id":"tfu_1","name":"web_fetch","input":{"url":"https://example.com"}},
			` + fetch + `]},
		{"role":"user","content":[{"type":"text","text":"thanks"}]}]}`
	req, err := codec{}.DecodeRequest([]byte(reqBody))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	blocks := req.Messages[1].Content
	if len(blocks) != 2 {
		t.Fatalf("assistant 块数 = %d，want 2", len(blocks))
	}
	if blocks[0].Type != ir.BlockServerToolUse {
		t.Errorf("block[0] = %q，want server_tool_use", blocks[0].Type)
	}
	if blocks[1].Type != ir.BlockOpaque || blocks[1].Opaque == nil ||
		blocks[1].Opaque.WireType != "web_fetch_tool_result" {
		t.Fatalf("block[1] = %+v，want opaque/web_fetch_tool_result", blocks[1])
	}
	out, err := codec{}.EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	s := string(out)
	// 配平：server_tool_use 与它的结果块必须同时在线上，缺一边上游按配平校验拒整轮。
	if !strings.Contains(s, `"type":"server_tool_use"`) || !strings.Contains(s, `"id":"tfu_1"`) {
		t.Errorf("server_tool_use 丢失：%s", s)
	}
	if !strings.Contains(s, fetch) {
		t.Errorf("结果块未原样回传：%s", s)
	}
	if strings.Contains(s, `"type":"text"}`) {
		t.Errorf("结果块被降级成空文本块：%s", s)
	}
	// 同族往返不漂移。
	back, err := codec{}.DecodeRequest(out)
	if err != nil {
		t.Fatalf("DecodeRequest(往返): %v", err)
	}
	got := back.Messages[1].Content[1]
	if got.Type != ir.BlockOpaque || string(got.Opaque.Body) != fetch {
		t.Errorf("同族往返漂移：%+v", got)
	}
}

// mid_conv_system：会话中途的系统指令块。降级成裸文本会把指令混进用户内容，
// 而它自己的 content 数组被丢掉后指令正文（"be terse"）也一起消失。
func TestOpaqueMidConvSystemRoundTrip(t *testing.T) {
	mid := `{"type":"mid_conv_system","content":[{"type":"text","text":"be terse"}]}`
	reqBody := `{"model":"claude-x","max_tokens":16,"messages":[
		{"role":"user","content":[` + mid + `,{"type":"text","text":"hi"}]}]}`
	req, err := codec{}.DecodeRequest([]byte(reqBody))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	blocks := req.Messages[0].Content
	if len(blocks) != 2 {
		t.Fatalf("块数 = %d，want 2", len(blocks))
	}
	if blocks[0].Type != ir.BlockOpaque || blocks[0].Opaque.WireType != "mid_conv_system" {
		t.Errorf("block[0] = %+v，want opaque/mid_conv_system", blocks[0])
	}
	if blocks[1].Type != ir.BlockText || blocks[1].Text != "hi" {
		t.Errorf("兄弟文本块被牵连：%+v", blocks[1])
	}
	out, err := codec{}.EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, mid) {
		t.Errorf("mid_conv_system 未原样回传：%s", s)
	}
	if !strings.Contains(s, "be terse") {
		t.Errorf("指令正文丢失：%s", s)
	}
	if strings.Contains(s, `"type":"text"}`) {
		t.Errorf("凭空多出空文本块：%s", s)
	}
}

// ---- 同名字段形状冲突：不得牵连兄弟块 ----

// document.citations 是 {"enabled":bool} 配置对象，与 text 块上的引用数组同名
// 不同形。声明成数组时这种块会让整条 content 的 Unmarshal 直接失败。
func TestDocumentCitationsConfigDoesNotDestroyMessage(t *testing.T) {
	reqBody := `{"model":"claude-x","max_tokens":16,"messages":[{"role":"user","content":[
		{"type":"document","title":"a.pdf","citations":{"enabled":true},
		 "source":{"type":"text","media_type":"text/plain","data":"hello world"}},
		{"type":"text","text":"quote it"}]}]}`
	req, err := codec{}.DecodeRequest([]byte(reqBody))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	blocks := req.Messages[0].Content
	if len(blocks) != 2 {
		t.Fatalf("块数 = %d，want 2（整条消息蒸发的旧缺陷在这里表现为 0 或 1）", len(blocks))
	}
	// document 是已建模块型：citations 键改成 RawMessage 后它应正常解成 media，
	// 而不是掉进不透明兜底。
	if blocks[0].Type != ir.BlockMedia {
		t.Errorf("document 块型 = %q，want media", blocks[0].Type)
	}
	if blocks[0].Media == nil || blocks[0].Media.Data != "hello world" {
		t.Errorf("document 载荷丢失：%+v", blocks[0].Media)
	}
	// 用户真正在问的那句话必须活着。
	if blocks[1].Type != ir.BlockText || blocks[1].Text != "quote it" {
		t.Errorf("兄弟文本块被牵连蒸发：%+v", blocks[1])
	}
	out, err := codec{}.EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if !strings.Contains(string(out), "quote it") {
		t.Errorf("出站丢失用户问题：%s", out)
	}
}

// search_result.source 是字符串，而 block.Source 建模成对象：同款冲突。
// 这个块自身无槽位可留（不透明兜底），但它不得带走兄弟块。
func TestSearchResultStringSourceDoesNotDestroyMessage(t *testing.T) {
	sr := `{"type":"search_result","source":"https://example.com/a","title":"A doc","content":[{"type":"text","text":"the actual document body"}]}`
	reqBody := `{"model":"claude-x","max_tokens":16,"messages":[{"role":"user","content":[
		` + sr + `,{"type":"text","text":"summarize"}]}]}`
	req, err := codec{}.DecodeRequest([]byte(reqBody))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	blocks := req.Messages[0].Content
	if len(blocks) != 2 {
		t.Fatalf("块数 = %d，want 2", len(blocks))
	}
	if blocks[0].Type != ir.BlockOpaque || blocks[0].Opaque.WireType != "search_result" {
		t.Errorf("block[0] = %+v，want opaque/search_result", blocks[0])
	}
	if blocks[1].Type != ir.BlockText || blocks[1].Text != "summarize" {
		t.Errorf("兄弟文本块被牵连蒸发：%+v", blocks[1])
	}
	out, err := codec{}.EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, sr) {
		t.Errorf("search_result 未原样回传：%s", s)
	}
	if !strings.Contains(s, "summarize") {
		t.Errorf("兄弟文本块出站丢失：%s", s)
	}
}

// 核心不变量：冲突块夹在中间时，它两侧的兄弟块都要活下来。一次性解析
// []block 的实现会让三块一起蒸发且调用方只拿到 nil——没有错误也没有注记。
func TestDecodeBlocksIsolatesShapeConflict(t *testing.T) {
	conflict := r91ServerToolResults[1].body
	raw := `[{"type":"text","text":"before"},` + conflict + `,{"type":"text","text":"after"}]`
	blocks := decodeBlocks([]byte(raw))
	if len(blocks) != 3 {
		t.Fatalf("块数 = %d，want 3：%+v", len(blocks), blocks)
	}
	if blocks[0].Type != ir.BlockText || blocks[0].Text != "before" {
		t.Errorf("前兄弟块丢失：%+v", blocks[0])
	}
	if blocks[1].Type != ir.BlockOpaque {
		t.Errorf("冲突块未留成不透明：%+v", blocks[1])
	}
	if blocks[2].Type != ir.BlockText || blocks[2].Text != "after" {
		t.Errorf("后兄弟块丢失：%+v", blocks[2])
	}
}

// 连 type 都读不出来的元素不是 Anthropic 块：留着会在出站时变成 {"type":""}
// 让上游 400，必须整块丢弃而不是留成不透明块。
func TestBlockWithoutTypeIsDroppedNotOpaque(t *testing.T) {
	blocks := decodeBlocks([]byte(`[{"foo":1},{"type":"text","text":"ok"}]`))
	if len(blocks) != 1 {
		t.Fatalf("块数 = %d，want 1：%+v", len(blocks), blocks)
	}
	if blocks[0].Type != ir.BlockText || blocks[0].Text != "ok" {
		t.Errorf("有效块被牵连：%+v", blocks[0])
	}
	req := &ir.Request{Model: "claude-x", MaxTokens: 16, Messages: []ir.Message{
		{Role: ir.RoleUser, Content: blocks},
	}}
	out, err := codec{}.EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if strings.Contains(string(out), `"type":""`) {
		t.Errorf("出站出现空块型：%s", out)
	}
}

// tool_result.content 与 message.content 同形，但空字符串要留成一个空文本块：
// tool_result 没有内容会让上游认为工具没返回。这个差异不能被 decodeContent 收编。
func TestToolResultEmptyStringKeepsPlaceholderBlock(t *testing.T) {
	blocks := decodeToolResultContent([]byte(`""`))
	if len(blocks) != 1 || blocks[0].Type != ir.BlockText || blocks[0].Text != "" {
		t.Fatalf("空字符串 tool_result = %+v，want 一个空文本块", blocks)
	}
	// 数组形态与 message.content 一致：冲突块同样不得牵连兄弟块。
	arr := decodeToolResultContent([]byte(`[` + r91ServerToolResults[0].body + `,{"type":"text","text":"tool said"}]`))
	if len(arr) != 2 {
		t.Fatalf("数组形态块数 = %d，want 2：%+v", len(arr), arr)
	}
	if arr[0].Type != ir.BlockOpaque || arr[1].Text != "tool said" {
		t.Errorf("tool_result 内容解码错：%+v", arr)
	}
}

// ---- 流式 ----

// 不透明块没有增量形态：载荷随 content_block_start 一次给全。
func TestOpaqueStreamDecodeAndAggregate(t *testing.T) {
	raw := r91ServerToolResults[0].body
	d := codec{}.NewStreamDecoder()
	if _, err := d.Feed("message_start", `{"type":"message_start","message":{"id":"msg_1","model":"claude-x","usage":{"input_tokens":1,"output_tokens":1}}}`); err != nil {
		t.Fatalf("Feed start: %v", err)
	}
	evs, err := d.Feed("content_block_start",
		`{"type":"content_block_start","index":0,"content_block":`+raw+`}`)
	if err != nil || len(evs) != 1 {
		t.Fatalf("Feed block_start: err=%v events=%d", err, len(evs))
	}
	ev := evs[0]
	if ev.Type != ir.EvBlockStart || ev.Block == nil {
		t.Fatalf("事件 = %+v", ev)
	}
	if ev.Block.Type != ir.BlockOpaque || ev.Block.Opaque == nil ||
		ev.Block.Opaque.WireType != "web_fetch_tool_result" {
		t.Fatalf("块 = %+v", ev.Block)
	}
	if string(ev.Block.Opaque.Body) != raw {
		t.Errorf("块体未逐字节保留：%s", ev.Block.Opaque.Body)
	}
	if _, err := d.Feed("content_block_stop", `{"type":"content_block_stop","index":0}`); err != nil {
		t.Fatalf("Feed block_stop: %v", err)
	}
	// 聚合：非流式回退与重试判定都读聚合结果，块体在这里丢了等于全丢。
	a := ir.NewAggregator()
	a.Feed(ir.Event{Type: ir.EvMessageStart, MessageID: "msg_1"})
	a.Feed(ev)
	a.Feed(ir.Event{Type: ir.EvBlockStop, Index: 0})
	a.Feed(ir.Event{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn})
	got, _ := a.Finish()
	if len(got.Content) != 1 {
		t.Fatalf("聚合块数 = %d", len(got.Content))
	}
	b := got.Content[0]
	if b.Type != ir.BlockOpaque || b.Opaque == nil || string(b.Opaque.Body) != raw {
		t.Errorf("聚合丢失块体：%+v", b)
	}
}

// content_block_start 里的块体出现未建模形状时，整个 SSE 事件不得解析失败：
// 那会把流打断，客户端只看到一次无故的断流。
func TestOpaqueStreamEventSurvivesShapeConflict(t *testing.T) {
	d := codec{}.NewStreamDecoder()
	frame := `{"type":"content_block_start","index":0,"content_block":{"type":"document","title":"a.pdf","citations":{"enabled":true},"source":{"type":"text","media_type":"text/plain","data":"hello"}}}`
	evs, err := d.Feed("content_block_start", frame)
	if err != nil {
		t.Fatalf("Feed 应只降级那一个块而不是打断流：%v", err)
	}
	if len(evs) != 1 || evs[0].Block == nil {
		t.Fatalf("事件数 = %d，want 1", len(evs))
	}
	if evs[0].Block.Type != ir.BlockMedia {
		t.Errorf("document 应正常解成 media，实得 %+v", evs[0].Block)
	}
}

// 编码侧：不透明块整块原样下发。走清字段逻辑会把它掏空成一个只有 type 的壳。
func TestOpaqueStreamEncodeNotHollowedOut(t *testing.T) {
	raw := r91ServerToolResults[2].body
	e := codec{}.NewStreamEncoder()
	if _, err := e.Encode(ir.Event{Type: ir.EvMessageStart, MessageID: "msg_1", Model: "claude-x"}); err != nil {
		t.Fatalf("Encode start: %v", err)
	}
	b := r91Opaque("bash_code_execution_tool_result", raw)
	frames, err := e.Encode(ir.Event{Type: ir.EvBlockStart, Index: 0, Block: &b})
	if err != nil || len(frames) != 1 {
		t.Fatalf("Encode block_start: err=%v frames=%d", err, len(frames))
	}
	f := string(frames[0])
	if !strings.Contains(f, raw) {
		t.Errorf("block_start 帧丢块体：%s", f)
	}
	if strings.Contains(f, `"content_block":{"type":"bash_code_execution_tool_result"}`) {
		t.Errorf("块被掏空成只有 type 的壳：%s", f)
	}
	if _, err := e.Encode(ir.Event{Type: ir.EvBlockStop, Index: 0}); err != nil {
		t.Fatalf("Encode block_stop: %v", err)
	}
	// 自家协议不报损耗。
	if notes := e.Notes(); len(notes) != 0 {
		t.Errorf("anthropic 误报损耗：%v", notes)
	}
}

// ---- 回归护栏：citations 键改成 RawMessage 不得破坏正常引用数组 ----

func TestTextCitationsStillDecodeAfterRawMessage(t *testing.T) {
	body := `{"id":"msg_1","type":"message","role":"assistant","model":"claude-x","content":[
		{"type":"text","text":"hello world","citations":[
			{"type":"web_search_result_location","cited_text":"hello","url":"https://w.com","title":"T","encrypted_index":"EI","start_char_index":0,"end_char_index":5}]}]}`
	resp, err := codec{}.DecodeResponse([]byte(body))
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	cs := resp.Content[0].Citations
	if len(cs) != 1 {
		t.Fatalf("引用数 = %d，want 1", len(cs))
	}
	if cs[0].URL != "https://w.com" || cs[0].CitedText != "hello" {
		t.Errorf("引用载荷错：%+v", cs[0])
	}
	out, err := codec{}.EncodeResponse(resp)
	if err != nil {
		t.Fatalf("EncodeResponse: %v", err)
	}
	if !strings.Contains(string(out), `"citations":[`) {
		t.Errorf("引用未回写成数组：%s", out)
	}
}
