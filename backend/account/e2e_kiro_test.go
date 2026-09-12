// e2e_kiro_test.go kiro 账号端到端测试（任务组 7.7）。
// 外部测试包（account_test）：导入 server 走完整客户端入口；
// host/refresh URL 模板经 export_test.go 暴露的指针指向 mock。
// mock 上游覆盖：refreshToken / ListAvailableModels / GetUsageLimits /
// generateAssistantResponse（真 AWS eventstream 二进制帧包裹 JSON 载荷）。
package account_test

import (
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"relayd/backend/account"
	"relayd/backend/codec/kiro"
	"relayd/backend/config"
	"relayd/backend/ir"
	"relayd/backend/server"
)

// kiroMockState mock 各端点的调用计数。
type kiroMockState struct {
	chat, refresh, models, usage, mcp atomic.Int32
	// mcpHandler 自定义 /mcp 响应（nil 时返回默认搜索结果）。
	mcpHandler func(w http.ResponseWriter, r *http.Request)
}

// mcpSearchResponse /mcp 默认响应（CallWebSearch 二层 JSON：外层
// JSON-RPC result.content[0].text，内层 {"results":[...]}）。
func mcpSearchResponse() []byte {
	return []byte(`{"result":{"content":[{"type":"text","text":"{\"results\":[{\"title\":\"Go\",\"url\":\"https://go.dev\",\"snippet\":\"fast\"}]}"}]}}`)
}

// kiroFrame 把 JSON 载荷包成 AWS eventstream 二进制帧
// （头长 0、CRC 填 0——网关解析器不读帧头不校验 CRC，按文本扫描载荷）。
func kiroFrame(payload []byte) []byte {
	total := uint32(16 + len(payload)) // prelude 12 + payload + crc 4
	f := make([]byte, total)
	binary.BigEndian.PutUint32(f[0:4], total)
	binary.BigEndian.PutUint32(f[4:8], 0)  // headers_len
	binary.BigEndian.PutUint32(f[8:12], 0) // prelude_crc
	copy(f[12:], payload)
	return f
}

// kiroChatStream 构造成功的聊天响应：text marker + context_usage 完成信号。
func kiroChatStream(marker string) []byte {
	body, err := (kiro.Codec{}).EncodeResponse(&ir.Response{
		Model:      "claude-sonnet-4-5",
		Content:    []ir.Block{{Type: ir.BlockText, Text: marker}},
		StopReason: ir.StopEndTurn,
	})
	if err != nil {
		panic(err)
	}
	var out []byte
	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		out = append(out, kiroFrame([]byte(line))...)
	}
	return out
}

// anthropicSSE api-key 兜底账号的 anthropic mock 流。
func anthropicSSE(marker string) string {
	return "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"m1","model":"native-model","usage":{"input_tokens":3,"output_tokens":1}}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"` + marker + `"}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"
}

// newKiroMock 起 mock Kiro 控制面/数据面服务器并接管 host/refresh 模板
// （测试毕恢复）。chat 为聊天端点处理器。
func newKiroMock(t *testing.T, chat func(w http.ResponseWriter, r *http.Request)) *kiroMockState {
	t.Helper()
	st := &kiroMockState{}
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/refreshToken"):
			st.refresh.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"accessToken":"at-` + fmt.Sprint(st.refresh.Load()) +
				`","refreshToken":"rt-rotated","expiresIn":3600,"profileArn":"arn:profile/p1"}`))
		case strings.HasSuffix(r.URL.Path, "/ListAvailableModels"):
			st.models.Add(1)
			_, _ = w.Write([]byte(`{"models":[{"modelId":"claude-sonnet-4-5","modelName":"Claude Sonnet 4.5","tokenLimits":{"maxInputTokens":200000}}]}`))
		case strings.HasSuffix(r.URL.Path, "/GetUsageLimits"):
			st.usage.Add(1)
			_, _ = w.Write([]byte(`{"usageBreakdownList":[{"resetDate":"2099-01-01"}]}`))
		case strings.HasSuffix(r.URL.Path, "/generateAssistantResponse"):
			st.chat.Add(1)
			chat(w, r)
		case strings.HasSuffix(r.URL.Path, "/mcp"):
			st.mcp.Add(1)
			if st.mcpHandler != nil {
				st.mcpHandler(w, r)
				return
			}
			_, _ = w.Write(mcpSearchResponse())
		default: // ListAvailableProfiles（POST .../）等控制面兜底
			_, _ = w.Write([]byte(`{"profiles":[{"arn":"arn:profile/p1"}]}`))
		}
	}))
	t.Cleanup(mock.Close)

	// 模板指向 mock（测试毕恢复）
	oldRt, oldQ, oldRefresh := *account.RuntimeHostTemplate, *account.QHostTemplate, *account.KiroRefreshURLTemplate
	*account.RuntimeHostTemplate = mock.URL + "/runtime-%s"
	*account.QHostTemplate = mock.URL + "/q-%s"
	*account.KiroRefreshURLTemplate = mock.URL + "/sso-%s/refreshToken"
	t.Cleanup(func() {
		*account.RuntimeHostTemplate, *account.QHostTemplate, *account.KiroRefreshURLTemplate = oldRt, oldQ, oldRefresh
	})
	return st
}

// newKiroEnv 起 mock Kiro 服务 + 单 kiro 账号网关。
// extraSeeds 追加账号（如 anthropic 兜底），插库顺序在 kiro 账号之后
// （rowid 调度序：kiro 优先）。
func newKiroEnv(t *testing.T, chat func(w http.ResponseWriter, r *http.Request), extraSeeds ...account.SeedUpstream) (*httptest.Server, *account.Manager, *kiroMockState) {
	t.Helper()
	return newKiroEnvWithConfig(t, &config.Config{Scheduler: &config.Scheduler{SameAccountRetries: 2}}, chat, extraSeeds...)
}

// newKiroEnvWithConfig 同 newKiroEnv 但允许自定义顶层配置
// （截断恢复开关等；测试直接构造 Config，不走 Load 默认值）。
func newKiroEnvWithConfig(t *testing.T, cfg *config.Config, chat func(w http.ResponseWriter, r *http.Request), extraSeeds ...account.SeedUpstream) (*httptest.Server, *account.Manager, *kiroMockState) {
	t.Helper()
	st := newKiroMock(t, chat)
	store, err := account.Open(filepath.Join(t.TempDir(), "kiro.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.InsertAccount(&account.Account{
		Name: "k1-kiro", Type: account.TypeKiro, Enabled: true,
		Kiro: &account.KiroAccount{Source: account.SourceRefreshToken, RefreshToken: "rt-1"},
	}); err != nil {
		t.Fatal(err)
	}
	m, err := account.NewManager(store, extraSeeds, nil, account.Cooldowns{}, account.ManagerDeps{})
	if err != nil {
		t.Fatal(err)
	}
	m.SetProbeRateForTest(0) // 消除熔断试探随机性

	if cfg.Scheduler == nil {
		cfg.Scheduler = &config.Scheduler{SameAccountRetries: 2}
	}
	gw := httptest.NewServer(server.New(cfg, m).Handler())
	t.Cleanup(gw.Close)
	return gw, m, st
}

// kiroAccount 状态快照里取 kiro 账号。
func kiroAccount(m *account.Manager) account.Account {
	for _, a := range m.Status() {
		if a.Name == "k1-kiro" {
			return a
		}
	}
	panic("k1-kiro not found")
}

// clientBody 四协议客户端请求（路径 + 体）。
func clientBody(protocol string, stream bool) (path, body string) {
	switch protocol {
	case "anthropic":
		return "/v1/messages", fmt.Sprintf(`{"model":"claude-sonnet-4-5","max_tokens":100,"stream":%v,"messages":[{"role":"user","content":"hi"}]}`, stream)
	case "openai-chat":
		return "/v1/chat/completions", fmt.Sprintf(`{"model":"claude-sonnet-4-5","stream":%v,"messages":[{"role":"user","content":"hi"}]}`, stream)
	case "openai-responses":
		return "/v1/responses", fmt.Sprintf(`{"model":"claude-sonnet-4-5","stream":%v,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`, stream)
	default: // gemini
		action := "generateContent"
		if stream {
			action = "streamGenerateContent"
		}
		return "/v1beta/models/claude-sonnet-4-5:" + action, `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`
	}
}

// streamDone 各协议流式响应的终止标志。
func streamDone(protocol string) string {
	switch protocol {
	case "anthropic":
		return "message_stop"
	case "openai-chat":
		return "[DONE]"
	case "openai-responses":
		return "response.completed"
	default:
		return `"finishReason"`
	}
}

func postChat(t *testing.T, gwURL, path, body string) (int, string) {
	t.Helper()
	resp, err := http.Post(gwURL+path, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

const anthropicChat = `{"model":"claude-sonnet-4-5","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`

// 四协议入口 x kiro 上游（流式/非流式）全矩阵：标记文本经 IR 枢纽正确回传。
func TestE2EKiroFourProtocols(t *testing.T) {
	for _, client := range []string{"anthropic", "openai-chat", "openai-responses", "gemini"} {
		client := client
		for _, stream := range []bool{false, true} {
			stream := stream
			name := client + "_to_kiro"
			if stream {
				name += "_stream"
			}
			t.Run(name, func(t *testing.T) {
				gw, _, st := newKiroEnv(t, func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
					w.WriteHeader(200)
					_, _ = w.Write(kiroChatStream("KIRO_E2E_MARKER"))
				})
				path, body := clientBody(client, stream)
				status, respBody := postChat(t, gw.URL, path, body)
				if status != 200 {
					t.Fatalf("status = %d, body = %s", status, respBody)
				}
				if !strings.Contains(respBody, "KIRO_E2E_MARKER") {
					t.Errorf("marker missing from %s response: %s", client, respBody)
				}
				if stream && !strings.Contains(respBody, streamDone(client)) {
					t.Errorf("stream terminator missing from %s response: %s", client, respBody)
				}
				if got := st.chat.Load(); got != 1 {
					t.Errorf("kiro chat called %d times, want 1", got)
				}
			})
		}
	}
}

// 402 配额超限：GetUsageLimits 重置日期（2099）冷却 + 切号到兜底账号。
func TestE2EKiroQuotaCooldown(t *testing.T) {
	fb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(anthropicSSE("FROM_FALLBACK")))
	}))
	t.Cleanup(fb.Close)

	gw, m, st := newKiroEnv(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(402)
		_, _ = w.Write([]byte(`{"message":"monthly request limit exceeded","reason":"MONTHLY_REQUEST_COUNT"}`))
	}, account.SeedUpstream{Name: "z-fallback", Protocol: "anthropic", BaseURL: fb.URL, APIKey: "k",
		Models: map[string]string{"claude-sonnet-4-5": "native-model"}})

	status, body := postChat(t, gw.URL, "/v1/messages", anthropicChat)
	if status != 200 || !strings.Contains(body, "FROM_FALLBACK") {
		t.Fatalf("status = %d, body = %s", status, body)
	}
	if got := st.usage.Load(); got != 1 {
		t.Errorf("GetUsageLimits called %d times, want 1", got)
	}
	if a := kiroAccount(m); a.CooldownUntil.Year() != 2099 {
		t.Errorf("kiro cooldown until = %v, want 2099 (reset date)", a.CooldownUntil)
	}
	// 冷却中：后续请求直达兜底，不再打 kiro
	if _, body := postChat(t, gw.URL, "/v1/messages", anthropicChat); !strings.Contains(body, "FROM_FALLBACK") {
		t.Errorf("second request body = %s", body)
	}
	if got := st.chat.Load(); got != 1 {
		t.Errorf("cooling kiro account called again (%d times)", got)
	}
}

// 400 INVALID_MODEL_ID：换号但不惩罚（不禁用、不熔断、无冷却）——
// 第二个请求仍先试 kiro（订阅差异，换号可能解决）。
func TestE2EKiroInvalidModelSwitchesWithoutPenalty(t *testing.T) {
	fb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(anthropicSSE("FROM_FALLBACK")))
	}))
	t.Cleanup(fb.Close)

	gw, m, st := newKiroEnv(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"message":"invalid model","reason":"INVALID_MODEL_ID"}`))
	}, account.SeedUpstream{Name: "z-fallback", Protocol: "anthropic", BaseURL: fb.URL, APIKey: "k",
		Models: map[string]string{"claude-sonnet-4-5": "native-model"}})

	for i := 0; i < 2; i++ {
		status, respBody := postChat(t, gw.URL, "/v1/messages", anthropicChat)
		if status != 200 || !strings.Contains(respBody, "FROM_FALLBACK") {
			t.Fatalf("request %d: status = %d, body = %s", i+1, status, respBody)
		}
	}
	if got := st.chat.Load(); got != 2 {
		t.Errorf("kiro chat called %d times, want 2 (no penalty: retried first each request)", got)
	}
	if a := kiroAccount(m); a.Disabled || a.Failures != 0 || a.CooldownUntil.After(time.Now()) {
		t.Errorf("kiro account penalized: %+v", a)
	}
}

// 403：强刷 token 原地重试 1 次；重试成功则不禁用、不切号。
func TestE2EKiro403ForceRefreshRetry(t *testing.T) {
	gw, m, st := newKiroEnv(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer at-2" { // 首个 token（at-1）403
			w.WriteHeader(403)
			_, _ = w.Write([]byte(`{"message":"token expired"}`))
			return
		}
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		w.WriteHeader(200)
		_, _ = w.Write(kiroChatStream("AFTER_REFRESH"))
	})

	status, body := postChat(t, gw.URL, "/v1/messages", anthropicChat)
	if status != 200 || !strings.Contains(body, "AFTER_REFRESH") {
		t.Fatalf("status = %d, body = %s", status, body)
	}
	if got := st.chat.Load(); got != 2 {
		t.Errorf("kiro chat called %d times, want 2 (403 then refreshed retry)", got)
	}
	if got := st.refresh.Load(); got < 2 {
		t.Errorf("refresh called %d times, want >= 2 (initial + force)", got)
	}
	if a := kiroAccount(m); a.Disabled {
		t.Error("kiro account should stay enabled after successful force refresh")
	}
}

// 混合池（kiro + api-key）瞬时错误：原地重试耗尽 → 熔断切号；
// 熔断冷却期内后续请求直达兜底账号。
func TestE2EKiroTransientBreakerMixedPool(t *testing.T) {
	fb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(anthropicSSE("FROM_FALLBACK")))
	}))
	t.Cleanup(fb.Close)

	gw, m, st := newKiroEnv(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`{"message":"boom"}`))
	}, account.SeedUpstream{Name: "z-fallback", Protocol: "anthropic", BaseURL: fb.URL, APIKey: "k",
		Models: map[string]string{"claude-sonnet-4-5": "native-model"}})

	status, respBody := postChat(t, gw.URL, "/v1/messages", anthropicChat)
	if status != 200 || !strings.Contains(respBody, "FROM_FALLBACK") {
		t.Fatalf("status = %d, body = %s", status, respBody)
	}
	// same_account_retries=2：原地 3 次（try 0/1/2）后熔断切号
	if got := st.chat.Load(); got != 3 {
		t.Errorf("kiro chat called %d times, want 3 (in-place retries exhausted)", got)
	}
	if a := kiroAccount(m); a.Failures != 1 || !a.CooldownUntil.After(time.Now()) {
		t.Errorf("breaker state = failures %d, cooldown %v", a.Failures, a.CooldownUntil)
	}
	// 冷却期内：直达兜底
	if _, respBody := postChat(t, gw.URL, "/v1/messages", anthropicChat); !strings.Contains(respBody, "FROM_FALLBACK") {
		t.Errorf("second request body = %s", respBody)
	}
	if got := st.chat.Load(); got != 3 {
		t.Errorf("cooling kiro account called again (%d times)", got)
	}
}
