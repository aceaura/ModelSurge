// websearch.go web_search 服务端工具代执行（mcp_tools.py 流式拦截的 relay 侧翻译）。
// Upstream 负责向 Kiro 请求注入工具；Agent 根据规范事件中的 web_search
// 调用 Replay 执行 MCP，再重写为服务端工具块序列。
package relay

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/aceaura/ModelSurge/agent/ir"
	"github.com/aceaura/ModelSurge/replay/contract/replayv1"
)

func hasWebSearchTool(req *ir.Request) bool {
	for _, t := range req.Tools {
		if t.Name == "web_search" || t.Hosted == ir.HostedWebSearch {
			return true
		}
	}
	return false
}

type webSearchCaller interface {
	CallWebSearch(ctx context.Context, query string) (string, []ir.WebSearchResult, error)
}

func (f *Forwarder) interceptWebSearch(ctx context.Context, cand candidate, req *ir.Request, events []ir.Event) []ir.Event {
	if cand.protocol != "kiro" || !hasWebSearchToolEvent(events) || f.replay == nil {
		return events
	}
	return rewriteWebSearchEvents(ctx, replayWebSearchCaller{replay: f.replay, targetID: cand.name}, events)
}

type replayWebSearchCaller struct {
	replay   Replay
	targetID string
}

func (c replayWebSearchCaller) CallWebSearch(ctx context.Context, query string) (string, []ir.WebSearchResult, error) {
	out, err := c.replay.WebSearch(ctx, replayv1.WebSearchRequest{TargetID: c.targetID, Query: query})
	if err != nil {
		return "", nil, err
	}
	return webSearchIR(out)
}

func webSearchIR(out replayv1.WebSearchResponse) (string, []ir.WebSearchResult, error) {
	results := make([]ir.WebSearchResult, len(out.Results))
	for i, r := range out.Results {
		results[i] = ir.WebSearchResult{Title: r.Title, URL: r.URL, Snippet: r.Snippet}
	}
	return out.ID, results, nil
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
