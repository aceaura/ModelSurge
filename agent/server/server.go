// Package server 提供客户端入口 HTTP 服务：按路径识别客户端协议，
// 解码为 IR 请求后交给 relay.Forwarder 转发。
package server

import (
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/aceaura/ModelSurge/agent/agentstore"
	"github.com/aceaura/ModelSurge/agent/config"
	"github.com/aceaura/ModelSurge/agent/ir"
	"github.com/aceaura/ModelSurge/agent/proto"
	"github.com/aceaura/ModelSurge/agent/proto/kiro"
	"github.com/aceaura/ModelSurge/agent/relay"
	"github.com/aceaura/ModelSurge/agent/replayclient"
	"github.com/aceaura/ModelSurge/replay/contract/replayv1"

	// 注册全部协议 codec
	_ "github.com/aceaura/ModelSurge/agent/proto/anthropic"
	_ "github.com/aceaura/ModelSurge/agent/proto/gemini"
	_ "github.com/aceaura/ModelSurge/agent/proto/openaichat"
	_ "github.com/aceaura/ModelSurge/agent/proto/openairesponses"
)

// Server 入口服务。
type Server struct {
	fwd       *relay.Forwarder
	replay    *replayclient.Client
	mux       *http.ServeMux
	accessLog bool
}

// New retains the legacy composition for compatibility tests.
// NewRelay composes the production relay without an in-process account Manager.
func New(cfg *config.Config, replay *replayclient.Client, store *agentstore.Store) *Server {
	s := &Server{fwd: relay.NewForwarder(cfg, replay, store), replay: replay, mux: http.NewServeMux(), accessLog: cfg.AccessLogEnabled}
	s.mountPublic()
	return s
}

func (s *Server) mountPublic() {
	s.mux.HandleFunc("POST /v1/messages", s.handleChat("anthropic"))
	s.mux.HandleFunc("POST /v1/messages/count_tokens", s.handleCountTokens)
	s.mux.HandleFunc("POST /v1/chat/completions", s.handleChat("openai-chat"))
	s.mux.HandleFunc("POST /v1/responses", s.handleChat("openai-responses"))
	s.mux.HandleFunc("POST /v1beta/models/", s.handleGemini)
	s.mux.HandleFunc("GET /v1/models", s.handleModels)
	// 协议前缀别名：同一地址按 /anthropic /openai /gemini 前缀区分接入协议
	s.mux.HandleFunc("POST /anthropic/v1/messages", s.handleChat("anthropic"))
	s.mux.HandleFunc("POST /anthropic/v1/messages/count_tokens", s.handleCountTokens)
	s.mux.HandleFunc("POST /openai/v1/chat/completions", s.handleChat("openai-chat"))
	s.mux.HandleFunc("POST /openai/v1/responses", s.handleChat("openai-responses"))
	s.mux.HandleFunc("POST /gemini/v1beta/models/", s.handleGemini)
	s.mux.HandleFunc("GET /openai/v1/models", s.handleModels)
	// 无 /v1 前缀裸路径：客户端 base_url 不带 /v1 时（DeepSeek 原生风格 /chat/completions 等）自动适配
	s.mux.HandleFunc("POST /chat/completions", s.handleChat("openai-chat"))
	s.mux.HandleFunc("POST /responses", s.handleChat("openai-responses"))
	s.mux.HandleFunc("POST /messages", s.handleChat("anthropic"))
	s.mux.HandleFunc("POST /messages/count_tokens", s.handleCountTokens)
	s.mux.HandleFunc("GET /models", s.handleModels)
	s.mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte("ok"))
	})
}

// NewAccountAdmin exposes the compatible account administration surface for the upstream process.
// SetProbeClient 替换管理面端点探测用 HTTP client（测试注入替身；
// 生产保持 New 设置的 http.DefaultClient）。
// Handler 返回根 handler（访问日志 -> 客户端鉴权与管理面分流 -> 路由）。
// /admin 前缀走 X-Admin-Key 鉴权（独立于客户端 api_key，避免客户端鉴权拦截管理请求）。
func (s *Server) Handler() http.Handler { return s.accessLogMiddleware(s.mux) }

// statusRecorder 记录中间件层拿不到的响应状态码。
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Flush 透传底层 Flusher：流式响应依赖逐块刷新，包装后不能丢。
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// accessLog 记录 method/path/status/耗时；鉴权失败（401）也在本层之内。
func (s *Server) accessLogMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.accessLog {
			next.ServeHTTP(w, r)
			return
		}
		rec := &statusRecorder{ResponseWriter: w, status: 200}
		start := time.Now()
		next.ServeHTTP(rec, r)
		log.Printf("agent: %d %s %s %s %s", rec.status, r.Method, r.URL.Path,
			time.Since(start).Round(time.Millisecond), r.RemoteAddr)
	})
}

// auth 校验客户端 key：兼容 Authorization: Bearer 与 x-api-key。
func requestAPIKey(r *http.Request) string {
	key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if key == r.Header.Get("Authorization") {
		key = r.Header.Get("x-api-key")
	}
	return key
}

// handleChat 三个 JSON-body 协议的统一入口。
func (s *Server) handleChat(codecName string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		codec := proto.MustInbound(codecName)
		body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
		if err != nil {
			s.renderError(w, codec, ir.NewHTTPError(400, "read body: "+err.Error()))
			return
		}
		req, err := codec.DecodeRequest(body)
		if err != nil {
			s.renderError(w, codec, ir.NewHTTPError(400, err.Error()))
			return
		}
		s.fwd.Forward(r.Context(), w, codec, req, requestAPIKey(r))
	}
}

// handleGemini Gemini 原生入口：[/gemini]/v1beta/models/{model}:generateContent
// 或 :streamGenerateContent。模型名与流式标志由路径决定（body 内无 stream 字段）。
func (s *Server) handleGemini(w http.ResponseWriter, r *http.Request) {
	codec := proto.MustInbound("gemini")
	path := strings.TrimPrefix(r.URL.Path, "/gemini")
	rest := strings.TrimPrefix(path, "/v1beta/models/")
	model, action, found := strings.Cut(rest, ":")
	if !found || model == "" {
		s.renderError(w, codec, ir.NewHTTPError(404, "unknown gemini action: "+r.URL.Path))
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		s.renderError(w, codec, ir.NewHTTPError(400, "read body: "+err.Error()))
		return
	}
	req, err := codec.DecodeRequest(body)
	if err != nil {
		s.renderError(w, codec, ir.NewHTTPError(400, err.Error()))
		return
	}
	req.Model = model
	req.Stream = action == "streamGenerateContent"
	s.fwd.Forward(r.Context(), w, codec, req, requestAPIKey(r))
}

// handleCountTokens Anthropic count_tokens 入口：请求体与 /v1/messages 同形。
// Claude Code 等客户端会调它做上下文窗口计量。
func (s *Server) handleCountTokens(w http.ResponseWriter, r *http.Request) {
	codec := proto.MustInbound("anthropic")
	body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		s.renderError(w, codec, ir.NewHTTPError(400, "read body: "+err.Error()))
		return
	}
	req, err := codec.DecodeRequest(body)
	if err != nil {
		s.renderError(w, codec, ir.NewHTTPError(400, err.Error()))
		return
	}
	_, derr := s.replay.Dispatch(r.Context(), replayv1.DispatchRequest{Model: req.Model, InboundProtocol: "anthropic", ClientKey: requestAPIKey(r), RequestID: "count-" + strconv.FormatInt(time.Now().UnixNano(), 10)})
	if derr != nil {
		s.renderReplayError(w, codec, derr)
		return
	}
	status, respBody := s.fwd.CountTokens(req)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(respBody)
}

// handleModels 列出账号池可用模型（Manager.Models 并集口径）。
// Claude ID 以横线形态展示（model_meta.go DashifyClaudeID：Claude Code /
// Desktop 只认横线形态；请求侧 normalize 等价解析回点号）。
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	models, err := s.replay.Models(r.Context())
	if err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"type":"upstream_error","message":"model catalog unavailable"}}`))
		return
	}
	var sb strings.Builder
	sb.WriteString(`{"object":"list","data":[`)
	first := true
	for _, m := range models {
		if !m.Enabled {
			continue
		}
		if !first {
			sb.WriteByte(',')
		}
		first = false
		sb.WriteString(`{"id":`)
		sb.WriteString(strconv.Quote(kiro.DashifyClaudeID(m.Name)))
		sb.WriteString(`,"object":"model"}`)
	}
	sb.WriteString(`]}`)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(sb.String()))
}

func (s *Server) renderError(w http.ResponseWriter, codec proto.InboundCodec, e *ir.Error) {
	status, body := codec.RenderError(e)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func (s *Server) renderReplayError(w http.ResponseWriter, codec proto.InboundCodec, err error) {
	status := 503
	typ := ir.ErrTypeUpstream
	if e, ok := err.(replayv1.Error); ok && e.Code == replayv1.CodeUnauthorized {
		status = 401
		typ = ir.ErrTypeAuth
	}
	s.renderError(w, codec, &ir.Error{StatusCode: status, Type: typ, Message: err.Error()})
}
