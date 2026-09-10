package egress

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"relayd/strategy"
)

type Forwarder struct {
	Client  *http.Client
	Timeout time.Duration
}

func NewForwarder() *Forwarder {
	return &Forwarder{Client: &http.Client{}}
}

// NewForwarderWithTimeout caps the whole upstream exchange (including a
// streamed body) at d. Zero means no cap.
func NewForwarderWithTimeout(d time.Duration) *Forwarder {
	return &Forwarder{Client: &http.Client{}, Timeout: d}
}

// EndpointFor maps protocol to the chat endpoint path.
func EndpointFor(protocol string) (string, error) {
	switch protocol {
	case "openai":
		return "/v1/chat/completions", nil
	case "claude":
		return "/v1/messages", nil
	}
	return "", fmt.Errorf("no endpoint for protocol %q", protocol)
}

// Forward sends body to the upstream, injecting auth and returning the raw
// response. Callers must close resp.Body.
func (f *Forwarder) Forward(ctx context.Context, u *strategy.Upstream, body []byte, origHeader http.Header) (*http.Response, error) {
	path, err := EndpointFor(u.Protocol)
	if err != nil {
		return nil, err
	}
	url := strings.TrimSuffix(u.BaseURL, "/") + path

	var cancel context.CancelFunc
	if f.Timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, f.Timeout)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		if cancel != nil {
			cancel()
		}
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	// let Transport negotiate encoding; a client-requested gzip would buffer SSE
	req.Header.Del("Accept-Encoding")

	switch u.Protocol {
	case "openai":
		req.Header.Set("Authorization", "Bearer "+u.APIKey)
	case "claude":
		req.Header.Set("x-api-key", u.APIKey)
		req.Header.Set("anthropic-version", "2023-06-01")
	}

	resp, err := f.Client.Do(req)
	if err != nil {
		if cancel != nil {
			cancel()
		}
		return nil, err
	}
	if cancel != nil {
		resp.Body = &cancelOnClose{ReadCloser: resp.Body, cancel: cancel}
	}
	return resp, nil
}

// cancelOnClose cancels the request context when the body is closed; the body
// outlives Forward (streaming), so the deadline must live until then.
type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelOnClose) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}

// ReadErrorBody bounds error-body reads; error responses are never SSE.
func ReadErrorBody(resp *http.Response) []byte {
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<10))
	return b
}

// CopySuccessHeaders forwards safe headers from upstream to client.
func CopySuccessHeaders(w http.ResponseWriter, resp *http.Response) {
	for _, h := range []string{"Content-Type", "Cache-Control", "X-Accel-Buffering"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
}
