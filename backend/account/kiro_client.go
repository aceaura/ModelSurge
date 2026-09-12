// kiro_client.go kiro 控制面 HTTP client（KiroaaS http_client.py + utils.get_kiro_headers
// + extensions/control_plane_host + chat_host_fallback 的 Go 翻译）。
// host 分流：聊天走 runtime.{region}.kiro.dev（无 profileArn 的免费账号回落
// q.{region}.amazonaws.com）；控制面（ListAvailableModels/GetUsageLimits/
// ListAvailableProfiles）固定 q.{region}.amazonaws.com——runtime 端点对控制面
// 操作返回 400/404（UnknownOperationException）。
package account

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"time"

	"github.com/google/uuid"

	"relayd/backend/ir"
)

// host 模板（测试可替换）。
var (
	runtimeHostTemplate = "https://runtime.%s.kiro.dev"
	qHostTemplate       = "https://q.%s.amazonaws.com"
)

// kiroUA 完整 UA 指纹（伪造 KiroIDE 客户端）。
const kiroUAFmt = "aws-sdk-js/1.0.27 ua/2.1 os/win32#10.0.19044 lang/js md/nodejs#22.21.1 api/codewhispererstreaming#1.0.27 m/E KiroIDE-0.7.45-%s"

// 控制面 x-amz-target。
const (
	targetListProfiles = "AmazonCodeWhispererService.ListAvailableProfiles"
	targetGetUsage     = "com.amazon.aws.codewhisperer.runtime.AmazonCodeWhispererService.GetUsageLimits"
)

// ChatHost 聊天请求 host：runtime.{api_region}.kiro.dev；
// 无 profileArn 的免费账号（Builder ID，ListAvailableProfiles 403 拿不到
// profileArn，而 runtime 端点每个请求都要求 profileArn）回落
// q.{region}.amazonaws.com（同 token 同请求形状，验证过不 400）。
func (s *AuthService) ChatHost() string {
	s.mu.Lock()
	hasProfile := s.token.ProfileArn != "" || s.k.ProfileArn != ""
	s.mu.Unlock()
	region := s.EffectiveAPIRegion()
	if !hasProfile {
		return fmt.Sprintf(qHostTemplate, region)
	}
	return fmt.Sprintf(runtimeHostTemplate, region)
}

// ControlPlaneHost 控制面 host：固定 q.{region}.amazonaws.com。
func (s *AuthService) ControlPlaneHost() string {
	return fmt.Sprintf(qHostTemplate, s.EffectiveAPIRegion())
}

// KiroHeaders 聊天请求头（Authorization/x-amz-target/伪造 UA/随机 invocation id）。
// target 为 x-amz-target 值（聊天为 GenerateAssistantResponse）。
func KiroHeaders(fingerprint, token, target string) map[string]string {
	return map[string]string{
		"Authorization":               "Bearer " + token,
		"Content-Type":                "application/x-amz-json-1.0",
		"x-amz-target":                target,
		"User-Agent":                  fmt.Sprintf(kiroUAFmt, fingerprint),
		"x-amz-user-agent":            "aws-sdk-js/1.0.27 KiroIDE-0.7.45-" + fingerprint,
		"x-amzn-codewhisperer-optout": "true",
		"x-amzn-kiro-agent-mode":      "vibe",
		"amz-sdk-invocation-id":       uuid.NewString(),
		"amz-sdk-request":             "attempt=1; max=3",
	}
}

// 聊天 x-amz-target（generateAssistantResponse 流式）。
const TargetGenerateAssistantResponse = "AmazonCodeWhispererStreamingService.GenerateAssistantResponse"

// KiroModel /ListAvailableModels 的模型条目。
type KiroModel struct {
	ModelID     string `json:"modelId"`
	ModelName   string `json:"modelName"`
	Description string `json:"description"`
	TokenLimits struct {
		MaxInputTokens int64 `json:"maxInputTokens"`
	} `json:"tokenLimits"`
}

// KiroClient 控制面 client：3 次指数退避重试（1s×2^n）；
// 403 → 强刷 token 重试；429/5xx/网络错误 → 退避。
type KiroClient struct {
	auth       *AuthService
	client     *http.Client
	maxRetries int
	retryDelay time.Duration
	sleep      func(ctx context.Context, d time.Duration) error
}

// NewKiroClient 构造控制面 client。
func NewKiroClient(auth *AuthService) *KiroClient {
	return &KiroClient{
		auth:       auth,
		client:     &http.Client{Timeout: 30 * time.Second, Transport: KiroTransport()},
		maxRetries: 3,
		retryDelay: time.Second,
		sleep:      ctxSleep,
	}
}

func ctxSleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// ListAvailableModels 拉取可用模型列表（origin=AI_EDITOR；desktop 型带
// profileArn）。runtime 端点不提供该 API（404），调用方需自行回落静态表。
func (c *KiroClient) ListAvailableModels(ctx context.Context) ([]KiroModel, error) {
	q := url.Values{"origin": []string{"AI_EDITOR"}}
	if arn := c.auth.EffectiveProfileArn(); arn != "" {
		q.Set("profileArn", arn)
	}
	body, err := c.request(ctx, http.MethodGet,
		c.auth.ControlPlaneHost()+"/ListAvailableModels?"+q.Encode(), nil,
		TargetGenerateAssistantResponse, "application/x-amz-json-1.0")
	if err != nil {
		return nil, err
	}
	var resp struct {
		Models []KiroModel `json:"models"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("kiro client: list models parse: %w", err)
	}
	return resp.Models, nil
}

// GetUsageLimits 查询账号配额（402 冷却时解析重置日期用；管理面 /usage 用）。
// 返回原始 JSON（字段随上游演进，不强行建模）。
func (c *KiroClient) GetUsageLimits(ctx context.Context) (json.RawMessage, error) {
	arn := c.auth.EffectiveProfileArn()
	if arn == "" {
		return nil, fmt.Errorf("kiro client: no profileArn for usage limits")
	}
	body, _ := json.Marshal(map[string]string{
		"profileArn":   arn,
		"origin":       "AI_EDITOR",
		"resourceType": "AGENTIC_REQUEST",
	})
	raw, err := c.request(ctx, http.MethodPost,
		c.auth.ControlPlaneHost()+"/GetUsageLimits", body,
		targetGetUsage, "application/x-amz-json-1.0")
	if err != nil {
		return nil, err
	}
	return json.RawMessage(raw), nil
}

// ListAvailableProfiles 取第一个 profile 的 ARN（profileArn 自动回填用）。
// host 用 SSO 区（profile_arn_autofetch 的行为）。
// 会经 ensureProfileArn 在 GetAccessToken 锁外调用；用免锁的
// SSORegionLocked 避免 IO 期间持锁（区域字段构造后不变）。
func (c *KiroClient) ListAvailableProfiles(ctx context.Context) (string, error) {
	body, _ := json.Marshal(map[string]any{"nextToken": nil})
	raw, err := c.request(ctx, http.MethodPost,
		fmt.Sprintf(qHostTemplate, c.auth.SSORegionLocked())+"/", body,
		targetListProfiles, "application/json; charset=UTF-8")
	if err != nil {
		return "", err
	}
	var resp struct {
		Profiles []struct {
			Arn         string `json:"arn"`
			ProfileArn  string `json:"profileArn"`
			ProfileName string `json:"profileName"`
		} `json:"profiles"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return "", fmt.Errorf("kiro client: list profiles parse: %w", err)
	}
	if len(resp.Profiles) == 0 {
		return "", fmt.Errorf("kiro client: no profiles available")
	}
	arn := resp.Profiles[0].Arn
	if arn == "" {
		arn = resp.Profiles[0].ProfileArn
	}
	if arn == "" {
		return "", fmt.Errorf("kiro client: first profile has no arn")
	}
	return arn, nil
}

// CallWebSearch 经 Kiro MCP API 执行 web_search（mcp_tools.py
// call_kiro_mcp_api 的 Go 翻译）。POST {q_host}/mcp，JSON-RPC tools/call；
// 头与控制面不同——仅 Bearer + optout(false) + application/json，
// 无 x-amz-target。单次 60s 无重试（Python 同款），失败由调用方降级。
// 返回合成 server_tool_use 的 id（"srvtoolu_" 前缀）与结果列表。
func (c *KiroClient) CallWebSearch(ctx context.Context, query string) (string, []ir.WebSearchResult, error) {
	token, _, err := c.auth.GetAccessToken(ctx)
	if err != nil {
		return "", nil, fmt.Errorf("kiro mcp: token: %w", err)
	}
	mcpReq := map[string]any{
		"id":      fmt.Sprintf("web_search_tooluse_%s_%d_%s", randID(22), time.Now().UnixMilli(), randID(8)),
		"jsonrpc": "2.0",
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "web_search",
			"arguments": map[string]string{"query": query},
		},
	}
	body, _ := json.Marshal(mcpReq)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.auth.ControlPlaneHost()+"/mcp", bytes.NewReader(body))
	if err != nil {
		return "", nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("x-amzn-codewhisperer-optout", "false")
	req.Header.Set("Content-Type", "application/json")

	client := c.client
	if client.Timeout < 60*time.Second {
		cl := *client
		cl.Timeout = 60 * time.Second
		client = &cl
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", nil, fmt.Errorf("kiro mcp: request: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != 200 {
		return "", nil, fmt.Errorf("kiro mcp: %s returned %d: %s",
			req.URL.Path, resp.StatusCode, excerptStr(string(respBody)))
	}
	var mcpResp struct {
		Error  *json.RawMessage `json:"error"`
		Result *struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(respBody, &mcpResp); err != nil {
		return "", nil, fmt.Errorf("kiro mcp: parse: %w", err)
	}
	if mcpResp.Error != nil {
		return "", nil, fmt.Errorf("kiro mcp: rpc error: %s", excerptStr(string(*mcpResp.Error)))
	}
	if mcpResp.Result == nil || len(mcpResp.Result.Content) == 0 {
		return "", nil, fmt.Errorf("kiro mcp: empty result")
	}
	// content[0].text 是 JSON 字符串（非对象），需二次解析
	var payload struct {
		Results []ir.WebSearchResult `json:"results"`
	}
	if err := json.Unmarshal([]byte(mcpResp.Result.Content[0].Text), &payload); err != nil {
		return "", nil, fmt.Errorf("kiro mcp: parse results: %w", err)
	}
	return "srvtoolu_" + randHex(32), payload.Results, nil
}

// randID 随机字母数字串（MCP 请求 id 用）。
func randID(n int) string {
	const chars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, n)
	_, _ = crand.Read(b)
	for i, v := range b {
		b[i] = chars[int(v)%len(chars)]
	}
	return string(b)
}

// randHex 随机十六进制串。
func randHex(n int) string {
	b := make([]byte, (n+1)/2)
	_, _ = crand.Read(b)
	return hex.EncodeToString(b)[:n]
}

// request 带重试的控制面请求：200 返回 body；
// 403 → 强刷 token 原地重试；429/5xx/网络错误 → 指数退避。
func (c *KiroClient) request(ctx context.Context, method, u string, body []byte, target, contentType string) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt < c.maxRetries; attempt++ {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		token, _, err := c.auth.GetAccessToken(ctx)
		if err != nil {
			return nil, err // 凭据失败不是网络问题，直接透出
		}
		headers := KiroHeaders(c.auth.Fingerprint(), token, target)
		headers["Content-Type"] = contentType

		var rdr io.Reader
		if body != nil {
			rdr = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, u, rdr)
		if err != nil {
			return nil, err
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := c.client.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("kiro client: request: %w", err)
		} else {
			respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			switch {
			case resp.StatusCode == 200:
				return respBody, nil
			case resp.StatusCode == 403:
				log.Printf("kiro client: 403 from %s, force refreshing token (attempt %d)", u, attempt+1)
				if err := c.auth.ForceRefresh(ctx); err != nil {
					return nil, fmt.Errorf("kiro client: force refresh after 403: %w", err)
				}
				lastErr = fmt.Errorf("kiro client: 403: %s", excerptStr(string(respBody)))
				continue // 立即用新 token 重试（无退避）
			default:
				lastErr = fmt.Errorf("kiro client: %s %s returned %d: %s",
					method, u, resp.StatusCode, excerptStr(string(respBody)))
			}
		}
		// 429/5xx/网络：指数退避 1s×2^n
		if err := c.sleep(ctx, c.retryDelay*time.Duration(1<<attempt)); err != nil {
			return nil, err
		}
	}
	return nil, lastErr
}
