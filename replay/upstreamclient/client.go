// Package upstreamclient implements relay's bounded, authenticated control-plane client.
package upstreamclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aceaura/ModelSurge/upstream/contract/upstreamv1"
)

type Client struct {
	BaseURL, ServiceKey string
	HTTP                *http.Client
	StreamHTTP          *http.Client
	MaxResponse         int64
}

func New(base, key string, timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &Client{BaseURL: strings.TrimRight(base, "/"), ServiceKey: key, HTTP: &http.Client{Timeout: timeout}, StreamHTTP: &http.Client{}, MaxResponse: 2 << 20}
}
func (c *Client) Health(ctx context.Context) error {
	var out upstreamv1.HealthResponse
	return c.do(ctx, "GET", "/health", nil, &out)
}
func (c *Client) Models(ctx context.Context) ([]upstreamv1.ModelSummary, error) {
	var out upstreamv1.ModelsResponse
	err := c.do(ctx, "GET", "/models", nil, &out)
	return out.Models, err
}
func (c *Client) Resolve(ctx context.Context, id string) (upstreamv1.ResolvedTarget, error) {
	var out upstreamv1.ResolvedTarget
	err := c.do(ctx, "GET", "/models/"+url.PathEscape(id)+"/resolve", nil, &out)
	if err == nil {
		err = upstreamv1.ValidateResolvedTarget(out)
	}
	return out, err
}
func (c *Client) Evaluate(ctx context.Context, ids []string) ([]upstreamv1.CandidateEvaluation, error) {
	var out upstreamv1.EvaluateResponse
	err := c.do(ctx, "POST", "/candidates/evaluate", upstreamv1.EvaluateRequest{TargetIDs: ids}, &out)
	return out.Candidates, err
}
func (c *Client) Report(ctx context.Context, r upstreamv1.ResultReport) (upstreamv1.ResultResponse, error) {
	var out upstreamv1.ResultResponse
	return out, c.do(ctx, "POST", "/results", r, &out)
}
func (c *Client) WebSearch(ctx context.Context, r upstreamv1.WebSearchRequest) (upstreamv1.WebSearchResponse, error) {
	var out upstreamv1.WebSearchResponse
	return out, c.do(ctx, "POST", "/kiro/web-search", r, &out)
}
func (c *Client) ExecuteKiro(ctx context.Context, r upstreamv1.KiroExecuteRequest) (*http.Response, error) {
	body, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+upstreamv1.BasePath+"/kiro/execute", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.ServiceKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/x-ndjson, application/json")
	client := c.StreamHTTP
	if client == nil {
		client = &http.Client{}
	}
	return client.Do(req)
}
func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+upstreamv1.BasePath+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.ServiceKey)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("upstream control unavailable: %w", err)
	}
	defer resp.Body.Close()
	limit := c.MaxResponse
	if limit <= 0 {
		limit = 2 << 20
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return err
	}
	if int64(len(b)) > limit {
		return fmt.Errorf("upstream control response too large")
	}
	if !strings.Contains(resp.Header.Get("Content-Type"), "application/json") {
		return fmt.Errorf("upstream control returned non-json")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var env upstreamv1.ErrorEnvelope
		if json.Unmarshal(b, &env) == nil && env.Error.Code != "" {
			return env.Error
		}
		return fmt.Errorf("upstream control status %d", resp.StatusCode)
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("decode upstream control response: %w", err)
	}
	return nil
}
