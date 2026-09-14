// probe_test.go 端点探测适配的单元测试：NormalizeBaseURL / CandidateRoots
// 表驱动覆盖 + ProbeEndpoint 的 httptest 判定矩阵。
package account

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestNormalizeBaseURL(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		// 裸域名：补 https
		{"api.anthropic.com", "https://api.anthropic.com", false},
		{"  api.openai.com  ", "https://api.openai.com", false},
		{"localhost:8080", "https://localhost:8080", false},
		{"192.168.1.5:9000/v1", "https://192.168.1.5:9000", false},
		// 显式 scheme 保留
		{"http://localhost:11434", "http://localhost:11434", false},
		{"HTTPS://Api.Example.COM", "https://api.example.com", false},
		// 版本段剥离（不双拼）
		{"https://host/v1", "https://host", false},
		{"https://host/v1beta", "https://host", false},
		{"https://host/api/v1", "https://host/api", false},
		{"https://host/v1alpha1/", "https://host", false},
		// 端点尾段剥离（粘贴完整端点 URL）
		{"https://host/v1/messages", "https://host", false},
		{"https://host/v1/chat/completions", "https://host", false},
		{"https://host/v1/responses", "https://host", false},
		{"https://host/v1/models", "https://host", false},
		{"https://host/v1/messages/count_tokens", "https://host", false},
		{"https://host/v1beta/models/gemini-2.0:streamGenerateContent?alt=sse", "https://host", false},
		// 带外部分剥离与杂项
		{"https://host/v1/?key=abc#frag", "https://host", false},
		{"https://user:pw@host/v1", "https://host", false},
		{"https://host//api//v1//", "https://host/api", false},
		// 不误伤普通路径段
		{"https://host/voice", "https://host/voice", false},
		{"https://host/messages-proxy/v1", "https://host/messages-proxy", false},
		{"https://host/api", "https://host/api", false},
		// 错误输入
		{"", "", true},
		{"   ", "", true},
		{"https://", "", true},
		{"ftp://host", "", true},
		{"ht tp://host", "", true},
	}
	for _, c := range cases {
		got, err := NormalizeBaseURL(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("NormalizeBaseURL(%q) err = nil, want error", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("NormalizeBaseURL(%q) err = %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("NormalizeBaseURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestCandidateRoots(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		// 裸域名（无路径）：追加 /api 候选
		{"https://host", []string{"https://host", "https://host/api"}},
		// 带非 api 路径：追加 /api，并兜底剥路径的裸 host 两个候选
		{"https://host/llm", []string{"https://host/llm", "https://host/llm/api", "https://host", "https://host/api"}},
		// 末段已是 api：不重复追加；裸 host + /api 与候选 1 去重
		{"https://host/api", []string{"https://host/api", "https://host"}},
		// 版本段已被规范化剥掉，此处不出现
		{"https://host", []string{"https://host", "https://host/api"}},
	}
	for _, c := range cases {
		got := CandidateRoots(c.in)
		if len(got) != len(c.want) {
			t.Fatalf("CandidateRoots(%q) = %v, want %v", c.in, got, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("CandidateRoots(%q)[%d] = %q, want %q", c.in, i, got[i], c.want[i])
			}
		}
	}
}

// probeSrv 构造一个按 pathPattern 应答的 mock 上游：命中返回模型列表 JSON，
// 未命中返回 404。
func probeSrv(t *testing.T, hits *atomic.Int64, hitPath string, status int, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == hitPath {
			if hits != nil {
				hits.Add(1)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
			return
		}
		w.WriteHeader(404)
	}))
}

func TestProbeEndpointResolved(t *testing.T) {
	var hits atomic.Int64
	up := probeSrv(t, &hits, "/v1/models", 200,
		`{"object":"list","data":[{"id":"gpt-x"},{"id":"gpt-y"}]}`)
	defer up.Close()

	rep := ProbeEndpoint(context.Background(), up.Client(), "openai-chat", up.URL, "sk-test")
	if !rep.OK || rep.Verdict != VerdictResolved {
		t.Fatalf("verdict = %s ok = %v, want resolved", rep.Verdict, rep.OK)
	}
	if rep.ResolvedBaseURL != up.URL {
		t.Errorf("resolved = %q, want %q", rep.ResolvedBaseURL, up.URL)
	}
	if len(rep.Models) != 2 || rep.Models[0] != "gpt-x" {
		t.Errorf("models = %v, want [gpt-x gpt-y]", rep.Models)
	}
	if hits.Load() != 1 {
		t.Errorf("hits = %d, want 1", hits.Load())
	}
}

func TestProbeEndpointAPIPrefix(t *testing.T) {
	up := probeSrv(t, nil, "/api/v1/models", 200, `{"data":[{"id":"claude-x"}]}`)
	defer up.Close()

	rep := ProbeEndpoint(context.Background(), up.Client(), "anthropic", up.URL, "k")
	if !rep.OK || rep.ResolvedBaseURL != up.URL+"/api" {
		t.Fatalf("verdict = %s resolved = %q, want %s/api", rep.Verdict, rep.ResolvedBaseURL, up.URL)
	}
	// 胜出的是第 2 候选：attempts 应包含两个候选的证据
	if len(rep.Attempts) != 2 || rep.Attempts[0].Verdict != VerdictNotFound || rep.Attempts[1].Verdict != VerdictResolved {
		t.Errorf("attempts = %+v", rep.Attempts)
	}
}

func TestProbeEndpointDoesNotGenerateGeminiEndpointOrHeader(t *testing.T) {
	var sawV1Beta atomic.Bool
	var sawGoogleKey atomic.Bool
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "v1beta") {
			sawV1Beta.Store(true)
		}
		if r.Header.Get("x-goog-api-key") != "" {
			sawGoogleKey.Store(true)
		}
		if r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"legacy"}]}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer up.Close()

	rep := ProbeEndpoint(context.Background(), up.Client(), "gemini", up.URL, "gk")
	if !rep.OK || rep.ResolvedBaseURL != up.URL {
		t.Fatalf("verdict = %s resolved = %q", rep.Verdict, rep.ResolvedBaseURL)
	}
	if sawV1Beta.Load() || sawGoogleKey.Load() {
		t.Fatalf("generated Gemini probe behavior: v1beta=%v x-goog-api-key=%v", sawV1Beta.Load(), sawGoogleKey.Load())
	}
}

func TestProbeEndpointAuthFailedAdopted(t *testing.T) {
	// 恰一候选 401：路径存在、密钥无效 -> 采纳该根
	up := probeSrv(t, nil, "/v1/models", 401, `{"error":"bad key"}`)
	defer up.Close()

	rep := ProbeEndpoint(context.Background(), up.Client(), "openai-chat", up.URL, "bad")
	if rep.OK || rep.Verdict != VerdictAuthFailed {
		t.Fatalf("verdict = %s ok = %v, want auth_failed", rep.Verdict, rep.OK)
	}
	if rep.ResolvedBaseURL != up.URL {
		t.Errorf("resolved = %q, want %q", rep.ResolvedBaseURL, up.URL)
	}
}

func TestProbeEndpointNoMatch(t *testing.T) {
	// 双候选均 401（上游可达但鉴权墙挡住探测，无法区分路径）-> auth_gated
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(401)
	}))
	defer up.Close()

	rep := ProbeEndpoint(context.Background(), up.Client(), "openai-chat", up.URL, "bad")
	if rep.OK || rep.Verdict != "auth_gated" || rep.ResolvedBaseURL != "" {
		t.Fatalf("verdict = %s ok = %v resolved = %q, want auth_gated", rep.Verdict, rep.OK, rep.ResolvedBaseURL)
	}

	// 全部 404 -> no_match
	up404 := probeSrv(t, nil, "/none", 404, `{}`)
	defer up404.Close()
	rep = ProbeEndpoint(context.Background(), up404.Client(), "openai-chat", up404.URL, "k")
	if rep.Verdict != "no_match" {
		t.Fatalf("verdict = %s, want no_match", rep.Verdict)
	}
}

func TestProbeEndpointUnreachable(t *testing.T) {
	// 关停的服务器：网络层错误 -> unreachable
	up := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	up.Close()

	rep := ProbeEndpoint(context.Background(), http.DefaultClient, "openai-chat", up.URL, "k")
	if rep.OK || rep.Verdict != VerdictUnreachable {
		t.Fatalf("verdict = %s, want unreachable", rep.Verdict)
	}
	for _, att := range rep.Attempts {
		if att.Status != 0 || att.Verdict != VerdictUnreachable {
			t.Errorf("attempt %+v, want status 0 unreachable", att)
		}
	}
}

// TestProbeEndpointLocalBareHost 本地裸 IP:port 输入（无 scheme）：自动加测
// http 变体（http 优先），探到后根地址以 http 形态写回。
func TestProbeEndpointLocalBareHost(t *testing.T) {
	up := probeSrv(t, nil, "/v1/models", 200, `{"data":[{"id":"local-m"}]}`)
	defer up.Close()

	bare := strings.TrimPrefix(up.URL, "http://") // 127.0.0.1:port 形态
	rep := ProbeEndpoint(context.Background(), up.Client(), "openai-chat", bare, "k")
	if !rep.OK || rep.Verdict != VerdictResolved || rep.ResolvedBaseURL != up.URL {
		t.Fatalf("verdict = %s ok = %v resolved = %q, want http resolved %s", rep.Verdict, rep.OK, rep.ResolvedBaseURL, up.URL)
	}
}

func TestProbeEndpointUnexpectedAndPriority(t *testing.T) {
	// 两个候选都 200：按候选序取第一个；200 但非模型列表 JSON 记 unexpected
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(200)
		_, _ = w.Write([]byte("<html>pong</html>"))
	}))
	defer up.Close()

	rep := ProbeEndpoint(context.Background(), up.Client(), "openai-chat", up.URL, "k")
	if rep.OK || rep.Verdict != "no_match" {
		t.Fatalf("verdict = %s ok = %v, want no_match (200 非 JSON)", rep.Verdict, rep.OK)
	}
	for _, att := range rep.Attempts {
		if att.Status != 200 || att.Verdict != VerdictUnexpected {
			t.Errorf("attempt %+v, want status 200 unexpected", att)
		}
	}
}

func TestProbeEndpointTimeout(t *testing.T) {
	// 探测总超时的表现形态即传输层失败（probeTimeout 到期 ctx 取消 -> client.Do
	// 报错）：用 100ms 预超时 ctx 等价驱动，避免测试真等 5s。
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(3 * time.Second)
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer up.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	rep := ProbeEndpoint(ctx, up.Client(), "openai-chat", up.URL, "k")
	if rep.OK || rep.Verdict != VerdictUnreachable {
		t.Fatalf("verdict = %s ok = %v, want unreachable", rep.Verdict, rep.OK)
	}
}

func TestParseModelListCapsAndGeminiPrefix(t *testing.T) {
	// 封顶
	var sb strings.Builder
	sb.WriteString(`{"data":[`)
	for i := 0; i < 150; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(`{"id":"m-` + strings.Repeat("x", 1) + `"}`)
	}
	sb.WriteString(`]}`)
	ids, ok := parseModelList([]byte(sb.String()))
	if !ok || len(ids) != probeModelsCap {
		t.Errorf("ids = %d ok = %v, want cap %d", len(ids), ok, probeModelsCap)
	}
	// gemini models/name 前缀剥离
	ids, ok = parseModelList([]byte(`{"models":[{"name":"models/gemini-1"},{"name":"models/gemini-2"}]}`))
	if !ok || len(ids) != 2 || ids[0] != "gemini-1" {
		t.Errorf("ids = %v ok = %v", ids, ok)
	}
	// data/models 键都缺失 -> false
	if _, ok := parseModelList([]byte(`{"foo":1}`)); ok {
		t.Error("want false for non-model JSON")
	}
	if _, ok := parseModelList([]byte(`not json`)); ok {
		t.Error("want false for invalid JSON")
	}
}
