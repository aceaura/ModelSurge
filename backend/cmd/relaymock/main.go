// relaymock is a local LLM upstream simulator: multiple instances in one
// process, each speaking OpenAI or Anthropic protocol (stream + non-stream),
// exposing a newapi-style balance endpoint, with a control endpoint for
// fault injection (balance drain, 429, slow, down, auth errors).
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"
)

type instanceCfg struct {
	Name           string `yaml:"name"`
	Listen         string `yaml:"listen"`
	Protocol       string `yaml:"protocol"` // openai | claude
	Quota          int64  `yaml:"quota"`    // newapi quota units; relayd probe scale converts to USD
	CostPerRequest int64  `yaml:"cost_per_request"`
	APIKey         string `yaml:"api_key"`
}

type config struct {
	Instances []instanceCfg `yaml:"instances"`
}

type instance struct {
	cfg instanceCfg

	quota atomic.Int64
	mode  atomic.Value // string: normal | ratelimit | slow | down | auth_error
	delay atomic.Int64 // ms, used by slow mode
}

func (in *instance) currentMode() string {
	if v := in.mode.Load(); v != nil {
		return v.(string)
	}
	return "normal"
}

func main() {
	configPath := flag.String("config", "relaymock.yaml", "path to mock config")
	flag.Parse()

	raw, err := os.ReadFile(*configPath)
	if err != nil {
		log.Fatalf("read config: %v", err)
	}
	var cfg config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		log.Fatalf("parse config: %v", err)
	}
	if len(cfg.Instances) == 0 {
		log.Fatal("no instances configured")
	}

	for _, ic := range cfg.Instances {
		in := &instance{cfg: ic}
		in.quota.Store(ic.Quota)
		in.mode.Store("normal")

		mux := http.NewServeMux()
		switch ic.Protocol {
		case "openai":
			mux.HandleFunc("/v1/chat/completions", in.serveOpenAI)
			mux.HandleFunc("/api/user/self", in.serveBalance)
		case "claude":
			mux.HandleFunc("/v1/messages", in.serveClaude)
		default:
			log.Fatalf("instance %s: unknown protocol %q", ic.Name, ic.Protocol)
		}
		mux.HandleFunc("/mock/control", in.serveControl)
		mux.HandleFunc("/mock/state", in.serveState)

		go func() {
			log.Printf("mock %s (%s) listening on %s", ic.Name, ic.Protocol, ic.Listen)
			if err := http.ListenAndServe(ic.Listen, mux); err != nil {
				log.Fatalf("instance %s: %v", ic.Name, err)
			}
		}()
	}
	select {}
}

// ---- control plane ----

func (in *instance) serveControl(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Mode    string `json:"mode"`
		DelayMs int64  `json:"delay_ms"`
		Quota   *int64 `json:"quota"`
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
			return
		}
	}
	switch req.Mode {
	case "":
	case "normal", "ratelimit", "slow", "down", "auth_error":
		in.mode.Store(req.Mode)
	default:
		http.Error(w, "unknown mode "+req.Mode, http.StatusBadRequest)
		return
	}
	if req.DelayMs > 0 {
		in.delay.Store(req.DelayMs)
	}
	if req.Quota != nil {
		in.quota.Store(*req.Quota)
	}
	in.serveState(w, r)
}

func (in *instance) serveState(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{
		"name":     in.cfg.Name,
		"protocol": in.cfg.Protocol,
		"quota":    in.quota.Load(),
		"mode":     in.currentMode(),
		"delay_ms": in.delay.Load(),
	})
}

// ---- shared gates ----

// gate applies auth check and failure modes. Returns false if a failure
// response was written.
func (in *instance) gate(w http.ResponseWriter, r *http.Request) bool {
	if !in.authOK(r) || in.currentMode() == "auth_error" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":{"message":"invalid api key","type":"authentication_error"}}`)
		return false
	}
	switch in.currentMode() {
	case "ratelimit":
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":{"message":"rate limit exceeded","type":"rate_limit_error"}}`)
		return false
	case "slow":
		d := in.delay.Load()
		if d <= 0 {
			d = 10000
		}
		time.Sleep(time.Duration(d) * time.Millisecond)
	case "down":
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `{"error":{"message":"upstream unavailable"}}`)
		return false
	}
	return true
}

func (in *instance) authOK(r *http.Request) bool {
	if in.cfg.APIKey == "" {
		return true
	}
	switch in.cfg.Protocol {
	case "openai":
		return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") == in.cfg.APIKey
	case "claude":
		return r.Header.Get("x-api-key") == in.cfg.APIKey
	}
	return false
}

func requestModel(r *http.Request) (string, bool) {
	var req struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	_ = json.Unmarshal(body, &req)
	return req.Model, req.Stream
}

func replyText(name string) []string {
	return []string{"mock ", "reply ", "from ", name}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// ---- OpenAI protocol ----

func (in *instance) serveOpenAI(w http.ResponseWriter, r *http.Request) {
	if !in.gate(w, r) {
		return
	}
	model, stream := requestModel(r)
	in.quota.Add(-in.cfg.CostPerRequest)
	if stream {
		in.serveOpenAIStream(w, model)
		return
	}
	writeJSON(w, map[string]any{
		"id":      "chatcmpl-mock-" + in.cfg.Name,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": strings.Join(replyText(in.cfg.Name), "")},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 8, "total_tokens": 18},
	})
}

func (in *instance) serveOpenAIStream(w http.ResponseWriter, model string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	fl, _ := w.(http.Flusher)
	send := func(v map[string]any) {
		b, _ := json.Marshal(v)
		fmt.Fprintf(w, "data: %s\n\n", b)
		if fl != nil {
			fl.Flush()
		}
	}
	base := map[string]any{
		"id":      "chatcmpl-mock-" + in.cfg.Name,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   model,
	}
	chunk := func(delta map[string]any, finish any) map[string]any {
		c := map[string]any{}
		for k, v := range base {
			c[k] = v
		}
		c["choices"] = []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}
		return c
	}
	send(chunk(map[string]any{"role": "assistant"}, nil))
	for _, piece := range replyText(in.cfg.Name) {
		send(chunk(map[string]any{"content": piece}, nil))
		time.Sleep(20 * time.Millisecond)
	}
	send(chunk(map[string]any{}, "stop"))
	usage := map[string]any{}
	for k, v := range base {
		usage[k] = v
	}
	usage["choices"] = []any{}
	usage["usage"] = map[string]any{"prompt_tokens": 10, "completion_tokens": 8, "total_tokens": 18}
	send(usage)
	fmt.Fprint(w, "data: [DONE]\n\n")
	if fl != nil {
		fl.Flush()
	}
}

// serveBalance mimics new-api /api/user/self; quota is in station units.
func (in *instance) serveBalance(w http.ResponseWriter, r *http.Request) {
	if !in.authOK(r) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"success":false,"message":"invalid api key"}`)
		return
	}
	writeJSON(w, map[string]any{
		"success": true,
		"data": map[string]any{
			"quota": in.quota.Load(),
		},
	})
}

// ---- Anthropic protocol ----

func (in *instance) serveClaude(w http.ResponseWriter, r *http.Request) {
	if !in.gate(w, r) {
		return
	}
	model, stream := requestModel(r)
	in.quota.Add(-in.cfg.CostPerRequest)
	if stream {
		in.serveClaudeStream(w, model)
		return
	}
	writeJSON(w, map[string]any{
		"id":            "msg-mock-" + in.cfg.Name,
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       []any{map[string]any{"type": "text", "text": strings.Join(replyText(in.cfg.Name), "")}},
		"stop_reason":   "end_turn",
		"stop_sequence": nil,
		"usage":         map[string]any{"input_tokens": 10, "output_tokens": 8},
	})
}

func (in *instance) serveClaudeStream(w http.ResponseWriter, model string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	fl, _ := w.(http.Flusher)
	send := func(event string, v map[string]any) {
		b, _ := json.Marshal(v)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		if fl != nil {
			fl.Flush()
		}
	}
	send("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            "msg-mock-" + in.cfg.Name,
			"type":          "message",
			"role":          "assistant",
			"model":         model,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         map[string]any{"input_tokens": 10, "output_tokens": 0},
		},
	})
	send("content_block_start", map[string]any{
		"type":          "content_block_start",
		"index":         0,
		"content_block": map[string]any{"type": "text", "text": ""},
	})
	for _, piece := range replyText(in.cfg.Name) {
		send("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": 0,
			"delta": map[string]any{"type": "text_delta", "text": piece},
		})
		time.Sleep(20 * time.Millisecond)
	}
	send("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	send("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": 8},
	})
	send("message_stop", map[string]any{"type": "message_stop"})
}
