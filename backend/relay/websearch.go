// websearch.go web_search 服务端工具代执行（mcp_tools.py 流式拦截的 relay 侧翻译）。
// 注入（Path B）：kiro 账号开启 web_search 时经 Metadata["kiro_web_search"]
// 通知 kiro codec 注入工具声明（codec 侧 BuildPayload 执行）。
// 拦截：上游 Finish 事件中的 web_search 工具调用 → 调 MCP 代执行 →
// 合成 server_tool_use + web_search_tool_result + <web_search> 摘要文本块；
// MCP 失败或无 query 时降级为普通 tool_use 透传（Python 同款语义）。
// 摘要文本随 assistant 消息回传，模型上下文由该文本承载
// （kiro toUnified 对两个新块跳过）。
package relay

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"relayd/backend/account"
	"relayd/backend/ir"
)

// Metadata 标志键（relay -> kiro codec 的账号级策略通道）。
const (
	metaWebSearch     = "kiro_web_search"
	metaFakeReasoning = "kiro_fake_reasoning"
)

// hasWebSearchTool 请求是否已声明 web_search（具名或 Hosted）。
func hasWebSearchTool(req *ir.Request) bool {
	for _, t := range req.Tools {
		if t.Name == "web_search" || t.Hosted == ir.HostedWebSearch {
			return true
		}
	}
	return false
}

// prepareKiroMetadata kiro 候选确定后注入 Metadata：profileArn（载荷必带，
// runtime 端点必需）、web_search 注入标志（全局默认或账号开关，且未声明时）、
// fake_reasoning 标志（账号开关，codec 侧与全局配置取或）。
func (f *Forwarder) prepareKiroMetadata(cand candidate, req *ir.Request) {
	if f.sched == nil || cand.acc == nil || cand.acc.Type != account.TypeKiro {
		return
	}
	rt := f.sched.KiroRuntimeOf(cand.acc.Name)
	if rt == nil {
		return
	}
	if req.Metadata == nil {
		req.Metadata = map[string]string{}
	}
	if arn := rt.Auth.EffectiveProfileArn(); arn != "" {
		req.Metadata["kiro_profile_arn"] = arn
	}
	webSearch := f.kiroWebSearchInject
	if cand.acc.Kiro != nil && cand.acc.Kiro.WebSearch {
		webSearch = true
	}
	if webSearch && !hasWebSearchTool(req) {
		req.Metadata[metaWebSearch] = "1"
	}
	if cand.acc.Kiro != nil && cand.acc.Kiro.FakeReasoning {
		req.Metadata[metaFakeReasoning] = "1"
	}
}

// webSearchCaller web_search MCP 执行通道（*account.KiroClient 实现，
// 测试注入 fake）。
type webSearchCaller interface {
	CallWebSearch(ctx context.Context, query string) (string, []ir.WebSearchResult, error)
}

// interceptWebSearch 处理 kiro 解码器 Finish 事件中的 web_search 工具调用：
// 命中则调 MCP 代执行并重写为服务端工具块序列（索引重排）。
// 拦截条件：kiro 候选 + （全局默认或账号开启）或请求已声明 web_search
// （原生声明优先于开关）。
func (f *Forwarder) interceptWebSearch(ctx context.Context, cand candidate, req *ir.Request, events []ir.Event) []ir.Event {
	if f.sched == nil || cand.acc == nil || cand.acc.Type != account.TypeKiro {
		return events
	}
	injectEnabled := f.kiroWebSearchInject
	if cand.acc.Kiro != nil && cand.acc.Kiro.WebSearch {
		injectEnabled = true
	}
	if !injectEnabled && !hasWebSearchTool(req) {
		return events
	}
	if !hasWebSearchToolEvent(events) {
		return events
	}
	rt := f.sched.KiroRuntimeOf(cand.acc.Name)
	if rt == nil {
		return events
	}
	return rewriteWebSearchEvents(ctx, rt.Client, events)
}

// hasWebSearchToolEvent 事件序列中是否存在 web_search 工具调用块。
func hasWebSearchToolEvent(events []ir.Event) bool {
	for _, ev := range events {
		if ev.Type == ir.EvBlockStart && ev.Block != nil &&
			ev.Block.Type == ir.BlockToolUse && ev.Block.ToolUse != nil &&
			ev.Block.ToolUse.Name == "web_search" {
			return true
		}
	}
	return false
}

// rewriteWebSearchEvents 扫描工具块序列并替换 web_search 调用。
// kiro 解码器契约：Finish 中先闭 text/thinking，再连续发工具块
// （start/input/stop 三元组），最后 message_delta/message_stop。
// 替换块数变化后从首个工具块索引起重排；拦截后无剩余普通工具块时
// stop 改 end_turn。
func rewriteWebSearchEvents(ctx context.Context, client webSearchCaller, events []ir.Event) []ir.Event {
	type toolBlock struct {
		start, stop int    // 事件下标（含）
		input       string // 累积的参数 JSON
		intercepted bool
	}
	var blocks []toolBlock
	inBlock := -1
	for i, ev := range events {
		switch ev.Type {
		case ir.EvBlockStart:
			if ev.Block != nil && ev.Block.Type == ir.BlockToolUse {
				inBlock = len(blocks)
				blocks = append(blocks, toolBlock{start: i, stop: i})
			}
		case ir.EvToolInput:
			if inBlock >= 0 {
				blocks[inBlock].input += ev.Text
			}
		case ir.EvBlockStop:
			if inBlock >= 0 {
				blocks[inBlock].stop = i
				inBlock = -1
			}
		}
	}
	if len(blocks) == 0 {
		return events
	}

	// MCP 代执行（顺序执行，Python 同款；失败降级为普通 tool_use 透传）
	var synths [][]ir.Event
	for i := range blocks {
		start := events[blocks[i].start]
		if start.Block == nil || start.Block.ToolUse == nil || start.Block.ToolUse.Name != "web_search" {
			continue
		}
		query := extractQuery(blocks[i].input)
		if query == "" {
			continue
		}
		id, results, err := client.CallWebSearch(ctx, query)
		if err != nil {
			continue
		}
		blocks[i].intercepted = true
		synths = append(synths, synthWebSearchEvents(id, query, results))
	}
	if len(synths) == 0 {
		return events
	}

	// 重写：首个工具块索引为基准，按块顺序重排；
	// 工具块之前的 text/thinking 关闭事件原样保留（下游靠它合块）
	var out []ir.Event
	out = append(out, events[:blocks[0].start]...)
	next := events[blocks[0].start].Index
	si := 0
	for _, b := range blocks {
		if b.intercepted {
			out = append(out, reindexEvents(synths[si], &next)...)
			si++
			continue
		}
		for j := b.start; j <= b.stop; j++ {
			ev := events[j]
			switch ev.Type {
			case ir.EvBlockStart:
				ev.Index = next
				next++
			case ir.EvToolInput, ir.EvBlockStop:
				ev.Index = next - 1 // 归属当前打开的块
			}
			out = append(out, ev)
		}
	}
	// 尾随事件（message_delta / message_stop）：块索引无关，原样接续；
	// 全部工具被拦截时 stop_reason 改 end_turn（无剩余客户端可执行工具）
	remaining := false
	for _, b := range blocks {
		if !b.intercepted {
			remaining = true
		}
	}
	for i := blocks[len(blocks)-1].stop + 1; i < len(events); i++ {
		ev := events[i]
		if ev.Type == ir.EvMessageDelta && !remaining && ev.StopReason == ir.StopToolUse {
			ev.StopReason = ir.StopEndTurn
		}
		out = append(out, ev)
	}
	return out
}

// synthWebSearchEvents 合成一次 web_search 的服务端工具块序列
// （server_tool_use + input delta + stop；result 块；摘要文本块）。
// 索引占位 0/1/2，reindexEvents 统一编号。
func synthWebSearchEvents(id, query string, results []ir.WebSearchResult) []ir.Event {
	input, _ := json.Marshal(map[string]string{"query": query})
	summary := formatSearchSummary(query, results)
	return []ir.Event{
		{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{
			Type: ir.BlockServerToolUse,
			ServerToolUse: &ir.ServerToolUse{
				ID: id, Name: "web_search", Input: json.RawMessage(input),
			},
		}},
		{Type: ir.EvToolInput, Index: 0, Text: string(input)},
		{Type: ir.EvBlockStop, Index: 0},
		{Type: ir.EvBlockStart, Index: 1, Block: &ir.Block{
			Type:                ir.BlockWebSearchToolResult,
			WebSearchToolResult: &ir.WebSearchToolResult{ToolUseID: id, Results: results},
		}},
		{Type: ir.EvBlockStop, Index: 1},
		{Type: ir.EvBlockStart, Index: 2, Block: &ir.Block{Type: ir.BlockText}},
		{Type: ir.EvTextDelta, Index: 2, Text: summary},
		{Type: ir.EvBlockStop, Index: 2},
	}
}

// reindexEvents 把合成序列的块索引改为自 next 起的连续编号
// （块开始递增，delta/stop 归属刚开的块），并推进 next。
func reindexEvents(events []ir.Event, next *int) []ir.Event {
	out := make([]ir.Event, 0, len(events))
	for _, ev := range events {
		switch ev.Type {
		case ir.EvBlockStart:
			ev.Index = *next
			*next++
		case ir.EvToolInput, ir.EvTextDelta, ir.EvBlockStop:
			ev.Index = *next - 1
		}
		out = append(out, ev)
	}
	return out
}

// extractQuery 从工具参数 JSON 提取 query 字段；解析失败返回空。
func extractQuery(input string) string {
	s := strings.TrimSpace(input)
	if s == "" {
		return ""
	}
	var p struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal([]byte(s), &p); err != nil {
		return ""
	}
	return p.Query
}

// formatSearchSummary 生成 <web_search> 包裹的可读摘要
// （mcp_tools.py generate_search_summary 的 Go 翻译：全量摘要不截断）。
func formatSearchSummary(query string, results []ir.WebSearchResult) string {
	var sb strings.Builder
	sb.WriteString("\n<web_search>\nSearch results for \"")
	sb.WriteString(query)
	sb.WriteString("\":\n\n")
	if len(results) == 0 {
		sb.WriteString("No results found.\n")
	}
	for i, r := range results {
		fmt.Fprintf(&sb, "%d. Title: **%s**\n", i+1, r.Title)
		if r.URL != "" {
			fmt.Fprintf(&sb, "   URL: %s\n", r.URL)
		}
		if r.Snippet != "" {
			fmt.Fprintf(&sb, "   %s\n", r.Snippet)
		}
		sb.WriteString("\n")
	}
	sb.WriteString("</web_search>\n")
	return sb.String()
}
