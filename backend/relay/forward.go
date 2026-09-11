package relay

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"relayd/backend/config"
	"relayd/backend/ir"
	"relayd/backend/proto"
)

// Forwarder 把 IR 请求转发到上游，并把上游响应回传给客户端。
// 核心原则（调研共识）：对上游永远以流式方式请求；
// 客户端要非流式时由网关聚合后一次性返回。
//
// 重试原则：只要还没向客户端写出任何字节（连接失败、429/5xx、
// 首事件超时、非流式聚合失败），就换下一个候选上游重发；
// 一旦写出第一个字节即锁死，错误只能在流内就地渲染。
type Forwarder struct {
	client            *http.Client
	upstreams         []config.Upstream
	firstTokenTimeout time.Duration
	estimateUsage     bool
}

// NewForwarder 构造转发器。
func NewForwarder(cfg *config.Config) *Forwarder {
	return &Forwarder{
		client:            &http.Client{Timeout: 0}, // 流式请求不设整体超时
		upstreams:         cfg.Upstreams,
		firstTokenTimeout: cfg.FirstTokenTimeoutDur,
		estimateUsage:     cfg.EstimateUsage,
	}
}

// candidate 一个可服务某 canonical model 的上游及其 native 模型名与 codec。
type candidate struct {
	up     config.Upstream
	native string
	codec  proto.Codec
}

// candidates 按 canonical model 列出候选上游（每个上游至多一次）：
// 显式 models 映射优先，透传型上游（models 为空）兜底。
func (f *Forwarder) candidates(model string) []candidate {
	var mapped, passthrough []candidate
	for _, u := range f.upstreams {
		c, err := proto.Get(u.Protocol)
		if err != nil {
			continue
		}
		if native, ok := u.Models[model]; ok {
			mapped = append(mapped, candidate{u, native, c})
		} else if len(u.Models) == 0 {
			passthrough = append(passthrough, candidate{u, model, c})
		}
	}
	return append(mapped, passthrough...)
}

// endpoint 上游请求的 URL 与鉴权头。
func endpoint(u config.Upstream, nativeModel string) (url string, headers map[string]string) {
	switch u.Protocol {
	case "anthropic":
		return u.BaseURL + "/v1/messages", map[string]string{
			"x-api-key":         u.APIKey,
			"anthropic-version": "2023-06-01",
		}
	case "openai-responses":
		return u.BaseURL + "/v1/responses", map[string]string{"Authorization": "Bearer " + u.APIKey}
	case "gemini":
		return fmt.Sprintf("%s/v1beta/models/%s:streamGenerateContent?alt=sse", u.BaseURL, nativeModel),
			map[string]string{"x-goog-api-key": u.APIKey}
	default: // openai-chat
		return u.BaseURL + "/v1/chat/completions", map[string]string{"Authorization": "Bearer " + u.APIKey}
	}
}

// maxErrBody 上游错误体的读取上限。
const maxErrBody = 4 * 1024

// Forward 执行一次转发。clientCodec 为客户端协议 codec（用于错误渲染与响应编码），
// req.Stream 表示客户端是否要求流式。所有响应直接写入 w。
func (f *Forwarder) Forward(ctx context.Context, w http.ResponseWriter, clientCodec proto.Codec, req *ir.Request) {
	cands := f.candidates(req.Model)
	if len(cands) == 0 {
		writeError(w, clientCodec, ir.NewHTTPError(404, fmt.Sprintf("no upstream serves model %q", req.Model)))
		return
	}
	var lastErr *ir.Error
	for i, cand := range cands {
		if ctx.Err() != nil {
			return // 客户端已断开，静默结束
		}
		if i > 0 {
			log.Printf("relay: model %q retry with upstream %s (%s): %s", req.Model, cand.up.Name, cand.up.Protocol, lastErr)
		}
		wrote, err := f.attempt(ctx, w, clientCodec, cand, req)
		if err == nil {
			return
		}
		lastErr = err
		if wrote || !err.Retryable {
			// 已写字节则错误已就地渲染；不可重试错误直接透出
			if !wrote {
				writeError(w, clientCodec, err)
			}
			return
		}
	}
	if ctx.Err() == nil {
		writeError(w, clientCodec, lastErr)
	}
}

// attempt 对单个上游做一次转发尝试。wrote 表示是否已向客户端写出字节。
func (f *Forwarder) attempt(ctx context.Context, w http.ResponseWriter, clientCodec proto.Codec, cand candidate, req *ir.Request) (wrote bool, err *ir.Error) {
	// 上游永远流式
	upReq := req.Clone()
	upReq.Model = cand.native
	upReq.Stream = true
	body, encErr := cand.codec.EncodeRequest(upReq)
	if encErr != nil {
		return false, ir.NewHTTPError(400, "encode upstream request: "+encErr.Error())
	}

	url, headers := endpoint(cand.up, cand.native)
	actx, cancel := context.WithCancel(ctx)
	defer cancel()
	httpReq, reqErr := http.NewRequestWithContext(actx, http.MethodPost, url, bytes.NewReader(body))
	if reqErr != nil {
		return false, ir.NewHTTPError(500, reqErr.Error())
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	for k, v := range headers {
		httpReq.Header.Set(k, v)
	}

	resp, doErr := f.client.Do(httpReq)
	if doErr != nil {
		if ctx.Err() != nil {
			return true, nil // 客户端断开
		}
		return false, &ir.Error{StatusCode: 502, Type: ir.ErrTypeUpstream, Message: "upstream unreachable: " + doErr.Error(), Retryable: true}
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrBody))
		return false, ir.NewHTTPError(resp.StatusCode, excerpt(string(errBody)))
	}

	// 已锁定该上游：落有损转换诊断（日志 + 响应头，须在 WriteHeader 前设置）
	if notes := Diagnose(req, cand.codec.Caps()); len(notes) > 0 {
		log.Printf("relay: upstream %s lossy conversion: %s", cand.up.Name, strings.Join(notes, "; "))
		w.Header().Set("X-Relayd-Notes", strings.Join(notes, "; "))
	}

	// 兜底：上游忽略 stream=true 返回完整 JSON
	if !strings.Contains(resp.Header.Get("Content-Type"), "event-stream") {
		full, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			return false, &ir.Error{StatusCode: 502, Type: ir.ErrTypeUpstream, Message: readErr.Error(), Retryable: true}
		}
		irResp, decErr := cand.codec.DecodeResponse(full)
		if decErr != nil {
			return false, &ir.Error{StatusCode: 502, Type: ir.ErrTypeUpstream, Message: "decode upstream response: " + decErr.Error(), Retryable: true}
		}
		f.estimateUsageOnResponse(req, irResp, cand.up.Name)
		writeResponse(w, clientCodec, irResp, req.Stream)
		return true, nil
	}

	if req.Stream {
		return f.streamUpstreamToClient(cancel, w, clientCodec, cand, req, resp.Body)
	}
	return f.collectUpstreamToClient(cancel, w, clientCodec, cand, req, resp.Body)
}

// awaitFirstEvent 等上游的第一个 SSE 事件；超时则取消本次请求并返回可重试错误。
// 参考 kiro-gateway stream_with_first_token_retry：建连成功不代表上游健康，
// 迟迟不出首 chunk 应视为失败换上游。
func (f *Forwarder) awaitFirstEvent(er *EventReader, cancel context.CancelFunc) (SSEEvent, bool, *ir.Error) {
	type result struct {
		ev  SSEEvent
		ok  bool
		err error
	}
	ch := make(chan result, 1)
	go func() {
		ev, ok, err := er.Next()
		ch <- result{ev, ok, err}
	}()
	wrapErr := func(err error) *ir.Error {
		return &ir.Error{StatusCode: 502, Type: ir.ErrTypeUpstream, Message: "upstream stream read: " + err.Error(), Retryable: true}
	}
	if f.firstTokenTimeout <= 0 {
		r := <-ch
		if r.err != nil {
			return SSEEvent{}, false, wrapErr(r.err)
		}
		return r.ev, r.ok, nil
	}
	select {
	case r := <-ch:
		if r.err != nil {
			return SSEEvent{}, false, wrapErr(r.err)
		}
		return r.ev, r.ok, nil
	case <-time.After(f.firstTokenTimeout):
		cancel() // 杀掉阻塞中的 body 读取
		<-ch     // 等读取 goroutine 退出，避免泄露
		return SSEEvent{}, false, &ir.Error{
			StatusCode: 504, Type: ir.ErrTypeUpstream,
			Message:   fmt.Sprintf("upstream produced no event within %s", f.firstTokenTimeout),
			Retryable: true,
		}
	}
}

// streamUpstreamToClient 上游 SSE -> IR 事件 -> 客户端 SSE，逐 chunk 透传转换。
func (f *Forwarder) streamUpstreamToClient(cancel context.CancelFunc, w http.ResponseWriter, clientCodec proto.Codec, cand candidate, req *ir.Request, body io.Reader) (bool, *ir.Error) {
	er := NewEventReader(body)
	first, ok, firstErr := f.awaitFirstEvent(er, cancel)
	if firstErr != nil {
		return false, firstErr
	}
	if !ok {
		return false, &ir.Error{StatusCode: 502, Type: ir.ErrTypeUpstream, Message: "upstream closed stream without any event", Retryable: true}
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(200)
	flush, _ := w.(http.Flusher)

	dec := cand.codec.NewStreamDecoder()
	enc := clientCodec.NewStreamEncoder()
	var outText strings.Builder
	emit := func(events []ir.Event) bool {
		for _, ev := range events {
			ev = f.estimateUsageOnEvent(req, &outText, ev, cand.up.Name)
			frames, err := enc.Encode(ev)
			if err != nil {
				frames = [][]byte{clientCodec.RenderStreamError(&ir.Error{Type: ir.ErrTypeUpstream, Message: err.Error()})}
			}
			for _, fr := range frames {
				if _, err := w.Write(fr); err != nil {
					return false // 客户端断开
				}
			}
			if flush != nil {
				flush.Flush()
			}
		}
		return true
	}

	feed := func(ev SSEEvent) bool {
		events, err := dec.Feed(ev.Event, ev.Data)
		if err != nil {
			events = []ir.Event{{Type: ir.EvError, Err: &ir.Error{Type: ir.ErrTypeUpstream, Message: "upstream stream decode: " + err.Error()}}}
		}
		return emit(events)
	}

	if !feed(first) {
		return true, nil
	}
	for {
		ev, ok, rerr := er.Next()
		if rerr != nil {
			emit([]ir.Event{{Type: ir.EvError, Err: &ir.Error{Type: ir.ErrTypeUpstream, Message: "upstream stream read: " + rerr.Error()}}})
			break
		}
		if !ok {
			break
		}
		if !feed(ev) {
			return true, nil
		}
	}
	emit(dec.Finish())
	for _, fr := range enc.Finish() {
		_, _ = w.Write(fr)
	}
	if flush != nil {
		flush.Flush()
	}
	return true, nil
}

// collectUpstreamToClient 聚合上游流，向非流式客户端一次性返回完整 JSON。
// 聚合期间不向客户端写任何字节，因此聚合失败仍可换上游重试
// （代价是失败上游可能已计费——pre-write 重试的固有取舍）。
func (f *Forwarder) collectUpstreamToClient(cancel context.CancelFunc, w http.ResponseWriter, clientCodec proto.Codec, cand candidate, req *ir.Request, body io.Reader) (bool, *ir.Error) {
	er := NewEventReader(body)
	first, ok, firstErr := f.awaitFirstEvent(er, cancel)
	if firstErr != nil {
		return false, firstErr
	}
	if !ok {
		return false, &ir.Error{StatusCode: 502, Type: ir.ErrTypeUpstream, Message: "upstream closed stream without any event", Retryable: true}
	}

	dec := cand.codec.NewStreamDecoder()
	agg := ir.NewAggregator()
	feed := func(ev SSEEvent) {
		events, err := dec.Feed(ev.Event, ev.Data)
		if err != nil {
			events = []ir.Event{{Type: ir.EvError, Err: &ir.Error{Type: ir.ErrTypeUpstream, Message: "upstream stream decode: " + err.Error()}}}
		}
		for _, e := range events {
			agg.Feed(e)
		}
	}
	feed(first)
	for {
		ev, ok, rerr := er.Next()
		if rerr != nil {
			agg.Feed(ir.Event{Type: ir.EvError, Err: &ir.Error{Type: ir.ErrTypeUpstream, Message: "upstream stream read: " + rerr.Error()}})
			break
		}
		if !ok {
			break
		}
		feed(ev)
	}
	for _, e := range dec.Finish() {
		agg.Feed(e)
	}
	resp, aggErr := agg.Finish()
	if aggErr != nil {
		// 未写任何字节：强制可重试，换上游重发
		aggErr.Retryable = true
		return false, aggErr
	}
	f.estimateUsageOnResponse(req, resp, cand.up.Name)
	writeResponse(w, clientCodec, resp, false)
	return true, nil
}

// CountTokens 处理 Anthropic count_tokens 请求：优先转发给 anthropic 上游
// 原生计数；无可用上游时本地粗估并记日志。
func (f *Forwarder) CountTokens(ctx context.Context, req *ir.Request) (int, []byte) {
	for _, cand := range f.candidates(req.Model) {
		if cand.up.Protocol != "anthropic" {
			continue
		}
		upReq := req.Clone()
		upReq.Model = cand.native
		upReq.Stream = false
		body, err := cand.codec.EncodeRequest(upReq)
		if err != nil {
			break
		}
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
			cand.up.BaseURL+"/v1/messages/count_tokens", bytes.NewReader(body))
		if err != nil {
			break
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("x-api-key", cand.up.APIKey)
		httpReq.Header.Set("anthropic-version", "2023-06-01")
		resp, err := f.client.Do(httpReq)
		if err != nil {
			log.Printf("relay: count_tokens upstream %s unreachable: %v", cand.up.Name, err)
			continue
		}
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return resp.StatusCode, respBody
		}
		log.Printf("relay: count_tokens upstream %s returned %d: %s", cand.up.Name, resp.StatusCode, excerpt(string(respBody)))
	}
	est := ir.EstimateRequestTokens(req)
	log.Printf("relay: count_tokens estimated locally: %d tokens", est)
	return 200, []byte(fmt.Sprintf(`{"input_tokens":%d}`, est))
}

// estimateUsageOnResponse 上游未给 usage 且开启估算时，按请求/响应文本粗估。
func (f *Forwarder) estimateUsageOnResponse(req *ir.Request, resp *ir.Response, upName string) {
	if !f.estimateUsage {
		return
	}
	if resp.Usage.TotalInput()+resp.Usage.OutputTokens != 0 {
		return
	}
	var sb strings.Builder
	for _, b := range resp.Content {
		switch b.Type {
		case ir.BlockText:
			sb.WriteString(b.Text)
		case ir.BlockThinking:
			if b.Thinking != nil {
				sb.WriteString(b.Thinking.Text)
			}
		case ir.BlockToolUse:
			if b.ToolUse != nil {
				sb.Write(b.ToolUse.Input)
			}
		}
	}
	resp.Usage = ir.Usage{
		InputTokens:  ir.EstimateRequestTokens(req),
		OutputTokens: ir.EstimateTokens(sb.String()),
		Estimated:    true,
	}
	log.Printf("relay: upstream %s reported no usage; estimated in=%d out=%d", upName, resp.Usage.InputTokens, resp.Usage.OutputTokens)
}

// estimateUsageOnEvent 流式路径的 usage 估算：在最终 EvMessageDelta 上补估值。
func (f *Forwarder) estimateUsageOnEvent(req *ir.Request, outText *strings.Builder, ev ir.Event, upName string) ir.Event {
	switch ev.Type {
	case ir.EvTextDelta, ir.EvThinkingDelta, ir.EvToolInput:
		outText.WriteString(ev.Text)
	case ir.EvMessageDelta:
		if !f.estimateUsage {
			return ev
		}
		if ev.Usage != nil && ev.Usage.TotalInput()+ev.Usage.OutputTokens != 0 {
			return ev
		}
		ev.Usage = &ir.Usage{
			InputTokens:  ir.EstimateRequestTokens(req),
			OutputTokens: ir.EstimateTokens(outText.String()),
			Estimated:    true,
		}
		log.Printf("relay: upstream %s reported no usage; estimated in=%d out=%d", upName, ev.Usage.InputTokens, ev.Usage.OutputTokens)
	}
	return ev
}

// writeResponse 非流式输出；clientStream 为 true 时（上游返回了非 SSE 的兜底响应
// 而客户端要流式）把完整响应合成为一次性事件流。
func writeResponse(w http.ResponseWriter, clientCodec proto.Codec, resp *ir.Response, clientStream bool) {
	if !clientStream {
		body, err := clientCodec.EncodeResponse(resp)
		if err != nil {
			writeError(w, clientCodec, ir.NewHTTPError(500, "encode response: "+err.Error()))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write(body)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(200)
	enc := clientCodec.NewStreamEncoder()
	for _, ev := range EventsFromResponse(resp) {
		frames, _ := enc.Encode(ev)
		for _, fr := range frames {
			_, _ = w.Write(fr)
		}
	}
	for _, fr := range enc.Finish() {
		_, _ = w.Write(fr)
	}
	if flush, ok := w.(http.Flusher); ok {
		flush.Flush()
	}
}

// EventsFromResponse 把完整响应合成为一次性 IR 事件序列
// （用于"上游返回非流式 JSON 而客户端要 SSE"的兜底路径）。
func EventsFromResponse(resp *ir.Response) []ir.Event {
	events := []ir.Event{{Type: ir.EvMessageStart, MessageID: resp.ID, Model: resp.Model}}
	for i, b := range resp.Content {
		blk := b
		if blk.Type == ir.BlockToolUse && blk.ToolUse != nil {
			input := string(blk.ToolUse.Input)
			blk.ToolUse.Input = nil
			events = append(events,
				ir.Event{Type: ir.EvBlockStart, Index: i, Block: &blk},
				ir.Event{Type: ir.EvToolInput, Index: i, Text: input},
				ir.Event{Type: ir.EvBlockStop, Index: i})
			continue
		}
		events = append(events, ir.Event{Type: ir.EvBlockStart, Index: i, Block: &ir.Block{Type: blk.Type}})
		switch blk.Type {
		case ir.BlockText:
			if blk.Text != "" {
				events = append(events, ir.Event{Type: ir.EvTextDelta, Index: i, Text: blk.Text})
			}
		case ir.BlockThinking:
			if blk.Thinking != nil {
				if blk.Thinking.Text != "" {
					events = append(events, ir.Event{Type: ir.EvThinkingDelta, Index: i, Text: blk.Thinking.Text})
				}
				if blk.Thinking.Signature != "" {
					events = append(events, ir.Event{Type: ir.EvSigDelta, Index: i, Text: blk.Thinking.Signature})
				}
			}
		}
		events = append(events, ir.Event{Type: ir.EvBlockStop, Index: i})
	}
	u := resp.Usage
	events = append(events,
		ir.Event{Type: ir.EvMessageDelta, StopReason: resp.StopReason, Usage: &u},
		ir.Event{Type: ir.EvMessageStop})
	return events
}

func writeError(w http.ResponseWriter, clientCodec proto.Codec, e *ir.Error) {
	status, body := clientCodec.RenderError(e)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// excerpt 截取上游错误文本前 500 字符，避免刷屏与泄露。
func excerpt(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 500 {
		s = s[:500] + "..."
	}
	if s == "" {
		s = "upstream error"
	}
	return s
}
