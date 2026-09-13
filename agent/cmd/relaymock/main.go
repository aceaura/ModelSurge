// Command relaymock runs a deterministic HTTP provider for local and black-box E2E tests.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"strings"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:18102", "listen address")
	flag.Parse()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("POST /v1/messages", anthropic)
	mux.HandleFunc("POST /v1/chat/completions", openAIChat)
	mux.HandleFunc("POST /v1/responses", openAIResponses)

	log.Printf("relaymock listening on %s", *listen)
	log.Fatal(http.ListenAndServe(*listen, mux))
}

type request struct {
	Model  string `json:"model"`
	Stream bool   `json:"stream"`
}

func decodeRequest(w http.ResponseWriter, r *http.Request) (request, bool) {
	var req request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return request{}, false
	}
	return req, true
}

func anthropic(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeRequest(w, r)
	if !ok {
		return
	}
	if r.Header.Get("x-api-key") == "" {
		http.Error(w, "missing x-api-key", http.StatusUnauthorized)
		return
	}
	model := req.Model
	if model == "" {
		model = "mock-anthropic"
	}
	w.Header().Set("Content-Type", "text/event-stream")
	flusher, _ := w.(http.Flusher)
	frames := []string{
		fmt.Sprintf(`event: message_start\ndata: {"type":"message_start","message":{"id":"msg_mock","type":"message","role":"assistant","model":%q,"content":[],"usage":{"input_tokens":3,"output_tokens":0}}}\n\n`, model),
		`event: content_block_start\ndata: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}\n\n`,
		`event: content_block_delta\ndata: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"mock hello"}}\n\n`,
		`event: content_block_stop\ndata: {"type":"content_block_stop","index":0}\n\n`,
		`event: message_delta\ndata: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}\n\n`,
		`event: message_stop\ndata: {"type":"message_stop"}\n\n`,
	}
	for _, frame := range frames {
		_, _ = w.Write([]byte(frame))
		if flusher != nil {
			flusher.Flush()
		}
	}
}

func openAIChat(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeRequest(w, r)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = "mock-openai"
	}
	_, _ = fmt.Fprintf(w, "data: {\"id\":\"chatcmpl_mock\",\"object\":\"chat.completion.chunk\",\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"mock hello\"},\"finish_reason\":null}]}\n\n", model)
	_, _ = fmt.Fprint(w, "data: {\"id\":\"chatcmpl_mock\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2}}\n\ndata: [DONE]\n\n")
}

func openAIResponses(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeRequest(w, r)
	if !ok {
		return
	}
	model := req.Model
	if model == "" {
		model = "mock-responses"
	}
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = fmt.Fprintf(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_mock\",\"model\":%q,\"status\":\"in_progress\",\"output\":[]}}\n\n", model)
	_, _ = fmt.Fprint(w, "event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"id\":\"msg_mock\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[]}}\n\nevent: response.content_part.added\ndata: {\"type\":\"response.content_part.added\",\"output_index\":0,\"content_index\":0,\"part\":{\"type\":\"output_text\",\"text\":\"\"}}\n\nevent: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"content_index\":0,\"delta\":\"mock hello\"}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_mock\",\"status\":\"completed\",\"usage\":{\"input_tokens\":3,\"output_tokens\":2}}}\n\n")
}
