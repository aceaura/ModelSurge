package relay

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"relayd/backend/account"
	"relayd/backend/config"
	"relayd/backend/ir"
	"relayd/backend/proto"
	"relayd/backend/proto/kiro"

	"github.com/google/uuid"
)

// Forwarder 把 IR 请求转发到上游，并把上游响应回传给客户端。
// 核心原则（调研共识）：对上游永远以流式方式请求；
// 客户端要非流式时由网关聚合后一次性返回。
//
// 重试原则：只要还没向客户端写出任何字节（连接失败、429/5xx、
// 首事件超时、非流式聚合失败），就换下一个候选账号重发；
// 一旦写出第一个字节即锁死，错误只能在流内就地渲染。
//
// 候选一律来自 account.Manager（SQLite 账号池）：
// 限流（429）→ 账号冷却并切号；鉴权失败（401/403）→ 账号禁用并切号；
// 瞬时错误（5xx/网络/超时）→ 原地重试同账号，不轻易切（保缓存命中）。
type Forwarder struct {
	client             *http.Client
	firstTokenTimeout  time.Duration
	estimateUsage      bool
	sched              *account.Manager
	sameAccountRetries int
	trunc              *TruncationTracker
	paramLog           bool // 请求/响应参数日志（与访问日志同开关）

	// kiro 段运行参数（零值 = 不生效）。
	kiroFirstTokenTimeout    time.Duration // kiro 候选首事件超时；0 = 沿用全局
	kiroStreamingReadTimeout time.Duration // kiro 流式 chunk 间看门狗；0 = 禁用
	kiroWebSearchInject      bool          // web_search 注入全局默认（账号级可另开）
}

// NewForwarder 构造转发器。sched 为账号池调度器（必填）。
func NewForwarder(cfg *config.Config, sched *account.Manager) *Forwarder {
	f := &Forwarder{
		// Kiro 出站流量经云中转/调试 transport（按配置，两项都关时为默认）
		client:            &http.Client{Timeout: 0, Transport: account.KiroTransport()},
		firstTokenTimeout: cfg.FirstTokenTimeoutDur,
		estimateUsage:     cfg.EstimateUsage,
		sched:             sched,
		trunc:             NewTruncationTracker(cfg.TruncationRecoveryEnabled),
		paramLog:          cfg.AccessLogEnabled,
	}
	if sched != nil && cfg.Scheduler != nil {
		f.sameAccountRetries = cfg.Scheduler.SameAccountRetries
	}
	if k := cfg.Kiro; k != nil {
		f.kiroFirstTokenTimeout = k.FirstTokenTimeoutDur
		f.kiroStreamingReadTimeout = k.StreamingReadTimeoutDur
		f.kiroWebSearchInject = k.WebSearchInject
	}
	return f
}

// candidate 一个可服务某 canonical model 的账号候选及其 native 模型名与 codec。
type candidate struct {
	name     string           // 账号名（日志/参数摘要用）
	protocol string           // 上游协议（kiro 账号恒为 "kiro"）
	acc      *account.Account // 账号本体
	native   string
	codec    proto.Codec
	ov       *ir.Overrides    // 账号级请求参数覆盖（nil = 透传）
	resolve  endpointResolver // 每请求解析 URL 与鉴权头（kiro 动态取 token/host）
}

// endpointResolver 解析一次上游请求的 URL 与请求头。api-key 账号构造时固化；
// kiro 账号每次请求现取 token（含预刷新）与 host 分流。
type endpointResolver func(ctx context.Context) (url string, headers map[string]string, err *ir.Error)

// staticEndpoint api-key 账号：URL 与鉴权头固定；extra 为账号级自定义头
// （如网关要求的会话头），同名键覆盖协议默认头。
func staticEndpoint(protocol, baseURL, apiKey, nativeModel string, extra map[string]string) endpointResolver {
	url, headers := endpoint(protocol, baseURL, apiKey, nativeModel)
	if len(extra) > 0 {
		merged := make(map[string]string, len(headers)+len(extra))
		for k, v := range headers {
			merged[k] = v
		}
		for k, v := range extra {
			merged[k] = v
		}
		headers = merged
	}
	return func(context.Context) (string, map[string]string, *ir.Error) {
		return url, headers, nil
	}
}

// kiroEndpoint kiro 账号：每次请求现取 token（GetAccessToken 含预刷新），
// host 按 profileArn 分流（runtime/q），头伪造 KiroIDE 指纹。
func kiroEndpoint(rt *account.KiroRuntime) endpointResolver {
	return func(ctx context.Context) (string, map[string]string, *ir.Error) {
		token, _, err := rt.Auth.GetAccessToken(ctx)
		if err != nil {
			return "", nil, &ir.Error{StatusCode: 401, Type: ir.ErrTypeAuth,
				Message: "kiro token: " + err.Error(), Retryable: true}
		}
		headers := account.KiroHeaders(rt.Auth.Fingerprint(), token, account.TargetGenerateAssistantResponse)
		headers["Connection"] = "close" // 防流式响应后连接滞留 CLOSE_WAIT（KiroaaS issue#38 同款）
		return rt.Auth.ChatHost() + "/generateAssistantResponse", headers, nil
	}
}

// endpoint api-key 账号的上游请求 URL 与鉴权头。
func endpoint(protocol, baseURL, apiKey, nativeModel string) (url string, headers map[string]string) {
	switch protocol {
	case "anthropic":
		return baseURL + "/v1/messages", map[string]string{
			"x-api-key":         apiKey,
			"anthropic-version": "2023-06-01",
		}
	case "openai-responses":
		return baseURL + "/v1/responses", map[string]string{"Authorization": "Bearer " + apiKey}
	case "codex":
		// Codex 订阅端点（ChatGPT OAuth）：路径无 /v1 前缀；身份头必须配套
		// （originator 与 User-Agent 首段一致、version 不低于上游门槛，
		// 否则上游 404 —— sub2api issue #3901）。账号 Headers 可覆盖默认头，
		// chatgpt-account-id 由账号配置补充（多 workspace 账号必需）。
		if baseURL == "" {
			baseURL = "https://chatgpt.com/backend-api/codex"
		}
		return baseURL + "/responses", map[string]string{
			"Authorization": "Bearer " + apiKey,
			"originator":    "codex-tui",
			"User-Agent":    "codex-tui/0.146.0 (Ubuntu 22.4.0; x86_64) xterm-256color",
			"version":       "0.146.0",
			"OpenAI-Beta":   "responses=experimental",
			"session_id":    uuid.NewString(), // 每请求新会话 id，对齐 sub2api 隔离语义
		}
	case "gemini":
		return fmt.Sprintf("%s/v1beta/models/%s:streamGenerateContent?alt=sse", baseURL, nativeModel),
			map[string]string{"x-goog-api-key": apiKey}
	default: // openai-chat
		return baseURL + "/v1/chat/completions", map[string]string{"Authorization": "Bearer " + apiKey}
	}
}

// maxErrBody 上游错误体的读取上限。
const maxErrBody = 4 * 1024

// Forward 执行一次转发。clientCodec 为客户端协议 codec（用于错误渲染与响应编码），
// req.Stream 表示客户端是否要求流式。所有响应直接写入 w。
func (f *Forwarder) Forward(ctx context.Context, w http.ResponseWriter, clientCodec proto.Codec, req *ir.Request) {
	if f.paramLog {
		log.Printf("relay: request %s", requestParams(clientCodec.Name(), req))
	}
	f.trunc.InjectNotices(req) // 上次截断的恢复提示（命中才修改）
	f.forwardScheduled(ctx, w, clientCodec, req)
}

// accountCandidate 把账号转为转发候选。api-key 走静态端点；
// kiro 走动态端点（每次请求现取 token）并要求运行时就位。
func (f *Forwarder) accountCandidate(a *account.Account, model string) (candidate, error) {
	protocol := a.Protocol
	if a.Type == account.TypeKiro {
		protocol = "kiro"
	}
	c, err := proto.Get(protocol)
	if err != nil {
		return candidate{}, err
	}
	native, _ := a.Serving(model)
	cand := candidate{
		name:     a.Name,
		protocol: protocol,
		acc:      a,
		native:   native,
		codec:    c,
		ov:       a.Overrides,
	}
	if a.Type == account.TypeKiro {
		rt := f.sched.KiroRuntimeOf(a.Name)
		if rt == nil {
			return candidate{}, fmt.Errorf("kiro runtime missing for account %q", a.Name)
		}
		cand.resolve = kiroEndpoint(rt)
	} else {
		cand.resolve = staticEndpoint(protocol, a.BaseURL, a.APIKey, native, a.Headers)
	}
	return cand, nil
}

// forwardScheduled 账号池调度模式：粘性取号 + 错误分类处置。
func (f *Forwarder) forwardScheduled(ctx context.Context, w http.ResponseWriter, clientCodec proto.Codec, req *ir.Request) {
	tried := map[string]bool{}
	var lastErr *ir.Error
	for {
		if ctx.Err() != nil {
			return // 客户端已断开
		}
		acc, ok := f.sched.Next(req.Model, tried)
		if !ok {
			if lastErr == nil {
				lastErr = ir.NewHTTPError(503, "no available account: all cooling down or disabled")
			}
			writeError(w, clientCodec, lastErr)
			return
		}
		tried[acc.Name] = true
		cand, err := f.accountCandidate(acc, req.Model)
		if err != nil {
			lastErr = ir.NewHTTPError(500, err.Error())
			continue
		}
		if len(tried) > 1 {
			log.Printf("relay: model %q switch to account %s: %s", req.Model, acc.Name, lastErr)
		}
		isKiro := acc.Type == account.TypeKiro
		forceRefreshed := false
		switchAccount := false
		for try := 0; ; try++ {
			onUsage := func(u *ir.Usage) {
				if u == nil || u.Estimated {
					return // 估算值不入库
				}
				f.sched.ReportUsage(acc.Name, account.Usage{
					InputTokens:   int64(u.InputTokens),
					OutputTokens:  int64(u.OutputTokens),
					CacheRead:     int64(u.CacheReadTokens),
					CacheCreation: int64(u.CacheCreationTokens),
				})
			}
			wrote, aerr := f.attempt(ctx, w, clientCodec, cand, req, onUsage)
			if aerr == nil {
				f.sched.ReportSuccess(acc.Name)
				return
			}
			lastErr = aerr
			if wrote {
				f.sched.ReportSuccess(acc.Name) // 已写出字节，流已结束——按成功结算
				return
			}
			switch {
			case aerr.StatusCode == 429:
				f.sched.ReportLimit(acc.Name, aerr.Message)
				switchAccount = true
			case isKiro && aerr.StatusCode == 402:
				// 配额超限：冷却到 GetUsageLimits 重置日期（不可得兜底 1h）+ 切号
				until := f.quotaCooldownUntil(ctx, acc)
				f.sched.ReportRecoverable(acc.Name, until)
				log.Printf("relay: kiro account %s quota exceeded, cooling until %s", acc.Name, until.Format(time.RFC3339))
				switchAccount = true
			case aerr.StatusCode == 401 || aerr.StatusCode == 403:
				// kiro：强刷 token 原地重试 1 次；仍败才禁用
				if isKiro && !forceRefreshed {
					forceRefreshed = true
					if rt := f.sched.KiroRuntimeOf(acc.Name); rt != nil {
						if ferr := rt.Auth.ForceRefresh(ctx); ferr == nil {
							log.Printf("relay: kiro account %s %d, token force refreshed, retrying in place", acc.Name, aerr.StatusCode)
							continue
						} else {
							log.Printf("relay: kiro account %s force refresh failed: %v", acc.Name, ferr)
						}
					}
				}
				f.sched.ReportAuthFailure(acc.Name)
				switchAccount = true
			case isKiro && aerr.Reason == account.ReasonInvalidModelID:
				// 订阅不含该模型：换号试试，不惩罚本账号
				log.Printf("relay: kiro account %s rejects model %q (subscription), switching", acc.Name, req.Model)
				switchAccount = true
			case !aerr.Retryable:
				writeError(w, clientCodec, aerr)
				return
			case try >= f.sameAccountRetries:
				log.Printf("relay: account %s transient failure persisted, switching", acc.Name)
				f.sched.ReportTransientFailure(acc.Name)
				switchAccount = true
			default:
				log.Printf("relay: account %s transient error, retrying in place (try %d): %s", acc.Name, try+1, aerr)
			}
			if switchAccount {
				break
			}
		}
	}
}

// quotaFallbackCooldown GetUsageLimits 不可得（免费账号无 profileArn 等）时
// 402 配额冷却的兜底时长。
const quotaFallbackCooldown = time.Hour

// quotaCooldownUntil kiro 402 的冷却时刻：resetDate 拉取失败/缺失兜底 1h。
func (f *Forwarder) quotaCooldownUntil(ctx context.Context, acc *account.Account) time.Time {
	if rt := f.sched.KiroRuntimeOf(acc.Name); rt != nil {
		if until := rt.QuotaCooldownUntil(ctx); until.After(time.Now()) {
			return until
		}
	}
	return time.Now().Add(quotaFallbackCooldown)
}

// openUpstream 对单候选发起一次上游请求：编码、解析端点、POST、
// 非 2xx 分类与 kiro 读超时看门狗包装。调用方负责 resp.Body.Close() 与 cancel。
// 供普通 attempt 与严格工具策略的恢复重发共用。
func (f *Forwarder) openUpstream(ctx context.Context, cand candidate, upReq *ir.Request) (*http.Response, context.CancelFunc, *ir.Error) {
	body, encErr := cand.codec.EncodeRequest(upReq)
	if encErr != nil {
		return nil, nil, ir.NewHTTPError(400, "encode upstream request: "+encErr.Error())
	}

	url, headers, rerr := cand.resolve(ctx)
	if rerr != nil {
		return nil, nil, rerr
	}
	actx, cancel := context.WithCancel(ctx)
	httpReq, reqErr := http.NewRequestWithContext(actx, http.MethodPost, url, bytes.NewReader(body))
	if reqErr != nil {
		cancel()
		return nil, nil, ir.NewHTTPError(500, reqErr.Error())
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	for k, v := range headers {
		httpReq.Header.Set(k, v)
	}

	resp, doErr := f.client.Do(httpReq)
	if doErr != nil {
		cancel()
		if ctx.Err() != nil {
			return nil, nil, &ir.Error{StatusCode: 499, Type: ir.ErrTypeUpstream, Message: "client disconnected"} // 特殊值：attempt 识别
		}
		return nil, nil, &ir.Error{StatusCode: 502, Type: ir.ErrTypeUpstream, Message: "upstream unreachable: " + doErr.Error(), Retryable: true}
	}
	// kiro 候选的 chunk 间读超时看门狗（须在 defer Close 前装上，
	// 使 defer 关闭的是看门狗 body——停表并关底层连接）。
	if f.kiroStreamingReadTimeout > 0 && cand.acc != nil && cand.acc.Type == account.TypeKiro {
		resp.Body = newIdleTimeoutBody(resp.Body, f.kiroStreamingReadTimeout)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrBody))
		resp.Body.Close()
		cancel()
		e := ir.NewHTTPError(resp.StatusCode, excerpt(string(errBody)))
		if cand.acc != nil && cand.acc.Type == account.TypeKiro {
			e.Reason, _ = account.ParseKiroErrorReason(errBody) // 调度分类用（INVALID_MODEL_ID 等）
		}
		if resp.StatusCode == http.StatusNotFound && cand.acc != nil {
			log.Printf("relay: upstream %s returned 404 — endpoint mismatch? re-probe via POST /admin/accounts/%s/test",
				cand.name, cand.acc.Name)
		}
		return nil, nil, e
	}
	return resp, cancel, nil
}

// attempt 对单个上游做一次转发尝试。wrote 表示是否已向客户端写出字节。
// onUsage 非空时上报响应中的真实 usage（调度模式记账用；估算值不调）。
func (f *Forwarder) attempt(ctx context.Context, w http.ResponseWriter, clientCodec proto.Codec, cand candidate, req *ir.Request, onUsage func(*ir.Usage)) (wrote bool, err *ir.Error) {
	// 上游永远流式
	upReq := req.Clone()
	upReq.Model = cand.native
	upReq.Stream = true
	cand.ov.Apply(upReq) // 账号级请求覆盖（未配置时 no-op）
	if cl, ok := cand.codec.(interface{ ClampThinking(*ir.Request) }); ok {
		cl.ClampThinking(upReq) // 协议级预算归一/夹紧，日志反映实发值
	}
	f.prepareKiroMetadata(cand, upReq) // profileArn 载荷必带 + web_search 注入标志
	if f.paramLog {
		log.Printf("relay: upstream request %s: %s", cand.name, requestParams(cand.codec.Name(), upReq))
	}

	// kiro + 严格 tool_choice：缓冲-校验-恢复重发（其余协议上游原生执行策略）
	if pol := strictToolChoice(upReq); pol != nil && cand.acc != nil && cand.acc.Type == account.TypeKiro {
		return f.attemptStrict(ctx, w, clientCodec, cand, req, upReq, pol, onUsage)
	}

	resp, cancel, uerr := f.openUpstream(ctx, cand, upReq)
	if uerr != nil {
		if uerr.StatusCode == 499 && ctx.Err() != nil {
			return true, nil // 客户端断开
		}
		return false, uerr
	}
	defer resp.Body.Close()
	defer cancel()

	// 已锁定该上游：落有损转换诊断（日志 + 响应头，须在 WriteHeader 前设置）
	if notes := Diagnose(req, cand.codec.Name(), cand.codec.Caps()); len(notes) > 0 {
		log.Printf("relay: upstream %s lossy conversion: %s", cand.name, strings.Join(notes, "; "))
		w.Header().Set("X-Relayd-Notes", strings.Join(notes, "; "))
	}

	// body 形态适配（可选 codec 缝）：kiro 二进制 eventstream -> SSE；
	// 适配过的 body 一律走流式路径。
	var upBody io.Reader = resp.Body
	isSSE := strings.Contains(resp.Header.Get("Content-Type"), "event-stream")
	if bw, ok := cand.codec.(interface{ WrapResponseBody(io.Reader) io.Reader }); ok {
		upBody = bw.WrapResponseBody(resp.Body)
		isSSE = true
	}

	dec := f.newDecoder(cand, req)

	// 兜底：上游忽略 stream=true 返回完整 JSON
	if !isSSE {
		full, readErr := io.ReadAll(upBody)
		if readErr != nil {
			return false, &ir.Error{StatusCode: 502, Type: ir.ErrTypeUpstream, Message: readErr.Error(), Retryable: true}
		}
		irResp, decErr := cand.codec.DecodeResponse(full)
		if decErr != nil {
			return false, &ir.Error{StatusCode: 502, Type: ir.ErrTypeUpstream, Message: "decode upstream response: " + decErr.Error(), Retryable: true}
		}
		f.estimateUsageOnResponse(req, irResp, cand.name)
		if onUsage != nil {
			onUsage(&irResp.Usage)
		}
		sum := newRespSummarizer(f.paramLog, cand.name, "json")
		sum.fill(irResp)
		sum.log()
		csum := newClientSummarizer(f.paramLog, clientCodec.Name(), req.Stream)
		csum.fill(irResp)
		csum.wrote(writeResponse(w, clientCodec, irResp, req.Stream))
		csum.log()
		return true, nil
	}

	if req.Stream {
		return f.streamUpstreamToClient(ctx, cancel, w, clientCodec, cand, req, dec, upBody, onUsage)
	}
	return f.collectUpstreamToClient(ctx, cancel, w, clientCodec, cand, req, dec, upBody, onUsage)
}

// newDecoder 构造上游流解码器；kiro 解码器注入模型名（message_start 回显）、
// 输入上限（context_usage -> input 换算）与账号级 fake_reasoning。
// 注入走可选接口，relay 不依赖具体类型。
func (f *Forwarder) newDecoder(cand candidate, req *ir.Request) proto.StreamDecoder {
	dec := cand.codec.NewStreamDecoder()
	if cand.acc == nil || cand.acc.Type != account.TypeKiro {
		return dec
	}
	if sd, ok := dec.(interface{ SetModel(string) }); ok {
		sd.SetModel(req.Model)
	}
	if cand.acc.Kiro != nil && cand.acc.Kiro.FakeReasoning {
		if sd, ok := dec.(interface{ SetFakeReasoning(bool) }); ok {
			sd.SetFakeReasoning(true)
		}
	}
	if f.sched != nil {
		if rt := f.sched.KiroRuntimeOf(cand.acc.Name); rt != nil {
			if sd, ok := dec.(interface{ SetMaxInputTokens(int) }); ok {
				sd.SetMaxInputTokens(int(rt.Models.MaxInputTokens(cand.native)))
			}
		}
	}
	return dec
}

// recordTruncation 流结束后探测解码器的截断上报缝并记录（kiro 实现）。
func (f *Forwarder) recordTruncation(dec proto.StreamDecoder, upName string) {
	tr, ok := dec.(proto.TruncationReporter)
	if !ok {
		return
	}
	f.trunc.Record(upName, tr.TruncatedTools(), tr.ContentTruncated(), tr.TruncatedContent())
}

// candidateFirstTokenTimeout 某候选等上游首事件的有效超时：
// kiro 候选配置了专用值则覆盖基数；再按请求 effort 档位放大
// （高档位推理推迟首字节，避免昂贵推理请求被提前判卡重试）。
func (f *Forwarder) candidateFirstTokenTimeout(cand candidate, req *ir.Request) time.Duration {
	base := f.firstTokenTimeout
	if f.kiroFirstTokenTimeout > 0 && cand.acc != nil && cand.acc.Type == account.TypeKiro {
		base = f.kiroFirstTokenTimeout
	}
	effort := ""
	if req != nil && req.Thinking != nil {
		effort = req.Thinking.Effort
	}
	return kiro.EffortFirstTokenTimeout(base, cand.native, effort)
}

// awaitFirstEvent 等上游的第一个 SSE 事件；超时则取消本次请求并返回可重试错误。
// 参考 kiro-gateway stream_with_first_token_retry：建连成功不代表上游健康，
// 迟迟不出首 chunk 应视为失败换上游。
func (f *Forwarder) awaitFirstEvent(er *EventReader, cancel context.CancelFunc, timeout time.Duration) (SSEEvent, bool, *ir.Error) {
	type result struct {
		ev  SSEEvent
		ok  bool
		err error
	}
	ch := make(chan result, 1)
	go func() {
		ev, ok, err := er.Next()
		ch <- result{ev, ok, err}
	}()
	wrapErr := func(err error) *ir.Error {
		return &ir.Error{StatusCode: 502, Type: ir.ErrTypeUpstream, Message: "upstream stream read: " + err.Error(), Retryable: true}
	}
	if timeout <= 0 {
		r := <-ch
		if r.err != nil {
			return SSEEvent{}, false, wrapErr(r.err)
		}
		return r.ev, r.ok, nil
	}
	select {
	case r := <-ch:
		if r.err != nil {
			return SSEEvent{}, false, wrapErr(r.err)
		}
		return r.ev, r.ok, nil
	case <-time.After(timeout):
		cancel() // 杀掉阻塞中的 body 读取
		<-ch     // 等读取 goroutine 退出，避免泄露
		return SSEEvent{}, false, &ir.Error{
			StatusCode: 504, Type: ir.ErrTypeUpstream,
			Message:   fmt.Sprintf("upstream produced no event within %s", timeout),
			Retryable: true,
		}
	}
}

// streamUpstreamToClient 上游 SSE -> IR 事件 -> 客户端 SSE，逐 chunk 透传转换。
func (f *Forwarder) streamUpstreamToClient(ctx context.Context, cancel context.CancelFunc, w http.ResponseWriter, clientCodec proto.Codec, cand candidate, req *ir.Request, dec proto.StreamDecoder, body io.Reader, onUsage func(*ir.Usage)) (bool, *ir.Error) {
	er := NewEventReader(body)
	first, ok, firstErr := f.awaitFirstEvent(er, cancel, f.candidateFirstTokenTimeout(cand, req))
	if firstErr != nil {
		return false, firstErr
	}
	if !ok {
		return false, &ir.Error{StatusCode: 502, Type: ir.ErrTypeUpstream, Message: "upstream closed stream without any event", Retryable: true}
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(200)
	flush, _ := w.(http.Flusher)

	enc := clientCodec.NewStreamEncoder()
	var outText strings.Builder
	var startUsage ir.Usage // message_start 携带的 input/cache 用量，记账时与 delta 合并
	sum := newRespSummarizer(f.paramLog, cand.name, "sse")
	csum := newClientSummarizer(f.paramLog, clientCodec.Name(), true)
	emit := func(events []ir.Event) bool {
		for _, ev := range events {
			if ev.Type == ir.EvMessageStart && ev.Usage != nil {
				startUsage = *ev.Usage
			}
			ev = f.estimateUsageOnEvent(req, &outText, ev, cand.name)
			if onUsage != nil && ev.Type == ir.EvMessageDelta && ev.Usage != nil {
				merged := startUsage
				merged.MergeNonZero(*ev.Usage)
				onUsage(&merged)
			}
			sum.observe(ev)
			csum.observe(ev)
			frames, err := enc.Encode(ev)
			if err != nil {
				csum.encErr()
				frames = [][]byte{clientCodec.RenderStreamError(&ir.Error{Type: ir.ErrTypeUpstream, Message: err.Error()})}
			}
			csum.framesAdd(len(frames))
			for _, fr := range frames {
				n, err := w.Write(fr)
				csum.wrote(n)
				if err != nil {
					return false // 客户端断开
				}
			}
			if flush != nil {
				flush.Flush()
			}
		}
		return true
	}

	feed := func(ev SSEEvent) bool {
		events, err := dec.Feed(ev.Event, ev.Data)
		if err != nil {
			events = []ir.Event{{Type: ir.EvError, Err: &ir.Error{Type: ir.ErrTypeUpstream, Message: "upstream stream decode: " + err.Error()}}}
		}
		return emit(events)
	}

	if !feed(first) {
		sum.log()  // 客户端断开，流已中断——按已发部分出摘要
		csum.log()
		return true, nil
	}
	for {
		ev, ok, rerr := er.Next()
		if rerr != nil {
			emit([]ir.Event{{Type: ir.EvError, Err: &ir.Error{Type: ir.ErrTypeUpstream, Message: "upstream stream read: " + rerr.Error()}}})
			break
		}
		if !ok {
			break
		}
		if !feed(ev) {
			sum.log()  // 客户端断开，流已中断——按已发部分出摘要
			csum.log()
			return true, nil
		}
	}
	emit(f.interceptWebSearch(ctx, cand, req, dec.Finish()))
	f.recordTruncation(dec, cand.name)
	fin := enc.Finish()
	csum.framesAdd(len(fin))
	for _, fr := range fin {
		n, _ := w.Write(fr)
		csum.wrote(n)
	}
	if flush != nil {
		flush.Flush()
	}
	sum.log()
	csum.log()
	return true, nil
}

// collectUpstreamToClient 聚合上游流，向非流式客户端一次性返回完整 JSON。
// 聚合期间不向客户端写任何字节，因此聚合失败仍可换上游重试
// （代价是失败上游可能已计费——pre-write 重试的固有取舍）。
func (f *Forwarder) collectUpstreamToClient(ctx context.Context, cancel context.CancelFunc, w http.ResponseWriter, clientCodec proto.Codec, cand candidate, req *ir.Request, dec proto.StreamDecoder, body io.Reader, onUsage func(*ir.Usage)) (bool, *ir.Error) {
	resp, aerr := f.aggregateUpstream(ctx, cand, req, dec, body, cancel)
	if aerr != nil {
		return false, aerr
	}
	f.estimateUsageOnResponse(req, resp, cand.name)
	if onUsage != nil {
		onUsage(&resp.Usage)
	}
	sum := newRespSummarizer(f.paramLog, cand.name, "sse")
	sum.fill(resp)
	sum.log()
	csum := newClientSummarizer(f.paramLog, clientCodec.Name(), false)
	csum.fill(resp)
	csum.wrote(writeResponse(w, clientCodec, resp, false))
	csum.log()
	return true, nil
}

// aggregateUpstream 消费上游流并聚合为完整 IR 响应：首事件超时、
// web_search 拦截（先于调用方可能紧跟的严格工具校验）、截断上报。
// 不向客户端写任何字节。聚合失败强制可重试（未写字节可换上游重发）。
// cancel 供首事件超时杀掉阻塞中的 body 读取（openUpstream 返回的）。
func (f *Forwarder) aggregateUpstream(ctx context.Context, cand candidate, req *ir.Request, dec proto.StreamDecoder, body io.Reader, cancel context.CancelFunc) (*ir.Response, *ir.Error) {
	er := NewEventReader(body)
	first, ok, firstErr := f.awaitFirstEvent(er, cancel, f.candidateFirstTokenTimeout(cand, req))
	if firstErr != nil {
		return nil, firstErr
	}
	if !ok {
		return nil, &ir.Error{StatusCode: 502, Type: ir.ErrTypeUpstream, Message: "upstream closed stream without any event", Retryable: true}
	}

	agg := ir.NewAggregator()
	feed := func(ev SSEEvent) {
		events, err := dec.Feed(ev.Event, ev.Data)
		if err != nil {
			events = []ir.Event{{Type: ir.EvError, Err: &ir.Error{Type: ir.ErrTypeUpstream, Message: "upstream stream decode: " + err.Error()}}}
		}
		for _, e := range events {
			agg.Feed(e)
		}
	}
	feed(first)
	for {
		ev, ok, rerr := er.Next()
		if rerr != nil {
			agg.Feed(ir.Event{Type: ir.EvError, Err: &ir.Error{Type: ir.ErrTypeUpstream, Message: "upstream stream read: " + rerr.Error()}})
			break
		}
		if !ok {
			break
		}
		feed(ev)
	}
	for _, e := range f.interceptWebSearch(ctx, cand, req, dec.Finish()) {
		agg.Feed(e)
	}
	f.recordTruncation(dec, cand.name)
	resp, aggErr := agg.Finish()
	if aggErr != nil {
		// 未写任何字节：强制可重试，换上游重发
		aggErr.Retryable = true
		return nil, aggErr
	}
	return resp, nil
}

// attemptStrict kiro + 严格 tool_choice 的缓冲-校验-恢复重发路径：
// 聚合整个上游响应（不写客户端），先 web_search 拦截再按策略校验；
// 违规克隆请求向最后 user 消息追加恢复指令、同候选重发一次；
// 再违规 502 tool_choice_not_satisfied。校验通过才向客户端写出
// （流式客户端经 EventsFromResponse 重放，REPLAY 前禁止 WriteHeader）；
// usage 只记最终 attempt。
func (f *Forwarder) attemptStrict(ctx context.Context, w http.ResponseWriter, clientCodec proto.Codec, cand candidate, req *ir.Request, upReq *ir.Request, policy *ir.ToolChoice, onUsage func(*ir.Usage)) (bool, *ir.Error) {
	var viol *toolViolation
	for attempt := 0; ; attempt++ {
		if viol != nil {
			upReq = appendRecoveryDirective(upReq, viol, policy)
		}
		resp, cancel, uerr := f.openUpstream(ctx, cand, upReq)
		if uerr != nil {
			if uerr.StatusCode == 499 && ctx.Err() != nil {
				return true, nil // 客户端断开
			}
			return false, uerr
		}
		var upBody io.Reader = resp.Body
		if bw, ok := cand.codec.(interface{ WrapResponseBody(io.Reader) io.Reader }); ok {
			upBody = bw.WrapResponseBody(resp.Body)
		}
		dec := f.newDecoder(cand, req)
		irResp, aerr := f.aggregateUpstream(ctx, cand, req, dec, upBody, cancel)
		resp.Body.Close()
		cancel()
		if aerr != nil {
			return false, aerr
		}
		if v := validateToolChoice(irResp, policy); v != nil {
			if attempt == 1 {
				log.Printf("relay: account %s strict tool_choice failed twice: %s", cand.name, v.msg)
				return false, &ir.Error{StatusCode: 502, Type: ir.ErrTypeUpstream, Message: "tool_choice_not_satisfied: " + v.msg}
			}
			log.Printf("relay: account %s strict tool_choice violated, retrying once with recovery directive: %s", cand.name, v.msg)
			viol = v
			continue
		}
		if f.paramLog {
			log.Printf("relay: upstream request %s: %s", cand.name, requestParams(cand.codec.Name(), upReq))
		}
		f.estimateUsageOnResponse(req, irResp, cand.name)
		if onUsage != nil {
			onUsage(&irResp.Usage)
		}
		csum := newClientSummarizer(f.paramLog, clientCodec.Name(), req.Stream)
		csum.fill(irResp)
		csum.wrote(writeResponse(w, clientCodec, irResp, req.Stream))
		csum.log()
		return true, nil
	}
}

// CountTokens 处理 Anthropic count_tokens 请求：优先转发给 anthropic 账号
// 原生计数；无可用账号时本地粗估并记日志。粗估按首个候选的协议语义
// （kiro 账号 → kiro tokenizer；其余 → IR 通用估算）。
func (f *Forwarder) CountTokens(ctx context.Context, req *ir.Request) (int, []byte) {
	var cands []candidate
	tried := map[string]bool{}
	for {
		acc, ok := f.sched.Next(req.Model, tried)
		if !ok {
			break
		}
		tried[acc.Name] = true
		if cand, err := f.accountCandidate(acc, req.Model); err == nil {
			cands = append(cands, cand)
		}
	}
	for _, cand := range cands {
		if cand.protocol != "anthropic" {
			continue
		}
		upReq := req.Clone()
		upReq.Model = cand.native
		upReq.Stream = false
		body, err := cand.codec.EncodeRequest(upReq)
		if err != nil {
			break
		}
		if f.paramLog {
			log.Printf("relay: upstream request %s (count_tokens): %s", cand.name, requestParams(cand.codec.Name(), upReq))
		}
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
			cand.acc.BaseURL+"/v1/messages/count_tokens", bytes.NewReader(body))
		if err != nil {
			break
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("x-api-key", cand.acc.APIKey)
		httpReq.Header.Set("anthropic-version", "2023-06-01")
		resp, err := f.client.Do(httpReq)
		if err != nil {
			log.Printf("relay: count_tokens upstream %s unreachable: %v", cand.name, err)
			continue
		}
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return resp.StatusCode, respBody
		}
		log.Printf("relay: count_tokens upstream %s returned %d: %s", cand.name, resp.StatusCode, excerpt(string(respBody)))
	}
	est := ir.EstimateRequestTokens(req)
	// 协议语义估算缝：kiro codec 实现按 kiro tokenizer 语义估算
	// （tiktoken 结构 × 1.15 Claude 修正），其余协议用 IR 通用估算。
	for _, cand := range cands {
		if te, ok := cand.codec.(interface {
			EstimateRequestTokens(*ir.Request) int
		}); ok {
			est = te.EstimateRequestTokens(req)
			break
		}
	}
	log.Printf("relay: count_tokens estimated locally: %d tokens", est)
	return 200, []byte(fmt.Sprintf(`{"input_tokens":%d}`, est))
}

// estimateUsageOnResponse 上游未给 usage 且开启估算时，按请求/响应文本粗估。
func (f *Forwarder) estimateUsageOnResponse(req *ir.Request, resp *ir.Response, upName string) {
	if !f.estimateUsage {
		return
	}
	if resp.Usage.TotalInput()+resp.Usage.OutputTokens != 0 {
		return
	}
	var sb strings.Builder
	for _, b := range resp.Content {
		switch b.Type {
		case ir.BlockText:
			sb.WriteString(b.Text)
		case ir.BlockThinking:
			if b.Thinking != nil {
				sb.WriteString(b.Thinking.Text)
			}
		case ir.BlockToolUse:
			if b.ToolUse != nil {
				sb.Write(b.ToolUse.Input)
			}
		}
	}
	resp.Usage = ir.Usage{
		InputTokens:  ir.EstimateRequestTokens(req),
		OutputTokens: ir.EstimateTokens(sb.String()),
		Estimated:    true,
	}
	log.Printf("relay: upstream %s reported no usage; estimated in=%d out=%d", upName, resp.Usage.InputTokens, resp.Usage.OutputTokens)
}

// estimateUsageOnEvent 流式路径的 usage 估算：在最终 EvMessageDelta 上补估值。
func (f *Forwarder) estimateUsageOnEvent(req *ir.Request, outText *strings.Builder, ev ir.Event, upName string) ir.Event {
	switch ev.Type {
	case ir.EvTextDelta, ir.EvThinkingDelta, ir.EvToolInput:
		outText.WriteString(ev.Text)
	case ir.EvMessageDelta:
		if !f.estimateUsage {
			return ev
		}
		if ev.Usage != nil && ev.Usage.TotalInput()+ev.Usage.OutputTokens != 0 {
			return ev
		}
		ev.Usage = &ir.Usage{
			InputTokens:  ir.EstimateRequestTokens(req),
			OutputTokens: ir.EstimateTokens(outText.String()),
			Estimated:    true,
		}
		log.Printf("relay: upstream %s reported no usage; estimated in=%d out=%d", upName, ev.Usage.InputTokens, ev.Usage.OutputTokens)
	}
	return ev
}

// writeResponse 非流式输出；clientStream 为 true 时（上游返回了非 SSE 的兜底响应
// 而客户端要流式）把完整响应合成为一次性事件流。返回写出字节数。
func writeResponse(w http.ResponseWriter, clientCodec proto.Codec, resp *ir.Response, clientStream bool) int {
	if !clientStream {
		body, err := clientCodec.EncodeResponse(resp)
		if err != nil {
			writeError(w, clientCodec, ir.NewHTTPError(500, "encode response: "+err.Error()))
			return 0
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		n, _ := w.Write(body)
		return n
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(200)
	enc := clientCodec.NewStreamEncoder()
	total := 0
	for _, ev := range EventsFromResponse(resp) {
		frames, _ := enc.Encode(ev)
		for _, fr := range frames {
			n, _ := w.Write(fr)
			total += n
		}
	}
	for _, fr := range enc.Finish() {
		n, _ := w.Write(fr)
		total += n
	}
	if flush, ok := w.(http.Flusher); ok {
		flush.Flush()
	}
	return total
}

// EventsFromResponse 把完整响应合成为一次性 IR 事件序列
// （用于"上游返回非流式 JSON 而客户端要 SSE"的兜底路径）。
func EventsFromResponse(resp *ir.Response) []ir.Event {
	events := []ir.Event{{Type: ir.EvMessageStart, MessageID: resp.ID, Model: resp.Model}}
	for i, b := range resp.Content {
		blk := b
		if blk.Type == ir.BlockToolUse && blk.ToolUse != nil {
			input := string(blk.ToolUse.Input)
			blk.ToolUse.Input = nil
			events = append(events,
				ir.Event{Type: ir.EvBlockStart, Index: i, Block: &blk},
				ir.Event{Type: ir.EvToolInput, Index: i, Text: input},
				ir.Event{Type: ir.EvBlockStop, Index: i})
			continue
		}
		// 全块透传（server_tool_use / web_search_tool_result 的载荷在块上）
		events = append(events, ir.Event{Type: ir.EvBlockStart, Index: i, Block: &blk})
		switch blk.Type {
		case ir.BlockText:
			if blk.Text != "" {
				events = append(events, ir.Event{Type: ir.EvTextDelta, Index: i, Text: blk.Text})
			}
		case ir.BlockThinking:
			if blk.Thinking != nil {
				if blk.Thinking.Text != "" {
					events = append(events, ir.Event{Type: ir.EvThinkingDelta, Index: i, Text: blk.Thinking.Text})
				}
				if blk.Thinking.Signature != "" {
					events = append(events, ir.Event{Type: ir.EvSigDelta, Index: i, Text: blk.Thinking.Signature})
				}
			}
		}
		events = append(events, ir.Event{Type: ir.EvBlockStop, Index: i})
	}
	u := resp.Usage
	events = append(events,
		ir.Event{Type: ir.EvMessageDelta, StopReason: resp.StopReason, Usage: &u},
		ir.Event{Type: ir.EvMessageStop})
	return events
}

func writeError(w http.ResponseWriter, clientCodec proto.Codec, e *ir.Error) {
	status, body := clientCodec.RenderError(e)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// excerpt 截取上游错误文本前 500 字符，避免刷屏与泄露。
func excerpt(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 500 {
		s = s[:500] + "..."
	}
	if s == "" {
		s = "upstream error"
	}
	return s
}
