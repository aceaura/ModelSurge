package probe

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"relayd/backend/catalog"
	"relayd/strategy"
)

type pingProber struct {
	cfg catalog.ProbeConfig
}

func (p *pingProber) Name() string            { return "ping" }
func (p *pingProber) Interval() time.Duration { return p.cfg.Interval.D() }

// Probe sends a minimal business request and classifies the response.
// It never bills, never resolves users — one tiny request, classify, done.
func (p *pingProber) Probe(ctx context.Context, t Target) strategy.Signal {
	start := time.Now()
	sig := strategy.Signal{Kind: strategy.KindPing, At: start}

	model := p.cfg.Model
	if model == "" && len(t.Models) > 0 {
		model = t.Models[0]
	}
	native := model
	if v, ok := t.Native[model]; ok {
		native = v
	}

	// OpenAI-shaped minimal request; claude providers get the same shape with
	// max_tokens (accepted by both /v1/chat/completions and /v1/messages paths
	// used by relay stations).
	payload := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}],"max_tokens":1}`, native)

	path := "/v1/chat/completions"
	if p.cfg.Request != nil && p.cfg.Request.Path != "" {
		path = p.cfg.Request.Path
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		trimSlash(t.BaseURL)+path, bytes.NewReader([]byte(payload)))
	if err != nil {
		sig.OK = false
		sig.Reason = err.Error()
		return sig
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+t.APIKey)
	req.Header.Set("x-api-key", t.APIKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := t.Client.Do(req)
	if err != nil {
		sig.OK = false
		sig.Reason = "ping: " + err.Error()
		return sig
	}
	sig.Latency = time.Since(start)

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<10))
		resp.Body.Close()
		sig = strategy.FromResponse(resp.StatusCode, resp.Header, body)
		sig.Kind = strategy.KindPing
		sig.At = start
		sig.Latency = time.Since(start)
		return sig
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, maxProbeBody))
	resp.Body.Close()

	sig.OK = true
	if th := p.cfg.SlowThreshold.D(); th > 0 && sig.Latency > th {
		sig.OK = false
		sig.Reason = "slow: " + sig.Latency.String()
	}
	return sig
}

func trimSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}
