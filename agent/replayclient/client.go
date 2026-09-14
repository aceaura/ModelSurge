package replayclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/aceaura/ModelSurge/replay/contract/replayv1"
)

type Client struct {
	baseURL    string
	key        string
	http       *http.Client
	streamHTTP *http.Client
}

func New(baseURL, serviceKey string, timeout time.Duration) *Client {
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), key: serviceKey, http: &http.Client{Timeout: timeout}, streamHTTP: &http.Client{}}
}

func (c *Client) Health(ctx context.Context) (replayv1.HealthResponse, error) {
	var out replayv1.HealthResponse
	return out, c.do(ctx, http.MethodGet, "/health", nil, &out)
}
func (c *Client) Models(ctx context.Context) ([]replayv1.ModelSummary, error) {
	var out replayv1.ModelsResponse
	if err := c.do(ctx, http.MethodGet, "/models", nil, &out); err != nil {
		return nil, err
	}
	return out.Models, nil
}
func (c *Client) Dispatch(ctx context.Context, req replayv1.DispatchRequest) (replayv1.TargetLease, error) {
	var out replayv1.TargetLease
	return out, c.do(ctx, http.MethodPost, "/dispatch", req, &out)
}
func (c *Client) Report(ctx context.Context, report replayv1.ResultReport) (replayv1.ResultResponse, error) {
	var out replayv1.ResultResponse
	return out, c.do(ctx, http.MethodPost, "/results", report, &out)
}
func (c *Client) WebSearch(ctx context.Context, req replayv1.WebSearchRequest) (replayv1.WebSearchResponse, error) {
	var out replayv1.WebSearchResponse
	return out, c.do(ctx, http.MethodPost, "/kiro/web-search", req, &out)
}
func (c *Client) ExecuteKiro(ctx context.Context, req replayv1.KiroExecuteRequest) (*http.Response, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+replayv1.BasePath+"/kiro/execute", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.key)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/x-ndjson, application/json")
	client := c.streamHTTP
	if client == nil {
		client = &http.Client{}
	}
	return client.Do(httpReq)
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
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+replayv1.BasePath+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var env replayv1.ErrorEnvelope
		if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&env) == nil && env.Error.Code != "" {
			return env.Error
		}
		return fmt.Errorf("replay returned %s", resp.Status)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
}
