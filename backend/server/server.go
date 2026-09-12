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

	"relayd/backend/account"
	"relayd/backend/config"
	"relayd/backend/ir"
	"relayd/backend/proto"
	"relayd/backend/proto/kiro"
	"relayd/backend/relay"

	// 注册全部协议 codec
	_ "relayd/backend/proto/anthropic"
	_ "relayd/backend/proto/gemini"
	_ "relayd/backend/proto/openaichat"
	_ "relayd/backend/proto/openairesponses"
)

// Server 入口服务。
type Server struct {
	fwd       *relay.Forwarder
	apiKey    string
	mux       *http.ServeMux
	accessLog bool

	sched    *account.Manager // 账号池调度（必填：候选与 /v1/models 模型源）
	store    *account.Store   // 管理面 CRUD 落库
	adminKey string           // X-Admin-Key；空则不挂载管理面
	adminMux *http.ServeMux   // /admin 管理路由
}

// New 按配置构造服务。sched 非空时启用账号池动态调度（限流冷却 + 粘性取号）。
func New(cfg *config.Config, sched *account.Manager) *Server {
	s := &Server{
		fwd:       relay.NewForwarder(cfg, sched),
		apiKey:    cfg.APIKey,
		mux:       http.NewServeMux(),
		accessLog: cfg.AccessLogEnabled,
	}
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
	s.mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte("ok"))
	})
	if sched != nil {
		s.sched = sched
		if cfg.Admin != nil && cfg.Admin.APIKey != "" {
			s.store = sched.Store()
			s.adminKey = cfg.Admin.APIKey
			s.mountAdmin()
		}
	}
	return s
}

// Handler 返回根 handler（访问日志 -> 客户端鉴权与管理面分流 -> 路由）。
// /admin 前缀走 X-Admin-Key 鉴权（独立于客户端 api_key，避免客户端鉴权拦截管理请求）。
func (s *Server) Handler() http.Handler {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.adminMux != nil && strings.HasPrefix(r.URL.Path, "/admin") {
			s.adminAuth(s.adminMux).ServeHTTP(w, r)
			return
		}
		s.auth(s.mux).ServeHTTP(w, r)
	})
	return s.accessLogMiddleware(inner)
}

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
		log.Printf("relayd: %d %s %s %s %s", rec.status, r.Method, r.URL.Path,
			time.Since(start).Round(time.Millisecond), r.RemoteAddr)
	})
}

// auth 校验客户端 key：兼容 Authorization: Bearer 与 x-api-key。
func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.apiKey == "" || r.URL.Path == "/health" {
			next.ServeHTTP(w, r)
			return
		}
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if key == r.Header.Get("Authorization") { // 无 Bearer 前缀
			key = r.Header.Get("x-api-key")
		}
		if key != s.apiKey {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"error":{"type":"authentication_error","message":"invalid api key"}}`))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// handleChat 三个 JSON-body 协议的统一入口。
func (s *Server) handleChat(codecName string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		codec := proto.Must(codecName)
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
		s.fwd.Forward(r.Context(), w, codec, req)
	}
}

// handleGemini Gemini 原生入口：[/gemini]/v1beta/models/{model}:generateContent
// 或 :streamGenerateContent。模型名与流式标志由路径决定（body 内无 stream 字段）。
func (s *Server) handleGemini(w http.ResponseWriter, r *http.Request) {
	codec := proto.Must("gemini")
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
	s.fwd.Forward(r.Context(), w, codec, req)
}

// handleCountTokens Anthropic count_tokens 入口：请求体与 /v1/messages 同形。
// Claude Code 等客户端会调它做上下文窗口计量。
func (s *Server) handleCountTokens(w http.ResponseWriter, r *http.Request) {
	codec := proto.Must("anthropic")
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
	status, respBody := s.fwd.CountTokens(r.Context(), req)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(respBody)
}

// handleModels 列出账号池可用模型（Manager.Models 并集口径）。
// Claude ID 以横线形态展示（model_meta.go DashifyClaudeID：Claude Code /
// Desktop 只认横线形态；请求侧 normalize 等价解析回点号）。
func (s *Server) handleModels(w http.ResponseWriter, _ *http.Request) {
	models := s.sched.Models()
	var sb strings.Builder
	sb.WriteString(`{"object":"list","data":[`)
	for i, m := range models {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(`{"id":`)
		sb.WriteString(strconv.Quote(kiro.DashifyClaudeID(m)))
		sb.WriteString(`,"object":"model"}`)
	}
	sb.WriteString(`]}`)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(sb.String()))
}

func (s *Server) renderError(w http.ResponseWriter, codec proto.Codec, e *ir.Error) {
	status, body := codec.RenderError(e)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
