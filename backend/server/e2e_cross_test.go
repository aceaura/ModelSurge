package server_test

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"relayd/backend/config"
	"relayd/backend/server"
)

// 4 客户端协议 x 4 上游协议 x 流式/非流式 的端到端矩阵测试。
// mock 上游返回带标记文本的 SSE 流；断言：
//  1. 客户端响应包含标记文本（协议转换正确）；
//  2. 流式响应带本协议终止标志；
//  3. 上游收到的请求使用了映射后的 native model；
//  4. 上游请求被强制为流式（永远流式原则）。

var protocols = []string{"anthropic", "openai-chat", "openai-responses", "gemini"}

type recorded struct {
	mu   sync.Mutex
	body string
	path string
}

func (r *recorded) set(body, path string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.body, r.path = body, path
}

// mockUpstream 启动一个指定协议的 mock 上游，永远以 SSE 流式响应。
func mockUpstream(t *testing.T, protocol, marker string) (*httptest.Server, *recorded) {
	t.Helper()
	rec := &recorded{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rec.set(string(body), r.URL.Path)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(upstreamStream(protocol, marker)))
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

// upstreamStream 各协议的流式响应 SSE 载荷。
func upstreamStream(protocol, marker string) string {
	switch protocol {
	case "anthropic":
		return `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"native-model","content":[],"stop_reason":null,"usage":{"input_tokens":10,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"` + marker + `"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":5}}

event: message_stop
data: {"type":"message_stop"}

`
	case "openai-chat":
		return `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"native-model","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}

data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"native-model","choices":[{"index":0,"delta":{"content":"` + marker + `"},"finish_reason":null}]}

data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"native-model","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"native-model","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}

data: [DONE]

`
	case "openai-responses":
		return `event: response.created
data: {"type":"response.created","response":{"id":"resp_1","object":"response","model":"native-model","status":"in_progress","output":[]}}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"id":"msg_1","type":"message","status":"in_progress","role":"assistant","content":[]}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"` + marker + `"}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","object":"response","model":"native-model","status":"completed","output":[{"id":"msg_1","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"` + marker + `","annotations":[]}]}],"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}}

`
	case "gemini":
		return `data: {"candidates":[{"content":{"role":"model","parts":[{"text":"` + marker + `"}]},"index":0}],"modelVersion":"native-model","responseId":"r1"}

data: {"candidates":[{"content":{"role":"model","parts":[]},"finishReason":"STOP","index":0}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"totalTokenCount":15}}

`
	}
	return ""
}

// clientRequest 各协议客户端的请求路径与 body。
func clientRequest(protocol string, stream bool) (path, body string) {
	switch protocol {
	case "anthropic":
		return "/v1/messages", fmt.Sprintf(`{"model":"test-model","max_tokens":100,"stream":%v,"messages":[{"role":"user","content":"hi"}]}`, stream)
	case "openai-chat":
		return "/v1/chat/completions", fmt.Sprintf(`{"model":"test-model","stream":%v,"messages":[{"role":"user","content":"hi"}]}`, stream)
	case "openai-responses":
		return "/v1/responses", fmt.Sprintf(`{"model":"test-model","stream":%v,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`, stream)
	case "gemini":
		action := "generateContent"
		if stream {
			action = "streamGenerateContent"
		}
		return "/v1beta/models/test-model:" + action, `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`
	}
	return "", ""
}

// streamTerminator 各协议流式响应的终止标志。
func streamTerminator(protocol string) string {
	switch protocol {
	case "anthropic":
		return "message_stop"
	case "openai-chat":
		return "[DONE]"
	case "openai-responses":
		return "response.completed"
	case "gemini":
		return `"finishReason"`
	}
	return ""
}

func TestCrossProtocolMatrix(t *testing.T) {
	for _, up := range protocols {
		up := up
		for _, client := range protocols {
			client := client
			for _, stream := range []bool{false, true} {
				stream := stream
				name := fmt.Sprintf("%s_to_%s", client, up)
				if stream {
					name += "_stream"
				}
				t.Run(name, func(t *testing.T) {
					t.Parallel()
					marker := "HELLO_FROM_" + strings.ToUpper(strings.ReplaceAll(up, "-", "_"))
					upSrv, rec := mockUpstream(t, up, marker)
					s := server.New(&config.Config{Upstreams: []config.Upstream{{
						Name:     "mock",
						Protocol: up,
						BaseURL:  upSrv.URL,
						APIKey:   "sk-mock",
						Models:   map[string]string{"test-model": "native-model"},
					}}}, nil)
					gw := httptest.NewServer(s.Handler())
					defer gw.Close()

					path, body := clientRequest(client, stream)
					resp, err := http.Post(gw.URL+path, "application/json", strings.NewReader(body))
					if err != nil {
						t.Fatalf("client request: %v", err)
					}
					defer resp.Body.Close()
					respBody, _ := io.ReadAll(resp.Body)

					if resp.StatusCode != 200 {
						t.Fatalf("status = %d, body = %s", resp.StatusCode, respBody)
					}
					if !strings.Contains(string(respBody), marker) {
						t.Errorf("response missing marker %q:\n%s", marker, respBody)
					}
					if stream && !strings.Contains(string(respBody), streamTerminator(client)) {
						t.Errorf("stream response missing terminator %q:\n%s", streamTerminator(client), respBody)
					}
					if !stream {
						if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
							t.Errorf("non-stream Content-Type = %q, want application/json", ct)
						}
					}

					// 模型映射生效
					if !strings.Contains(rec.body, "native-model") && !strings.Contains(rec.path, "native-model") {
						t.Errorf("upstream did not receive native model name: path=%s body=%s", rec.path, rec.body)
					}
					// 上游永远流式
					switch up {
					case "gemini":
						if !strings.Contains(rec.path, "streamGenerateContent") {
							t.Errorf("gemini upstream path = %s, want streamGenerateContent", rec.path)
						}
					default:
						if !strings.Contains(rec.body, `"stream":true`) {
							t.Errorf("upstream request not forced to stream: %s", rec.body)
						}
					}
				})
			}
		}
	}
}
