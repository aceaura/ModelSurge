package upstream

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
	if model.Protocol != "kiro" || !model.Enabled || model.CooldownUntil.After(time.Now()) {
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
	request.Stream = true
	native := model.NativeModel
	if (native == "" || native == "*") && model.DisplayName != "*" {
		native, _ = runtime.Resolve(model.DisplayName)
	}
	request.Model = native
	if len(model.RequestOverrides) > 0 && string(model.RequestOverrides) != "null" {
		var overrides ir.Overrides
		if json.Unmarshal(model.RequestOverrides, &overrides) == nil {
			overrides.Apply(&request)
		}
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
	resp, doErr := client.Do(httpReq)
	if doErr != nil {
		cancel()
		if ctx.Err() != nil {
			return nil, &upstreamv1.Error{Code: upstreamv1.CodeTargetUnavailable, Message: "request cancelled", Status: 499}
		}
		return nil, &upstreamv1.Error{Code: upstreamv1.CodeTargetUnavailable, Message: "kiro upstream unavailable", Retryable: true, Status: http.StatusBadGateway}
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

	first, firstErr := awaitKiroFirstEvents(reader, decoder, cancel, s.KiroFirstTokenTimeout)
	if firstErr != nil {
		source.Close()
		cancel()
		return nil, firstErr
	}
	var closeOnce sync.Once
	closeAll := func() { closeOnce.Do(func() { source.Close(); cancel() }) }
	return &KiroExecution{
		First: first,
		Close: closeAll,
		Continue: func(emit func(ir.Event) error) error {
			defer closeAll()
			for {
				raw, readErr := nextKiroEvent(reader)
				if readErr != nil {
					if readErr != io.EOF && execCtx.Err() == nil {
						return emit(ir.Event{Type: ir.EvError, Err: &ir.Error{StatusCode: http.StatusBadGateway, Type: ir.ErrTypeUpstream, Message: "kiro stream read: " + readErr.Error(), Retryable: true}})
					}
					break
				}
				events, decodeErr := decoder.Feed("", raw)
				if decodeErr != nil {
					return emit(ir.Event{Type: ir.EvError, Err: &ir.Error{StatusCode: http.StatusBadGateway, Type: ir.ErrTypeUpstream, Message: "kiro stream decode: " + decodeErr.Error(), Retryable: true}})
				}
				for _, event := range events {
					if err := emit(event); err != nil {
						return err
					}
				}
			}
			finished := decoder.Finish()
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
					return err
				}
			}
			return nil
		},
	}, nil
}

type kiroFirstResult struct {
	events []ir.Event
	err    error
}

func awaitKiroFirstEvents(reader *bufio.Reader, decoder proto.StreamDecoder, cancel context.CancelFunc, timeout time.Duration) ([]ir.Event, *upstreamv1.Error) {
	ch := make(chan kiroFirstResult, 1)
	go func() {
		for {
			raw, err := nextKiroEvent(reader)
			if err != nil {
				ch <- kiroFirstResult{err: err}
				return
			}
			events, err := decoder.Feed("", raw)
			if err != nil {
				ch <- kiroFirstResult{err: fmt.Errorf("decode: %w", err)}
				return
			}
			if len(events) > 0 {
				ch <- kiroFirstResult{events: events}
				return
			}
		}
	}()
	if timeout <= 0 {
		r := <-ch
		return firstKiroResult(r)
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case r := <-ch:
		return firstKiroResult(r)
	case <-timer.C:
		cancel()
		return nil, &upstreamv1.Error{Code: upstreamv1.CodeTargetUnavailable, Message: fmt.Sprintf("kiro produced no event within %s", timeout), Retryable: true, Status: http.StatusGatewayTimeout}
	}
}

func firstKiroResult(r kiroFirstResult) ([]ir.Event, *upstreamv1.Error) {
	if r.err == nil {
		return r.events, nil
	}
	message := "kiro closed stream without a valid event"
	if r.err != io.EOF {
		message = "kiro first event " + r.err.Error()
	}
	return nil, &upstreamv1.Error{Code: upstreamv1.CodeTargetUnavailable, Message: message, Retryable: true, Status: http.StatusBadGateway}
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
