package server_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"relayd/backend/config"
)

// 工具调用的跨协议转换：Anthropic 客户端（带 tools） -> OpenAI Chat 上游
// 流式返回 tool_calls。断言：
//  1. 上游收到 OpenAI 形态的 tools（type:function 包裹）；
//  2. Anthropic 流式客户端收到 content_block_start{tool_use} + input_json_delta，stop_reason=tool_use；
//  3. Anthropic 非流式客户端收到聚合的 tool_use block，input 为解析后的 JSON 对象。
func TestToolCall_AnthropicClient_OpenAIUpstream(t *testing.T) {
	upStream := `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"native-model","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_abc","type":"function","function":{"name":"get_weather","arguments":""}}]},"finish_reason":null}]}

data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"native-model","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":"}}]},"finish_reason":null}]}

data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"native-model","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"Paris\"}"}}]},"finish_reason":null}]}

data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"native-model","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}

data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"native-model","choices":[],"usage":{"prompt_tokens":20,"completion_tokens":8,"total_tokens":28}}

data: [DONE]

`
	rec := &recorded{}
	upSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rec.set(string(body), r.URL.Path)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(upStream))
	}))
	defer upSrv.Close()

	gw := newGateway(t, &config.Config{},
		apiKeyAcc("mock", "openai-chat", upSrv.URL, "test-model", "native-model"))
	defer gw.Close()

	reqBody := `{"model":"test-model","max_tokens":100,"stream":%v,"tools":[{"name":"get_weather","description":"查询天气","input_schema":{"type":"object","properties":{"city":{"type":"string"}}}}],"messages":[{"role":"user","content":"巴黎天气如何"}]}`

	t.Run("stream", func(t *testing.T) {
		resp, err := http.Post(gw.URL+"/v1/messages", "application/json", strings.NewReader(strings.Replace(reqBody, "%v", "true", 1)))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		out := string(body)
		for _, want := range []string{`"type":"tool_use"`, `"name":"get_weather"`, "input_json_delta", `"stop_reason":"tool_use"`, "message_stop"} {
			if !strings.Contains(out, want) {
				t.Errorf("stream missing %q:\n%s", want, out)
			}
		}
		// 参数 JSON 分片应原样透传
		if !strings.Contains(out, `\"city\":`) || !strings.Contains(out, `\"Paris\"`) {
			t.Errorf("stream missing argument fragments:\n%s", out)
		}
	})

	t.Run("non_stream", func(t *testing.T) {
		resp, err := http.Post(gw.URL+"/v1/messages", "application/json", strings.NewReader(strings.Replace(reqBody, "%v", "false", 1)))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		out := string(body)
		if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
			t.Errorf("Content-Type = %q", ct)
		}
		for _, want := range []string{`"type":"tool_use"`, `"name":"get_weather"`, `"city":"Paris"`, `"stop_reason":"tool_use"`} {
			if !strings.Contains(out, want) {
				t.Errorf("response missing %q:\n%s", want, out)
			}
		}
	})

	// 上游收到 OpenAI 形态的 tools
	if !strings.Contains(rec.body, `"type":"function"`) || !strings.Contains(rec.body, `"name":"get_weather"`) {
		t.Errorf("upstream did not receive openai-style tools: %s", rec.body)
	}
	if strings.Contains(rec.body, "input_schema") {
		t.Errorf("upstream request leaked anthropic-style input_schema: %s", rec.body)
	}
}
