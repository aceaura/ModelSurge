package upstream

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/aceaura/ModelSurge/upstream/account"
	"github.com/aceaura/ModelSurge/upstream/contract/upstreamv1"
	"github.com/aceaura/ModelSurge/upstream/ir"
	"github.com/aceaura/ModelSurge/upstream/proto"
	"github.com/aceaura/ModelSurge/upstream/proto/kiro"
)

const maxKiroErrorBody = 4 << 10

const (
	kiroProfileARNKey    = "kiro_profile_arn"
	kiroWebSearchKey     = "kiro_web_search"
	kiroFakeReasoningKey = "kiro_fake_reasoning"
)

func kiroRequestParams(req *ir.Request) string {
	var b strings.Builder
	fmt.Fprintf(&b, "model=%s stream=%t max_tokens=%d msgs=%d", req.Model, req.Stream, req.MaxTokens, len(req.Messages))
	if req.Temperature != nil {
		fmt.Fprintf(&b, " temp=%g", *req.Temperature)
	}
	if req.TopP != nil {
		fmt.Fprintf(&b, " top_p=%g", *req.TopP)
	}
	if req.TopK != nil {
		fmt.Fprintf(&b, " top_k=%d", *req.TopK)
	}
	fmt.Fprintf(&b, " stop=%d system=%d tools=%d", len(req.StopSequences), len(req.System), len(req.Tools))
	if req.ToolChoice != nil {
		fmt.Fprintf(&b, " tool_choice=%s", req.ToolChoice.Mode)
	}
	if req.Thinking == nil {
		b.WriteString(" thinking=absent")
	} else if !req.Thinking.Enabled {
		b.WriteString(" thinking=off")
	} else {
		fmt.Fprintf(&b, " thinking=on budget=%d effort=%s", req.Thinking.BudgetTokens, req.Thinking.Effort)
	}
	return b.String()
}

type KiroExecution struct {
	First    []ir.Event
	Continue func(func(ir.Event) error) error
	Close    func()
}

func (s *Service) SetKiroOptions(firstTokenTimeout, streamingReadTimeout time.Duration) {
	s.KiroFirstTokenTimeout = firstTokenTimeout
	s.KiroStreamingReadTimeout = streamingReadTimeout
}

func (s *Service) ExecuteKiro(ctx context.Context, envelope upstreamv1.KiroExecuteRequest) (*KiroExecution, *upstreamv1.Error) {
	requestID := envelope.RequestID
	if requestID == "" {
		requestID = requestIDFrom(ctx)
	}
	started := time.Now()
	if envelope.TargetID == "" || len(envelope.Request) == 0 {
		return nil, &upstreamv1.Error{Code: upstreamv1.CodeInvalidRequest, Message: "target_id and request are required", Status: http.StatusBadRequest}
	}
	model, err := s.Store.GetModel(ctx, envelope.TargetID)
	if err != nil {
		return nil, internalError()
	}
	if model == nil {
		return nil, &upstreamv1.Error{Code: upstreamv1.CodeNotFound, Message: "target not found", Status: http.StatusNotFound}
	}
	if model.Protocol != "kiro" || !model.Enabled || (model.CooldownUntil.After(time.Now()) && !s.probeGranted(ctx, model.ID, entryFromModel(model))) {
		return nil, &upstreamv1.Error{Code: upstreamv1.CodeTargetUnavailable, Message: "target unavailable", Retryable: true, Status: http.StatusServiceUnavailable}
	}
	var acc *account.Account
	for _, current := range s.Manager.Status() {
		if current.Name == model.Account {
			copy := current
			acc = &copy
			break
		}
	}
	if acc == nil || !acc.Enabled || acc.Disabled || acc.Type != account.TypeKiro {
		return nil, &upstreamv1.Error{Code: upstreamv1.CodeTargetUnavailable, Message: "target unavailable", Retryable: true, Status: http.StatusServiceUnavailable}
	}
	runtime := s.Manager.KiroRuntimeOf(acc.Name)
	if runtime == nil {
		return nil, &upstreamv1.Error{Code: upstreamv1.CodeCredentialRefresh, Message: "credential runtime unavailable", Retryable: true, Status: http.StatusServiceUnavailable}
	}

	var request ir.Request
	if err := json.Unmarshal(envelope.Request, &request); err != nil {
		return nil, &upstreamv1.Error{Code: upstreamv1.CodeInvalidRequest, Message: "invalid canonical request", Status: http.StatusBadRequest}
	}
	if s.AccessLogEnabled {
		log.Printf("upstream phase=canonical_in request_id=%s target=%s %s request_bytes=%d", requestID, envelope.TargetID, kiroRequestParams(&request), len(envelope.Request))
	}
	request.Stream = true
	native := model.NativeModel
	if (native == "" || native == "*") && model.DisplayName != "*" {
		native, _ = runtime.Resolve(model.DisplayName)
	}
	request.Model = native
	var overrideTemperature, overrideTopP, overrideMaxTokens, overrideThinking bool
	if len(model.RequestOverrides) > 0 && string(model.RequestOverrides) != "null" {
		var overrides ir.Overrides
		if json.Unmarshal(model.RequestOverrides, &overrides) == nil {
			overrideTemperature = overrides.Temperature != nil
			overrideTopP = overrides.TopP != nil
			overrideMaxTokens = overrides.MaxTokens != nil
			overrideThinking = overrides.Thinking != nil
			overrides.Apply(&request)
		}
	}
	if s.AccessLogEnabled {
		log.Printf("upstream phase=resolved request_id=%s target=%s account=%s protocol=%s native_model=%s override_temperature=%t override_top_p=%t override_max_tokens=%t override_thinking=%t", requestID, envelope.TargetID, model.Account, model.Protocol, native, overrideTemperature, overrideTopP, overrideMaxTokens, overrideThinking)
	}
	if request.Metadata == nil {
		request.Metadata = map[string]string{}
	}
	delete(request.Metadata, kiroProfileARNKey)
	delete(request.Metadata, kiroWebSearchKey)
	delete(request.Metadata, kiroFakeReasoningKey)
	if s.KiroWebSearchInject || acc.Kiro != nil && acc.Kiro.WebSearch {
		request.Metadata[kiroWebSearchKey] = "1"
	}
	if acc.Kiro != nil && acc.Kiro.FakeReasoning {
		request.Metadata[kiroFakeReasoningKey] = "1"
	}

	token, state, tokenErr := runtime.Auth.GetAccessToken(ctx)
	if tokenErr != nil {
		// 刷新失败原因必须可排障（瞬时网络 vs 凭据失效），错误串已截断脱敏。
		log.Printf("upstream phase=kiro_auth_failed request_id=%s target=%s error=%v", requestID, envelope.TargetID, tokenErr)
		return nil, &upstreamv1.Error{Code: upstreamv1.CodeCredentialRefresh, Message: "credential refresh failed", Retryable: true, Status: http.StatusServiceUnavailable}
	}
	if state.ProfileArn != "" {
		request.Metadata[kiroProfileARNKey] = state.ProfileArn
	}
	codec := kiro.Codec{}
	body, encodeErr := codec.EncodeRequest(&request)
	if encodeErr != nil {
		return nil, &upstreamv1.Error{Code: upstreamv1.CodeInvalidRequest, Message: "encode kiro request: " + encodeErr.Error(), Status: http.StatusBadRequest}
	}

	execCtx, cancel := context.WithCancel(ctx)
	httpReq, reqErr := http.NewRequestWithContext(execCtx, http.MethodPost, runtime.Auth.ChatHost()+"/generateAssistantResponse", bytes.NewReader(body))
	if reqErr != nil {
		cancel()
		return nil, internalError()
	}
	for k, v := range account.KiroHeaders(runtime.Auth.Fingerprint(), token, account.TargetGenerateAssistantResponse) {
		httpReq.Header.Set(k, v)
	}
	httpReq.Header.Set("Connection", "close")
	client := s.KiroHTTPClient
	if client == nil {
		client = &http.Client{Transport: account.KiroTransport()}
	}
	providerURL, _ := url.Parse(httpReq.URL.String())
	providerStarted := time.Now()
	if s.AccessLogEnabled {
		log.Printf("upstream phase=provider_out request_id=%s host=%s path=%s body_bytes=%d header_count=%d", requestID, providerURL.Host, providerURL.Path, len(body), len(httpReq.Header))
	}
	resp, doErr := client.Do(httpReq)
	if doErr != nil {
		if s.AccessLogEnabled {
			log.Printf("upstream phase=provider_in request_id=%s status=0 content_type= latency=%s error=true", requestID, time.Since(providerStarted))
		}
		cancel()
		if ctx.Err() != nil {
			return nil, &upstreamv1.Error{Code: upstreamv1.CodeTargetUnavailable, Message: "request cancelled", Status: 499}
		}
		return nil, &upstreamv1.Error{Code: upstreamv1.CodeTargetUnavailable, Message: "kiro upstream unavailable", Retryable: true, Status: http.StatusBadGateway}
	}
	if s.AccessLogEnabled {
		log.Printf("upstream phase=provider_in request_id=%s status=%d content_type=%s latency=%s error=false", requestID, resp.StatusCode, resp.Header.Get("Content-Type"), time.Since(providerStarted))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxKiroErrorBody))
		resp.Body.Close()
		cancel()
		reason, message := account.ParseKiroErrorReason(errBody)
		if message == "" {
			message = strings.TrimSpace(string(errBody))
		}
		if message == "" {
			message = resp.Status
		}
		return nil, &upstreamv1.Error{Code: upstreamv1.CodeTargetUnavailable, Message: message, Retryable: account.ClassifyKiroError(resp.StatusCode, reason) == account.ClassRecoverable || resp.StatusCode >= 500, Status: resp.StatusCode, Reason: reason}
	}

	source := io.ReadCloser(resp.Body)
	if s.KiroStreamingReadTimeout > 0 {
		source = newReadTimeoutBody(source, s.KiroStreamingReadTimeout)
	}
	reader := bufio.NewReader(kiro.NewEventStreamToSSE(source))
	decoder := codec.NewStreamDecoder()
	if setter, ok := decoder.(interface{ SetModel(string) }); ok {
		setter.SetModel(native)
	}
	if acc.Kiro != nil && acc.Kiro.FakeReasoning {
		if setter, ok := decoder.(interface{ SetFakeReasoning(bool) }); ok {
			setter.SetFakeReasoning(true)
		}
	}
	if setter, ok := decoder.(interface{ SetMaxInputTokens(int) }); ok {
		setter.SetMaxInputTokens(int(runtime.Models.MaxInputTokens(native)))
	}

	firstStarted := time.Now()
	first, firstRawEvents, firstBytes, firstErr := awaitKiroFirstEvents(reader, decoder, cancel, s.KiroFirstTokenTimeout)
	if firstErr != nil {
		source.Close()
		cancel()
		if s.AccessLogEnabled {
			log.Printf("upstream phase=stream_first request_id=%s events=0 raw_events=%d bytes=%d latency=%s error=true", requestID, firstRawEvents, firstBytes, time.Since(firstStarted))
		}
		return nil, firstErr
	}
	if s.AccessLogEnabled {
		log.Printf("upstream phase=stream_first request_id=%s events=%d raw_events=%d bytes=%d latency=%s error=false", requestID, len(first), firstRawEvents, firstBytes, time.Since(firstStarted))
	}
	var closeOnce sync.Once
	closeAll := func() { closeOnce.Do(func() { source.Close(); cancel() }) }
	return &KiroExecution{
		First: first,
		Close: closeAll,
		Continue: func(emit func(ir.Event) error) (continueErr error) {
			defer closeAll()
			rawEvents := firstRawEvents
			decodedEvents := len(first)
			streamBytes := firstBytes
			defer func() {
				if s.AccessLogEnabled {
					log.Printf("upstream phase=stream_done request_id=%s raw_events=%d events=%d bytes=%d latency=%s error=%t", requestID, rawEvents, decodedEvents, streamBytes, time.Since(started), continueErr != nil)
				}
			}()
			for {
				raw, readErr := nextKiroEvent(reader)
				if readErr != nil {
					if readErr != io.EOF && execCtx.Err() == nil {
						continueErr = emit(ir.Event{Type: ir.EvError, Err: &ir.Error{StatusCode: http.StatusBadGateway, Type: ir.ErrTypeUpstream, Message: "kiro stream read: " + readErr.Error(), Retryable: true}})
						return continueErr
					}
					break
				}
				rawEvents++
				streamBytes += len(raw)
				events, decodeErr := decoder.Feed("", raw)
				if decodeErr != nil {
					continueErr = emit(ir.Event{Type: ir.EvError, Err: &ir.Error{StatusCode: http.StatusBadGateway, Type: ir.ErrTypeUpstream, Message: "kiro stream decode: " + decodeErr.Error(), Retryable: true}})
					return continueErr
				}
				decodedEvents += len(events)
				for _, event := range events {
					if err := emit(event); err != nil {
						continueErr = err
						return continueErr
					}
				}
			}
			finished := decoder.Finish()
			decodedEvents += len(finished)
			if reporter, ok := decoder.(proto.TruncationReporter); ok && len(finished) > 0 {
				last := &finished[len(finished)-1]
				for _, tool := range reporter.TruncatedTools() {
					last.TruncatedTools = append(last.TruncatedTools, ir.TruncatedTool{ID: tool.ID, Name: tool.Name, Reason: tool.Reason})
				}
				if reporter.ContentTruncated() {
					last.TruncatedContent = reporter.TruncatedContent()
				}
			}
			for _, event := range finished {
				if err := emit(event); err != nil {
					continueErr = err
					return continueErr
				}
			}
			return nil
		},
	}, nil
}

type kiroFirstResult struct {
	events    []ir.Event
	rawEvents int
	bytes     int
	err       error
}

func awaitKiroFirstEvents(reader *bufio.Reader, decoder proto.StreamDecoder, cancel context.CancelFunc, timeout time.Duration) ([]ir.Event, int, int, *upstreamv1.Error) {
	ch := make(chan kiroFirstResult, 1)
	go func() {
		result := kiroFirstResult{}
		for {
			raw, err := nextKiroEvent(reader)
			if err != nil {
				result.err = err
				ch <- result
				return
			}
			result.rawEvents++
			result.bytes += len(raw)
			events, err := decoder.Feed("", raw)
			if err != nil {
				result.err = fmt.Errorf("decode: %w", err)
				ch <- result
				return
			}
			if len(events) > 0 {
				result.events = events
				ch <- result
				return
			}
		}
	}()
	if timeout <= 0 {
		return firstKiroResult(<-ch)
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case r := <-ch:
		return firstKiroResult(r)
	case <-timer.C:
		cancel()
		return nil, 0, 0, &upstreamv1.Error{Code: upstreamv1.CodeTargetUnavailable, Message: fmt.Sprintf("kiro produced no event within %s", timeout), Retryable: true, Status: http.StatusGatewayTimeout}
	}
}

func firstKiroResult(r kiroFirstResult) ([]ir.Event, int, int, *upstreamv1.Error) {
	if r.err == nil {
		return r.events, r.rawEvents, r.bytes, nil
	}
	message := "kiro closed stream without a valid event"
	if r.err != io.EOF {
		message = "kiro first event " + r.err.Error()
	}
	return nil, r.rawEvents, r.bytes, &upstreamv1.Error{Code: upstreamv1.CodeTargetUnavailable, Message: message, Retryable: true, Status: http.StatusBadGateway}
}

func nextKiroEvent(reader *bufio.Reader) (string, error) {
	for {
		line, err := reader.ReadString('\n')
		if err != nil && len(line) == 0 {
			return "", err
		}
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "data:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "data:")), nil
		}
		if err != nil {
			return "", err
		}
	}
}

type readTimeoutBody struct {
	body    io.ReadCloser
	timeout time.Duration

	mu     sync.Mutex
	timer  *time.Timer
	closed bool
}

func newReadTimeoutBody(body io.ReadCloser, timeout time.Duration) io.ReadCloser {
	b := &readTimeoutBody{body: body, timeout: timeout}
	b.timer = time.AfterFunc(timeout, func() { _ = body.Close() })
	return b
}

func (b *readTimeoutBody) Read(p []byte) (int, error) {
	n, err := b.body.Read(p)
	if err == nil {
		b.mu.Lock()
		if !b.closed {
			b.timer.Reset(b.timeout)
		}
		b.mu.Unlock()
	}
	return n, err
}

func (b *readTimeoutBody) Close() error {
	b.mu.Lock()
	b.closed = true
	if b.timer != nil {
		b.timer.Stop()
	}
	b.mu.Unlock()
	return b.body.Close()
}
