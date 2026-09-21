package relay

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/aceaura/ModelSurge/agent/ir"
	"github.com/aceaura/ModelSurge/agent/proto"
	"github.com/aceaura/ModelSurge/replay/contract/replayv1"
)

func (f *Forwarder) attemptKiro(ctx context.Context, w http.ResponseWriter, clientCodec proto.InboundCodec, cand candidate, req *ir.Request, onUsage func(*ir.Usage)) (bool, *ir.Error) {
	upReq := req.Clone()
	upReq.Stream = true
	cand.ov.Apply(upReq) // 账号级请求覆盖（未配置时 no-op）
	// 推理风格互补。这条路径直接把 canonical IR 交给 replay，不经 codec 的
	// EncodeRequest 也不经 ClampThinking，所以换算只能在这里做一次；
	// kiro 的 effortFragment 优先读 Effort，客户端只给 budget 时若不补全
	// 会落进它自己那套与 new-api 不同源的分档阈值。
	thinkNotes := ir.CompleteThinking(upReq)
	notes := append(Diagnose(req, cand.codec.Name(), cand.codec.Caps()), thinkNotes...)
	body, err := json.Marshal(upReq)
	if err != nil {
		return false, ir.NewHTTPError(http.StatusBadRequest, "encode canonical request: "+err.Error())
	}
	if policy := strictToolChoice(upReq); policy != nil {
		return f.attemptKiroStrict(ctx, w, clientCodec, cand, req, upReq, policy, notes, onUsage)
	}
	resp, openErr := f.openKiroReplay(ctx, cand, body, requestParams("kiro", upReq))
	if openErr != nil {
		if openErr.StatusCode == 499 && ctx.Err() != nil {
			return true, nil
		}
		return false, openErr
	}
	defer resp.Body.Close()
	writeLossyNotes(w, cand.name, notes)
	if req.Stream {
		return f.streamKiroToClient(ctx, w, clientCodec, cand, req, resp.Body, onUsage)
	}
	return f.collectKiroToClient(ctx, w, clientCodec, cand, req, resp.Body, onUsage)
}

func (f *Forwarder) openKiroReplay(ctx context.Context, cand candidate, request json.RawMessage, params string) (*http.Response, *ir.Error) {
	executor, ok := f.replay.(interface {
		ExecuteKiro(context.Context, replayv1.KiroExecuteRequest) (*http.Response, error)
	})
	if !ok {
		return nil, &ir.Error{StatusCode: http.StatusBadGateway, Type: ir.ErrTypeUpstream, Message: "replay execute unavailable", Retryable: true}
	}
	requestID := requestIDFrom(ctx)
	started := time.Now()
	if f.paramLog {
		log.Printf("agent phase=upstream_out request_id=%s target=%s proto=kiro %s body_bytes=%d", requestID, cand.name, params, len(request))
	}
	resp, err := executor.ExecuteKiro(ctx, replayv1.KiroExecuteRequest{RequestID: requestID, TargetID: cand.name, Request: request})
	if err != nil {
		if f.paramLog {
			log.Printf("agent phase=upstream_in request_id=%s target=%s proto=kiro status=0 content_type= latency=%s error=true", requestID, cand.name, time.Since(started))
		}
		if ctx.Err() != nil {
			return nil, &ir.Error{StatusCode: 499, Type: ir.ErrTypeUpstream, Message: "client disconnected"}
		}
		return nil, &ir.Error{StatusCode: http.StatusBadGateway, Type: ir.ErrTypeUpstream, Message: "replay execute unavailable: " + err.Error(), Retryable: true}
	}
	if f.paramLog {
		log.Printf("agent phase=upstream_in request_id=%s target=%s proto=kiro status=%d content_type=%s latency=%s error=false", requestID, cand.name, resp.StatusCode, resp.Header.Get("Content-Type"), time.Since(started))
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp, nil
	}
	defer resp.Body.Close()
	errBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrBody))
	var envelope replayv1.ErrorEnvelope
	if json.Unmarshal(errBody, &envelope) == nil && envelope.Error.Code != "" {
		status := envelope.Error.Status
		if status == 0 {
			status = resp.StatusCode
		}
		return nil, &ir.Error{StatusCode: status, Type: ir.ErrTypeUpstream, Message: envelope.Error.Message, Reason: envelope.Error.Reason, Retryable: envelope.Error.Retryable}
	}
	return nil, &ir.Error{StatusCode: resp.StatusCode, Type: ir.ErrTypeUpstream, Message: excerpt(string(errBody)), Retryable: resp.StatusCode >= 500}
}

func (f *Forwarder) streamKiroToClient(ctx context.Context, w http.ResponseWriter, clientCodec proto.InboundCodec, cand candidate, req *ir.Request, body io.Reader, onUsage func(*ir.Usage)) (bool, *ir.Error) {
	reader := bufio.NewReader(body)
	first, err := readKiroIREvent(reader)
	if err != nil {
		return false, kiroNDJSONError("first event", err)
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	encoder := clientCodec.NewStreamEncoder()
	var output strings.Builder
	var startUsage ir.Usage
	var eventCount int
	var bytesWritten int64
	streamStarted := time.Now()
	clientSummary := newClientSummarizer(f.paramLog, requestIDFrom(ctx), clientCodec.Name(), true, requestLogFrom(ctx).started)
	defer func() {
		if f.paramLog {
			log.Printf("agent phase=kiro_stream_done request_id=%s target=%s events=%d bytes=%d latency=%s", requestIDFrom(ctx), cand.name, eventCount, bytesWritten, time.Since(streamStarted))
		}
		clientSummary.log()
	}()
	emit := func(event ir.Event) bool {
		eventCount++
		clientSummary.observe(event)
		if event.Type == ir.EvMessageStart && event.Usage != nil {
			startUsage = *event.Usage
		}
		event = f.estimateUsageOnEvent(req, &output, event, cand.name)
		if onUsage != nil && event.Type == ir.EvMessageDelta && event.Usage != nil {
			usage := startUsage
			usage.MergeNonZero(*event.Usage)
			onUsage(&usage)
		}
		frames, encodeErr := encoder.Encode(event)
		if encodeErr != nil {
			clientSummary.encErr()
			frames = [][]byte{clientCodec.RenderStreamError(&ir.Error{Type: ir.ErrTypeUpstream, Message: encodeErr.Error()})}
		}
		clientSummary.framesAdd(len(frames))
		for _, frame := range frames {
			written, writeErr := w.Write(frame)
			bytesWritten += int64(written)
			clientSummary.wrote(written)
			if writeErr != nil {
				return false
			}
		}
		if flusher != nil {
			flusher.Flush()
		}
		return true
	}
	if !emit(first) {
		return true, nil
	}
	var tail []ir.Event
	bufferingTools := false
	for {
		event, readErr := readKiroIREvent(reader)
		if readErr != nil {
			if readErr != io.EOF {
				emit(ir.Event{Type: ir.EvError, Err: kiroNDJSONError("stream read", readErr)})
			}
			break
		}
		if event.Type == ir.EvBlockStart && event.Block != nil && event.Block.Type == ir.BlockToolUse {
			bufferingTools = true
		}
		if bufferingTools {
			tail = append(tail, event)
			continue
		}
		f.recordKiroEventTruncation(cand.name, event)
		if !emit(event) {
			return true, nil
		}
	}
	for _, event := range f.interceptWebSearch(ctx, cand, req, tail) {
		f.recordKiroEventTruncation(cand.name, event)
		if !emit(event) {
			return true, nil
		}
	}
	finished := encoder.Finish()
	clientSummary.framesAdd(len(finished))
	for _, frame := range finished {
		written, _ := w.Write(frame)
		bytesWritten += int64(written)
		clientSummary.wrote(written)
	}
	if flusher != nil {
		flusher.Flush()
	}
	return true, nil
}

func (f *Forwarder) collectKiroToClient(ctx context.Context, w http.ResponseWriter, clientCodec proto.InboundCodec, cand candidate, req *ir.Request, body io.Reader, onUsage func(*ir.Usage)) (bool, *ir.Error) {
	response, err := f.aggregateKiro(ctx, cand, req, body)
	if err != nil {
		return false, err
	}
	f.estimateUsageOnResponse(req, response, cand.name)
	if onUsage != nil {
		onUsage(&response.Usage)
	}
	upstreamSummary := newRespSummarizer(f.paramLog, requestIDFrom(ctx), cand.name, "ndjson", requestLogFrom(ctx).started)
	upstreamSummary.fill(response)
	upstreamSummary.log()
	clientSummary := newClientSummarizer(f.paramLog, requestIDFrom(ctx), clientCodec.Name(), false, requestLogFrom(ctx).started)
	clientSummary.fill(response)
	writeResponse(w, clientCodec, response, false, clientSummary)
	clientSummary.log()
	return true, nil
}

func (f *Forwarder) aggregateKiro(ctx context.Context, cand candidate, req *ir.Request, body io.Reader) (*ir.Response, *ir.Error) {
	reader := bufio.NewReader(body)
	first, err := readKiroIREvent(reader)
	if err != nil {
		return nil, kiroNDJSONError("first event", err)
	}
	aggregator := ir.NewAggregator()
	aggregator.Feed(first)
	var tail []ir.Event
	for {
		event, readErr := readKiroIREvent(reader)
		if readErr != nil {
			if readErr != io.EOF {
				return nil, kiroNDJSONError("stream read", readErr)
			}
			break
		}
		if event.Type == ir.EvError && event.Err != nil {
			event.Err.Retryable = true
			return nil, event.Err
		}
		tail = append(tail, event)
	}
	for _, event := range f.interceptWebSearch(ctx, cand, req, tail) {
		f.recordKiroEventTruncation(cand.name, event)
		aggregator.Feed(event)
	}
	response, aggregateErr := aggregator.Finish()
	if aggregateErr != nil {
		aggregateErr.Retryable = true
		return nil, aggregateErr
	}
	return response, nil
}

func (f *Forwarder) attemptKiroStrict(ctx context.Context, w http.ResponseWriter, clientCodec proto.InboundCodec, cand candidate, req *ir.Request, upReq *ir.Request, policy *ir.ToolChoice, notes []string, onUsage func(*ir.Usage)) (bool, *ir.Error) {
	var violation *toolViolation
	for attempt := 0; ; attempt++ {
		if violation != nil {
			upReq = appendRecoveryDirective(upReq, violation, policy)
		}
		body, err := json.Marshal(upReq)
		if err != nil {
			return false, ir.NewHTTPError(http.StatusBadRequest, err.Error())
		}
		resp, openErr := f.openKiroReplay(ctx, cand, body, requestParams("kiro", upReq))
		if openErr != nil {
			return false, openErr
		}
		response, aggregateErr := f.aggregateKiro(ctx, cand, req, resp.Body)
		resp.Body.Close()
		if aggregateErr != nil {
			return false, aggregateErr
		}
		if current := validateToolChoice(response, policy); current != nil {
			if attempt == 1 {
				return false, &ir.Error{StatusCode: http.StatusBadGateway, Type: ir.ErrTypeUpstream, Message: "tool_choice_not_satisfied: " + current.msg}
			}
			violation = current
			continue
		}
		f.estimateUsageOnResponse(req, response, cand.name)
		if onUsage != nil {
			onUsage(&response.Usage)
		}
		upstreamSummary := newRespSummarizer(f.paramLog, requestIDFrom(ctx), cand.name, "ndjson", requestLogFrom(ctx).started)
		upstreamSummary.fill(response)
		upstreamSummary.log()
		clientSummary := newClientSummarizer(f.paramLog, requestIDFrom(ctx), clientCodec.Name(), req.Stream, requestLogFrom(ctx).started)
		clientSummary.fill(response)
		writeLossyNotes(w, cand.name, notes)
		writeResponse(w, clientCodec, response, req.Stream, clientSummary)
		clientSummary.log()
		return true, nil
	}
}

func readKiroIREvent(reader *bufio.Reader) (ir.Event, error) {
	line, err := reader.ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return ir.Event{}, err
	}
	if len(strings.TrimSpace(string(line))) == 0 {
		if err != nil {
			return ir.Event{}, err
		}
		return readKiroIREvent(reader)
	}
	var event ir.Event
	if decodeErr := json.Unmarshal(line, &event); decodeErr != nil {
		return ir.Event{}, decodeErr
	}
	if event.Type == "" {
		return ir.Event{}, fmt.Errorf("missing event type")
	}
	return event, nil
}

func kiroNDJSONError(stage string, err error) *ir.Error {
	message := "kiro " + stage + ": " + err.Error()
	if err == io.EOF {
		message = "kiro closed stream without a valid event"
	}
	return &ir.Error{StatusCode: http.StatusBadGateway, Type: ir.ErrTypeUpstream, Message: message, Retryable: true}
}

func (f *Forwarder) recordKiroEventTruncation(upName string, event ir.Event) {
	if len(event.TruncatedTools) == 0 && event.TruncatedContent == "" {
		return
	}
	tools := make([]proto.TruncatedTool, len(event.TruncatedTools))
	for i, tool := range event.TruncatedTools {
		tools[i] = proto.TruncatedTool{ID: tool.ID, Name: tool.Name, Reason: tool.Reason}
	}
	f.trunc.Record(upName, tools, event.TruncatedContent != "", event.TruncatedContent)
}
