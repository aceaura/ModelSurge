// probe.go 上游端点自动探测适配（api-key 型账号专用；kiro 端点固定不参与）。
// 职责：把用户随手填写的 base_url（裸域名 / SDK 风格带 /v1 / 网关 /api 前缀 /
// 粘贴的完整端点 URL）规范化为协议根地址，并以 GET 模型列表为探针（零 token
// 消耗）并发探测候选端点，解析出的根地址由管理面写回账号 base_url 持久化。
// 探测只发生在管理面写路径；转发期端点保持静态拼接（relay/forward.go endpoint）。
package account

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// 探测行为常量（不进 yaml：探测属固定行为，避免配置面膨胀）。
const (
	probeTimeout   = 5 * time.Second // 全部候选并发探测的总超时
	probeBodyLimit = 64 * 1024       // 模型列表响应体的读取上限（列表本身可达数十 KB）
	probeModelsCap = 100             // 报告中模型 id 的封顶数（仅报告，不落库）
)

// NormalizeBaseURL 规范化用户输入的 base_url，返回协议根地址（不含版本段，
// 与 relay/forward.go endpoint() 的拼接语义一致：root + "/v1/messages" 等）。
// 规则：去空白 -> 无 scheme 默认补 https://（显式 http:// 保留）-> 去带外部分
// （query/fragment）-> host 小写 -> 剥已知端点尾段 -> 剥末段版本段（v1/v1beta/
// v1alpha）-> 去尾部 /。无法解析出 host 时报错（管理面回 400 的唯一探测前阻断）。
func NormalizeBaseURL(input string) (string, error) {
	s := strings.TrimSpace(input)
	if s == "" {
		return "", fmt.Errorf("required")
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", fmt.Errorf("invalid url: %v", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("scheme must be http or https")
	}
	if u.Host == "" {
		return "", fmt.Errorf("host is required")
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	u.RawQuery, u.Fragment, u.User = "", "", nil
	u.Path = strings.TrimRight(u.Path, "/")
	for strings.Contains(u.Path, "//") { // 收敛多斜杠，保证候选 URL 形态规整
		u.Path = strings.ReplaceAll(u.Path, "//", "/")
	}
	for changed := true; changed; {
		changed = false
		for _, suffix := range []string{"/chat/completions", "/messages", "/responses", "/models", "/count_tokens"} {
			if strings.HasSuffix(u.Path, suffix) {
				u.Path = strings.TrimRight(strings.TrimSuffix(u.Path, suffix), "/")
				changed = true
			}
		}
		// Gemini 粘贴形态 .../models/gemini-xxx:streamGenerateContent：
		// 末段含 action 冒号时视为 models/<resource> 的一部分，整体截到 models 之前。
		if i := strings.LastIndex(u.Path, "/models/"); i >= 0 {
			u.Path = u.Path[:i]
			changed = true
		}
		if seg := lastSeg(u.Path); isVersionSeg(seg) {
			u.Path = strings.TrimRight(strings.TrimSuffix(u.Path, "/"+seg), "/")
			changed = true
		}
	}
	return u.String(), nil
}

// CandidateRoots 按优先级生成候选协议根地址（去重，至多 4 个）：
// ① 规范化输入本身；② 输入追加 "/api"（末段已为 api 或版本段时跳过）；
// ③ 剥掉全部路径的裸 scheme://host（输入带路径时的末位兜底——用户路径前缀
// 给错时仍能探到）；④ 裸 host + "/api"。
func CandidateRoots(normalized string) []string {
	roots := []string{normalized}
	u, err := url.Parse(normalized)
	if err != nil {
		return roots
	}
	base := u.Scheme + "://" + u.Host
	if seg := lastSeg(u.Path); seg != "api" && !isVersionSeg(seg) {
		roots = append(roots, normalized+"/api")
	}
	if u.Path != "" {
		roots = append(roots, base)
		roots = append(roots, base+"/api") // 裸 host 无路径，恒可加 /api
	}
	if len(roots) > 4 {
		roots = roots[:4]
	}
	return dedupe(roots)
}

// 探测判定（verdict 值同时是 admin 响应的 JSON 形态）。
const (
	VerdictResolved    = "resolved"    // 200 且为模型列表 JSON：端点与密钥均有效
	VerdictAuthFailed  = "auth_failed" // 401/403：路径存在但密钥无效
	VerdictNotFound    = "not_found"   // 404：路径不存在
	VerdictUnexpected  = "unexpected"  // 其余状态码 / 200 但非模型列表 JSON
	VerdictUnreachable = "unreachable" // 网络层错误（超时/连接失败/DNS）
)

// ProbeAttempt 单个候选的探测证据。
type ProbeAttempt struct {
	URL     string `json:"url"`
	Status  int    `json:"status"` // 0 = 网络层错误
	Verdict string `json:"verdict"`
}

// ProbeReport 一次探测的整体报告（admin 响应中的 "probe" 字段；不落库）。
type ProbeReport struct {
	OK              bool           `json:"ok"`
	Verdict         string         `json:"verdict"` // resolved | auth_failed | unreachable | no_match
	ResolvedBaseURL string         `json:"resolved_base_url,omitempty"`
	Attempts        []ProbeAttempt `json:"attempts"`
	Models          []string       `json:"models,omitempty"` // 胜出候选返回的上游模型 id，封顶 probeModelsCap
}

// Resolved 返回探测确认的协议根地址（resolved 与恰一 auth_failed 时非空，
// 两种情形管理面都应写回 base_url）。
func (r *ProbeReport) Resolved() string { return r.ResolvedBaseURL }

// ProbeEndpoint 并发探测全部候选根地址（总超时 probeTimeout），按候选生成
// 顺序取优先级最高的胜出者。探针：GET {root}/{v1|v1beta}/models，鉴权头按
// 协议取用；响应体读至 probeBodyLimit。无 resolved 但恰好一个候选 401/403
// 时采纳该根地址（路径存在证据），整体 verdict=auth_failed。
// baseURL 接受原始输入：无 scheme 且主机为本地/内网地址时，自动加测 http
// 变体（http 优先）——本地自托管运行时（ollama/vllm 等）几乎不开 TLS。
func ProbeEndpoint(ctx context.Context, client *http.Client, protocol, baseURL, apiKey string) *ProbeReport {
	schemeless := strings.TrimSpace(baseURL) != "" && !strings.Contains(baseURL, "://")
	normalized, err := NormalizeBaseURL(baseURL)
	if err != nil {
		return &ProbeReport{Verdict: "no_match", Attempts: []ProbeAttempt{}}
	}
	if client == nil {
		client = http.DefaultClient
	}
	roots := CandidateRoots(normalized)
	if schemeless && isLocalHost(normalized) {
		roots = localHTTPTwins(roots)
	}
	version := "v1"
	if protocol == "gemini" {
		version = "v1beta"
	}
	pctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	type result struct {
		att    ProbeAttempt
		models []string
	}
	results := make([]result, len(roots))
	var wg sync.WaitGroup
	for i, root := range roots {
		wg.Add(1)
		go func(i int, root string) {
			defer wg.Done()
			att := ProbeAttempt{URL: root + "/" + version + "/models", Verdict: VerdictUnreachable}
			req, err := http.NewRequestWithContext(pctx, http.MethodGet, att.URL, nil)
			if err != nil {
				results[i] = result{att, nil}
				return
			}
			for k, v := range probeHeaders(protocol, apiKey) {
				req.Header.Set(k, v)
			}
			resp, err := client.Do(req)
			if err != nil {
				results[i] = result{att, nil}
				return
			}
			body, _ := io.ReadAll(io.LimitReader(resp.Body, probeBodyLimit))
			resp.Body.Close()
			att.Status = resp.StatusCode
			switch {
			case resp.StatusCode == http.StatusOK:
				if ids, ok := parseModelList(body); ok {
					att.Verdict = VerdictResolved
					results[i] = result{att, ids}
					return
				}
				att.Verdict = VerdictUnexpected
			case resp.StatusCode == 401 || resp.StatusCode == 403:
				att.Verdict = VerdictAuthFailed
			case resp.StatusCode == 404:
				att.Verdict = VerdictNotFound
			default:
				att.Verdict = VerdictUnexpected
			}
			results[i] = result{att, nil}
		}(i, root)
	}
	wg.Wait()

	rep := &ProbeReport{Attempts: make([]ProbeAttempt, len(roots))}
	var authIdx []int
	allUnreachable := true
	firstResolved := -1
	for i, r := range results {
		rep.Attempts[i] = r.att
		if r.att.Verdict != VerdictUnreachable {
			allUnreachable = false
		}
		switch r.att.Verdict {
		case VerdictResolved:
			if firstResolved < 0 {
				firstResolved = i
				rep.Models = r.models
			}
		case VerdictAuthFailed:
			authIdx = append(authIdx, i)
		}
	}
	switch {
	case firstResolved >= 0:
		rep.OK = true
		rep.Verdict = VerdictResolved
		rep.ResolvedBaseURL = roots[firstResolved]
	case len(authIdx) == 1:
		rep.Verdict = VerdictAuthFailed
		rep.ResolvedBaseURL = roots[authIdx[0]]
	case len(authIdx) > 1:
		// 上游可达，但鉴权墙挡住探测（对不存在路径也回 401），无法区分路径；
		// 不采纳任何候选，保留规范化输入。
		rep.Verdict = "auth_gated"
	case allUnreachable:
		rep.Verdict = VerdictUnreachable
	default:
		rep.Verdict = "no_match"
	}
	return rep
}

// probeHeaders 探测请求的协议鉴权头（与 relay/forward.go endpoint 的转发头同源）。
func probeHeaders(protocol, apiKey string) map[string]string {
	switch protocol {
	case "anthropic":
		return map[string]string{"x-api-key": apiKey, "anthropic-version": "2023-06-01"}
	case "gemini":
		return map[string]string{"x-goog-api-key": apiKey}
	default: // openai-chat / openai-responses
		return map[string]string{"Authorization": "Bearer " + apiKey}
	}
}

// parseModelList 识别模型列表 JSON（openai/anthropic 的 data[] 或 gemini 的
// models[]，键存在即认为路径正确，即便数组为空）并提取模型 id。
func parseModelList(body []byte) ([]string, bool) {
	var parsed struct {
		Data   []map[string]any `json:"data"`
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, false
	}
	if parsed.Data == nil && parsed.Models == nil {
		return nil, false
	}
	var ids []string
	collect := func(items []map[string]any, keys ...string) {
		for _, it := range items {
			for _, k := range keys {
				if s, ok := it[k].(string); ok && s != "" {
					ids = append(ids, strings.TrimPrefix(s, "models/")) // gemini name 形态 models/xxx
					break
				}
			}
			if len(ids) >= probeModelsCap {
				return
			}
		}
	}
	collect(parsed.Data, "id")
	if len(ids) < probeModelsCap {
		collect(parsed.Models, "name", "id")
	}
	if len(ids) > probeModelsCap {
		ids = ids[:probeModelsCap]
	}
	return ids, true
}

// lastSeg 取路径末段（空路径返回空串）。
func lastSeg(path string) string {
	if path == "" {
		return ""
	}
	if i := strings.LastIndex(path, "/"); i >= 0 {
		return path[i+1:]
	}
	return path
}

// isVersionSeg 识别版本段：v + 数字开头 + 字母数字尾（v1 / v1beta / v1beta1 / v2）。
// 首字符后必须是数字，避免误伤 voice、vendor 等普通路径段。
func isVersionSeg(seg string) bool {
	if len(seg) < 2 || seg[0] != 'v' || seg[1] < '0' || seg[1] > '9' {
		return false
	}
	for i := 2; i < len(seg); i++ {
		c := seg[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'z') {
			return false
		}
	}
	return true
}

// dedupe 保序去重。
func dedupe(items []string) []string {
	seen := make(map[string]bool, len(items))
	out := items[:0]
	for _, it := range items {
		if !seen[it] {
			seen[it] = true
			out = append(out, it)
		}
	}
	return out
}

// localHTTPTwins 本地裸域名输入的 http 优先变体：每个 https 候选配一个 http
// 孪生，http 排前（本地运行时几乎不开 TLS），https 留作兜底，总量封顶 4。
func localHTTPTwins(roots []string) []string {
	var https, httpsAPI []string
	for _, r := range roots {
		if strings.HasSuffix(r, "/api") {
			httpsAPI = append(httpsAPI, r)
		} else {
			https = append(https, r)
		}
	}
	var out []string
	for _, group := range [][]string{https, httpsAPI} {
		for _, r := range group {
			out = append(out, strings.Replace(r, "https://", "http://", 1))
		}
	}
	for _, group := range [][]string{https, httpsAPI} {
		for _, r := range group {
			out = append(out, r)
		}
	}
	if len(out) > 4 {
		out = out[:4]
	}
	return dedupe(out)
}

// isLocalHost 判断规范化 URL 的主机是否为本地/内网地址（回环、RFC1918 私网、
// .localhost/.local 后缀）。
func isLocalHost(normalized string) bool {
	u, err := url.Parse(normalized)
	if err != nil || u.Host == "" {
		return false
	}
	host := u.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsLoopback() || ip.IsPrivate())
}
