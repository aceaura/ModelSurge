package relay

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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
	"github.com/aceaura/ModelSurge/replay/contract/replayv1"

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
	sameAccountRetries int
	estimateUsage      bool
	replay             Replay
	store              *agentstore.Store
	trunc              *TruncationTracker
	paramLog           bool // 请求/响应参数日志（与访问日志同开关）
}

// NewForwarder is retained for legacy tests. New production composition uses NewRemoteForwarder.
// NewRemoteForwarder constructs the relay data plane without an in-process account Manager.
type Replay interface {
	Dispatch(context.Context, replayv1.DispatchRequest) (replayv1.TargetLease, error)
	Report(context.Context, replayv1.ResultReport) (replayv1.ResultResponse, error)
}

func NewForwarder(cfg *config.Config, replay Replay, store *agentstore.Store) *Forwarder {
	f := newForwarder(cfg)
	f.replay = replay
	f.store = store
	return f
}

func newForwarder(cfg *config.Config) *Forwarder {
	return &Forwarder{
		client:             &http.Client{Timeout: 0},
		firstTokenTimeout:  cfg.FirstTokenTimeoutDur,
		sameAccountRetries: cfg.SameAccountRetries,
		estimateUsage:      cfg.EstimateUsage,
		trunc:              NewTruncationTracker(cfg.TruncationRecoveryEnabled),
		paramLog:           cfg.AccessLogEnabled,
	}
}

// candidate 一个可服务某 canonical model 的账号候选及其 native 模型名与 codec。
type candidate struct {
	name     string
	protocol string
	native   string
	codec    proto.OutboundCodec
	ov       *ir.Overrides
	resolve  endpointResolver
}

// endpointResolver 解析普通上游请求的 URL 与请求头。
type endpointResolver func(ctx context.Context) (url string, headers map[string]string, err *ir.Error)

// staticEndpoint api-key 账号：URL 与鉴权头固定；extra 为账号级自定义头
// （如网关要求的会话头），同名键覆盖协议默认头。
func staticEndpoint(protocol, baseURL, apiKey, nativeModel string, extra map[string]string) (endpointResolver, error) {
	url, headers, err := endpoint(protocol, baseURL, apiKey, nativeModel)
	if err != nil {
		return nil, err
	}
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
	}, nil
}

// endpoint api-key 账号的上游请求 URL 与鉴权头。
func endpoint(protocol, baseURL, apiKey, nativeModel string) (url string, headers map[string]string, err error) {
	switch protocol {
	case "anthropic":
		return baseURL + "/v1/messages", map[string]string{
			"x-api-key":         apiKey,
			"anthropic-version": "2023-06-01",
		}, nil
	case "openai-chat":
		return baseURL + "/v1/chat/completions", map[string]string{"Authorization": "Bearer " + apiKey}, nil
	case "openai-responses":
		return baseURL + "/v1/responses", map[string]string{"Authorization": "Bearer " + apiKey}, nil
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
		}, nil
	default:
		return "", nil, fmt.Errorf("unsupported outbound protocol %q", protocol)
	}
}

// maxErrBody 上游错误体的读取上限。
const maxErrBody = 4 * 1024

// Forward 执行一次转发。clientCodec 为客户端协议 codec（用于错误渲染与响应编码），
// req.Stream 表示客户端是否要求流式。所有响应直接写入 w。
func (f *Forwarder) Forward(ctx context.Context, w http.ResponseWriter, clientCodec proto.InboundCodec, req *ir.Request, clientKey string) {
	requestID := uuid.NewString()
	ctx = withRequestLog(ctx, requestID, time.Now())
	if f.paramLog {
		log.Printf("agent phase=request_in request_id=%s %s", requestID, requestParams(clientCodec.Name(), req))
	}
	f.trunc.InjectNotices(req) // 上次截断的恢复提示（命中才修改）
	// 客户端要求不回显思考时在入口装一层抑制（五处写出点自动覆盖）。
	clientCodec = withHiddenThoughts(clientCodec, req)
	f.forwardRemote(ctx, w, clientCodec, req, clientKey)
}

// resolvedCandidate 把调度租约转为转发候选：解析出站 codec、静态端点与鉴权头。
func resolvedCandidate(t replayv1.TargetLease) (candidate, error) {
	if !allowedOutboundProtocol(t.Protocol) {
		return candidate{}, fmt.Errorf("unsupported outbound protocol %q", t.Protocol)
	}
	ov := overridesFrom(t)
	c, err := proto.GetOutbound(t.Protocol)
	if err != nil {
		return candidate{}, err
	}
	url, defaultHeaders, err := endpoint(t.Protocol, t.BaseURL, t.Credential, t.NativeModel)
	if err != nil {
		return candidate{}, err
	}
	return candidate{name: t.TargetID, protocol: t.Protocol, native: t.NativeModel, codec: c, ov: ov,
		resolve: func(context.Context) (string, map[string]string, *ir.Error) {
			headers := make(map[string]string, len(t.Headers)+len(defaultHeaders))
			for k, v := range t.Headers {
				headers[k] = v
			}
			for k, v := range defaultHeaders {
				if _, ok := headers[k]; !ok {
					headers[k] = v
				}
			}
			return url, headers, nil
		}}, nil
}

func overridesFrom(t replayv1.TargetLease) *ir.Overrides {
	if t.RequestOverrides == nil {
		return nil
	}
	ov := &ir.Overrides{Temperature: t.RequestOverrides.Temperature, TopP: t.RequestOverrides.TopP, MaxTokens: t.RequestOverrides.MaxTokens}
	if x := t.RequestOverrides.Thinking; x != nil {
		ov.Thinking = &ir.ThinkingOverride{Enabled: x.Enabled, BudgetTokens: x.BudgetTokens, Effort: x.Effort}
	}
	return ov
}

func allowedOutboundProtocol(protocol string) bool {
	switch protocol {
	case "anthropic", "openai-chat", "openai-responses", "codex":
		return true
	default:
		return false
	}
}

func (f *Forwarder) forwardRemote(ctx context.Context, w http.ResponseWriter, clientCodec proto.InboundCodec, req *ir.Request, clientKey string) {
	requestID := requestIDFrom(ctx)
	// 估算输入+输出预算（入站 codec 自带 tokenizer 时用之，CountTokens 同款取法）。
	est := estimateReqTokens(clientCodec, req)
	tried := map[string]bool{}
	attempts := map[string]int{}
	var lastErr *ir.Error
	// 第一档压缩回退状态（循环局部）：compressModel 由 lease / dispatch 错误
	// 信封携带；compactOf 非空 = 已进入内部压缩调用（此后所有 dispatch 都带
	// CompressOf 供 Replay 跳过 key 校验）；compactRetried 永久封顶重试深度。
	compressModel := ""
	compactOf := ""
	compactRetried := false
	// 第二档自动压缩状态（循环局部）：src=原始全量历史快照（每轮压缩都从
	// 原始历史切分），k=保留轮数（2→0），calls=压缩调用数（≤2），
	// header=续命响应标记头。
	var auto autoCompactState
	for {
		if ctx.Err() != nil {
			return
		}
		if f.paramLog {
			log.Printf("agent phase=dispatch_out request_id=%s model=%s proto=%s tried=%d", requestID, req.Model, clientCodec.Name(), len(tried))
		}
		lease, err := f.replay.Dispatch(ctx, replayv1.DispatchRequest{Model: req.Model, InboundProtocol: clientCodec.Name(), ClientKey: clientKey, RequestID: requestID, TriedIDs: triedIDs(tried), EstTokens: est, CompressOf: compactOf})
		if err != nil {
			dispatchErr := replayDispatchError(err)
			// 鉴权通过后的失败信封携带 compress_model（组容灾穷尽/调度超限
			// 都可兜底）；鉴权类失败不携带（4.3：鉴权先于压缩）。
			if e, ok := err.(replayv1.Error); ok && e.CompressModel != "" {
				compressModel = e.CompressModel
			}
			// 重试 dispatch 失败：not_found = compress_model 引用不存在/禁用，
			// 视为未配置并告警，按原失败返回（1.5）；其余按最后一次失败
			// 原样返回（1.4）。
			if compactOf != "" {
				if e, ok := err.(replayv1.Error); ok && e.Code == replayv1.CodeNotFound {
					log.Printf("agent: compact fallback model %q not found or disabled; treated as unconfigured, returning original failure", req.Model)
					f.writeClientError(ctx, w, clientCodec, req.Stream, lastErr)
					return
				}
				f.writeClientError(ctx, w, clientCodec, req.Stream, dispatchErr)
				return
			}
			// 第一档拦截（dispatch 失败路径）：显式压缩请求 + 模型不可用类
			// 失败（组容灾穷尽/窗口过滤超限）+ 配了 compress_model + 未重试。
			if req.Compact && !compactRetried && compressModel != "" && modelUnavailableDispatch(err) {
				lastErr = dispatchErr
				log.Printf("agent phase=compact_fallback request_id=%s trigger=dispatch_unavailable model=%s compress_model=%s", requestID, req.Model, compressModel)
				compactRetried = true
				compactOf = req.Model
				req.Model = compressModel
				tried = map[string]bool{}
				attempts = map[string]int{}
				continue
			}
			// 第二档拦截（dispatch 失败路径）：普通请求被调度层窗口过滤
			// （context_too_large）→ 自动压缩续命（2.1/8.1）。
			if !req.Compact && compactOf == "" && compressModel != "" && isContextTooLargeDispatch(err) {
				if newReq, ok := f.tryAutoCompact(ctx, clientCodec, req, compressModel, clientKey, requestID, &auto); ok {
					req = newReq
					est = estimateReqTokens(clientCodec, req)
					tried = map[string]bool{}
					attempts = map[string]int{}
					continue
				}
				f.writeClientError(ctx, w, clientCodec, req.Stream, dispatchErr)
				return
			}
			if lastErr == nil {
				lastErr = dispatchErr
			}
			f.writeClientError(ctx, w, clientCodec, req.Stream, lastErr)
			return
		}
		if lease.CompressModel != "" {
			compressModel = lease.CompressModel
		}
		if f.paramLog {
			o := lease.RequestOverrides
			log.Printf("agent phase=dispatch_in request_id=%s target=%s group=%s protocol=%s native_model=%s headers=%d override_temperature=%t override_top_p=%t override_max_tokens=%t override_thinking=%t", requestID, lease.TargetID, lease.GroupID, lease.Protocol, lease.NativeModel, len(lease.Headers), o != nil && o.Temperature != nil, o != nil && o.TopP != nil, o != nil && o.MaxTokens != nil, o != nil && o.Thinking != nil)
		}
		cand, err := resolvedCandidate(lease)
		if err != nil {
			tried[lease.TargetID] = true
			lastErr = ir.NewHTTPError(500, "invalid resolved target")
			continue
		}
		for {
			attempt := attempts[lease.TargetID]
			var usage replayv1.Usage
			if auto.header {
				w.Header().Set(headerCompacted, "true") // 第二档续命标记（8.5）
			}
			wrote, aerr := f.attempt(ctx, w, clientCodec, cand, req, func(u *ir.Usage) {
				if u == nil || u.Estimated {
					return
				}
				usage = replayv1.Usage{InputTokens: int64(u.InputTokens), OutputTokens: int64(u.OutputTokens), CacheRead: int64(u.CacheReadTokens), CacheCreation: int64(u.CacheCreationTokens)}
			})
			report := replayv1.ResultReport{ReportID: uuid.NewString(), RequestID: requestID, GroupID: lease.GroupID, TargetID: lease.TargetID, Outcome: "normal", Usage: usage, At: time.Now(), Attempt: attempt}
			if aerr != nil {
				if aerr.Reason == "" && classifyContextError(aerr.StatusCode, aerr.Message, aerr.Code) {
					aerr.Reason = ReasonContextExceeded
				}
				report.Outcome = "abnormal"
				if aerr.Reason == ReasonContextExceeded {
					report.Outcome = ReasonContextExceeded
				}
				report.Status = aerr.StatusCode
				report.Reason = aerr.Reason
				report.Message = excerpt(aerr.Message)
			}
			result := f.finishReport(clientCodec.Name(), req.Model, report)
			if aerr == nil || wrote {
				if aerr == nil && auto.header {
					log.Printf("agent phase=auto_compact request_id=%s result=success model=%s compress_model=%s round=%d", requestID, req.Model, compressModel, auto.calls)
				}
				return
			}
			w.Header().Del(headerCompacted) // 本次尝试未写出任何字节，撤销标记头
			lastErr = aerr
			attempts[lease.TargetID] = attempt + 1
			action := result.Action
			if action == "" {
				action = localResultAction(aerr, attempt, f.sameAccountRetries)
			}
			switch action {
			case replayv1.ActionRetryTarget:
				log.Printf("agent: target %s redispatching in place after %d/%s", lease.TargetID, aerr.StatusCode, aerr.Reason)
			case replayv1.ActionSwitchTarget:
				tried[lease.TargetID] = true
			default:
				// 第一档拦截（action=stop 路径）：显式压缩请求 + context_exceeded
				// + 配了 compress_model + 未重试 → 换模型重发（1.3）。
				if req.Compact && !compactRetried && compactOf == "" && compressModel != "" && aerr.Reason == ReasonContextExceeded {
					log.Printf("agent phase=compact_fallback request_id=%s trigger=context_exceeded model=%s compress_model=%s", requestID, req.Model, compressModel)
					compactRetried = true
					compactOf = req.Model
					req.Model = compressModel
					tried = map[string]bool{}
					attempts = map[string]int{}
					break
				}
				// 第二档拦截（action=stop 路径）：普通请求超限 → 服务端自动
				// 压缩续命（2.1-2.3/8.1-8.3）。
				if !req.Compact && compactOf == "" && compressModel != "" && aerr.Reason == ReasonContextExceeded {
					if newReq, ok := f.tryAutoCompact(ctx, clientCodec, req, compressModel, clientKey, requestID, &auto); ok {
						req = newReq
						est = estimateReqTokens(clientCodec, req)
						tried = map[string]bool{}
						attempts = map[string]int{}
						break
					}
					f.writeClientError(ctx, w, clientCodec, req.Stream, aerr)
					return
				}
				f.writeClientError(ctx, w, clientCodec, req.Stream, aerr)
				return
			}
			break
		}
	}
}

// modelUnavailableDispatch dispatch 错误是否模型不可用类（第一档拦截条件；
// 鉴权/参数错误不触发压缩回退，replay 不可达同理——重试也必失败）。
func modelUnavailableDispatch(err error) bool {
	e, ok := err.(replayv1.Error)
	if !ok {
		return false
	}
	return e.Code == replayv1.CodeTargetUnavailable || e.Code == replayv1.CodeContextTooLarge
}

func localResultAction(err *ir.Error, attempt, sameTargetRetries int) string {
	if err == nil {
		return replayv1.ActionStop
	}
	if err.Reason == ReasonContextExceeded {
		return replayv1.ActionStop
	}
	// 402（余额/配额耗尽）与 429 同样直接换目标：账号没钱不会因为再问一次就有钱，
	// 同目标重试只是白烧一轮。ClassifyStatus 的 default 会把 402 判成不可重试直接
	// stop——同一个欠费上游，池子里明明还有可用账号却放弃了。
	if err.StatusCode == http.StatusTooManyRequests || err.StatusCode == http.StatusPaymentRequired {
		return replayv1.ActionSwitchTarget
	}
	if err.Retryable {
		if attempt < sameTargetRetries {
			return replayv1.ActionRetryTarget
		}
		return replayv1.ActionSwitchTarget
	}
	return replayv1.ActionStop
}

// openUpstream 对普通协议候选发起一次上游请求。
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
	// 客户端的 anthropic-beta 转交上游。放在账号头之后：要与账号级自定义头合并
	// 去重，而不是被它整条顶掉。
	applyClientWireHeaders(ctx, httpReq.Header, cand.protocol)

	resp, doErr := f.client.Do(httpReq)
	if doErr != nil {
		cancel()
		if ctx.Err() != nil {
			return nil, nil, &ir.Error{StatusCode: 499, Type: ir.ErrTypeUpstream, Message: "client disconnected"} // 特殊值：attempt 识别
		}
		return nil, nil, &ir.Error{StatusCode: 502, Type: ir.ErrTypeConnection, Message: "upstream unreachable: " + doErr.Error(), Retryable: true}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrBody))
		retryAfter := resp.Header.Get("Retry-After")
		resp.Body.Close()
		cancel()
		// 日志留原文（诊断要看上游到底吐了什么），客户端拿解析后的消息与错误码：
		// 整段 JSON 塞进 error.message 会让 SDK 读到一坨转义字符串，上游自报的
		// context_length_exceeded / RESOURCE_EXHAUSTED 也只糊在文本里，Code 恒空。
		log.Printf("agent: target %s upstream %d: %s", cand.name, resp.StatusCode, excerpt(string(errBody)))
		if resp.StatusCode == http.StatusNotFound {
			log.Printf("agent: target %s returned 404 (endpoint mismatch)", cand.name)
		}
		e := ir.ParseUpstreamError(resp.StatusCode, errBody)
		e.RetryAfter = sanitizeRetryAfter(retryAfter)
		return nil, nil, e
	}
	return resp, cancel, nil
}

// attempt 对单个上游做一次转发尝试。wrote 表示是否已向客户端写出字节。
// onUsage 非空时上报响应中的真实 usage（调度模式记账用；估算值不调）。
func (f *Forwarder) attempt(ctx context.Context, w http.ResponseWriter, clientCodec proto.InboundCodec, cand candidate, req *ir.Request, onUsage func(*ir.Usage)) (wrote bool, err *ir.Error) {
	// 上游永远流式
	upReq := req.Clone()
	upReq.Model = cand.native
	upReq.Stream = true
	cand.ov.Apply(upReq) // 账号级请求覆盖（未配置时 no-op）
	// 推理风格互补：四个出站各自只读 effort 或 budget 一侧，缺失的那侧若不
	// 补全会被出站的硬编码缺省顶替（客户端要 high 拿到 4096、要 32768 拿到
	// medium）。排在 ov.Apply 之后：账号级覆盖可能改 max_tokens 或指定某一侧，
	// 换算要基于实发值。排在 ClampThinking 之前：补出来的预算同样要受协议夹紧。
	thinkNotes := ir.CompleteThinking(upReq)
	if cl, ok := cand.codec.(interface{ ClampThinking(*ir.Request) }); ok {
		cl.ClampThinking(upReq) // 协议级预算归一/夹紧，日志反映实发值
	}
	if f.paramLog {
		body, _ := cand.codec.EncodeRequest(upReq)
		log.Printf("agent phase=upstream_out request_id=%s target=%s proto=%s %s body_bytes=%d", requestIDFrom(ctx), cand.name, cand.codec.Name(), requestParams(cand.codec.Name(), upReq), len(body))
	}

	upstreamStarted := time.Now()
	resp, cancel, uerr := f.openUpstream(ctx, cand, upReq)
	if f.paramLog {
		status := 0
		contentType := ""
		if resp != nil {
			status = resp.StatusCode
			contentType = resp.Header.Get("Content-Type")
		}
		log.Printf("agent phase=upstream_in request_id=%s target=%s proto=%s status=%d content_type=%s latency=%s error=%t", requestIDFrom(ctx), cand.name, cand.codec.Name(), status, contentType, time.Since(upstreamStarted), uerr != nil)
	}
	if uerr != nil {
		if uerr.StatusCode == 499 && ctx.Err() != nil {
			return true, nil // 客户端断开
		}
		return false, uerr
	}
	defer resp.Body.Close()
	defer cancel()

	// 已锁定该上游：落有损转换诊断（日志 + 响应头，须在 WriteHeader 前设置）
	notes := Diagnose(req, cand.codec.Name(), cand.codec.Caps())
	// 推理风格换算的说明来自 upReq（每个目标的 max_tokens 覆盖不同，档位换算
	// 结果也不同），Diagnose 看的是客户端原请求，两者只能在此处汇合。
	notes = append(notes, thinkNotes...)
	writeLossyNotes(w, cand.name, notes)
	// 上游限流头与 request id 回传。放在这里而不是各写出分支里：此处 resp 还在手、
	// 客户端一个字节都没写，且下游三条路（SSE 流、聚合转流、兜底 JSON）共用。
	forwardUpstreamHeaders(w, resp)

	isSSE := strings.Contains(resp.Header.Get("Content-Type"), "event-stream")

	dec := f.newDecoder(cand, req)

	// 兜底：上游忽略 stream=true 返回完整 JSON
	if !isSSE {
		full, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			return false, &ir.Error{StatusCode: 502, Type: ir.ErrTypeConnection, Message: readErr.Error(), Retryable: true}
		}
		var irResp *ir.Response
		var respNotes []string
		var decErr error
		if dec, ok := cand.codec.(proto.ResponseDecoderWithNotes); ok {
			irResp, respNotes, decErr = dec.DecodeResponseWithNotes(full)
		} else {
			irResp, decErr = cand.codec.DecodeResponse(full)
		}
		if decErr != nil {
			// 解码器返回的 *ir.Error 已带规范类型/错误码/可重试判定（如
			// status=failed 的上游响应），原样上交；只有裸 error 才按
			// 传输失败包成 connection_error。
			var ie *ir.Error
			if errors.As(decErr, &ie) {
				return false, ie
			}
			return false, &ir.Error{StatusCode: 502, Type: ir.ErrTypeConnection, Message: "decode upstream response: " + decErr.Error(), Retryable: true}
		}
		f.estimateUsageOnResponse(req, irResp, cand.name)
		if onUsage != nil {
			onUsage(&irResp.Usage)
		}
		sum := newRespSummarizer(f.paramLog, requestIDFrom(ctx), cand.name, "json", requestLogFrom(ctx).started)
		sum.fill(irResp)
		sum.log()
		csum := newClientSummarizer(f.paramLog, requestIDFrom(ctx), clientCodec.Name(), req.Stream, requestLogFrom(ctx).started)
		csum.fill(irResp)
		writeResponse(w, clientCodec, req, irResp, req.Stream, respNotes, csum)
		csum.log()
		return true, nil
	}

	if req.Stream {
		return f.streamUpstreamToClient(ctx, cancel, w, clientCodec, cand, req, dec, resp.Body, onUsage)
	}
	return f.collectUpstreamToClient(ctx, cancel, w, clientCodec, cand, req, dec, resp.Body, onUsage)
}

func (f *Forwarder) newDecoder(cand candidate, _ *ir.Request) proto.StreamDecoder {
	return cand.codec.NewStreamDecoder()
}

// recordTruncation 流结束后探测解码器的截断上报缝并记录（可选实现）。
func (f *Forwarder) recordTruncation(dec proto.StreamDecoder, upName string) {
	tr, ok := dec.(proto.TruncationReporter)
	if !ok {
		return
	}
	f.trunc.Record(upName, tr.TruncatedTools(), tr.ContentTruncated(), tr.TruncatedContent())
}

func decoderNotes(dec proto.StreamDecoder) []string {
	if reporter, ok := dec.(proto.DecoderNoteReporter); ok {
		return reporter.Notes()
	}
	return nil
}

func (f *Forwarder) candidateFirstTokenTimeout(candidate, *ir.Request) time.Duration {
	return f.firstTokenTimeout
}

// awaitFirstEvent 等上游的第一个 SSE 事件；超时则取消本次请求并返回可重试错误。
// 建连成功不代表上游健康，迟迟不出首 chunk 应视为失败换上游。
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
		return &ir.Error{StatusCode: 502, Type: ir.ErrTypeConnection, Message: "upstream stream read: " + err.Error(), Retryable: true}
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
			StatusCode: 504, Type: ir.ErrTypeConnection,
			Message:   fmt.Sprintf("upstream produced no event within %s", timeout),
			Retryable: true,
		}
	}
}

// streamUpstreamToClient 上游 SSE -> IR 事件 -> 客户端 SSE，逐 chunk 透传转换。
func (f *Forwarder) streamUpstreamToClient(ctx context.Context, cancel context.CancelFunc, w http.ResponseWriter, clientCodec proto.InboundCodec, cand candidate, req *ir.Request, dec proto.StreamDecoder, body io.Reader, onUsage func(*ir.Usage)) (bool, *ir.Error) {
	er := NewEventReader(body)
	first, ok, firstErr := f.awaitFirstEvent(er, cancel, f.candidateFirstTokenTimeout(cand, req))
	if firstErr != nil {
		return false, firstErr
	}
	if !ok {
		return false, &ir.Error{StatusCode: 502, Type: ir.ErrTypeConnection, Message: "upstream closed stream without any event", Retryable: true}
	}

	firstEvents, err := dec.Feed(first.Event, first.Data)
	if err != nil {
		return false, &ir.Error{StatusCode: 502, Type: ir.ErrTypeConnection, Message: "upstream first event decode: " + err.Error(), Retryable: true}
	}
	if len(firstEvents) == 0 {
		return false, &ir.Error{StatusCode: 502, Type: ir.ErrTypeConnection, Message: "upstream first event produced no IR event", Retryable: true}
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// 反向代理默认缓冲响应体，SSE 会被攒到流结束一次性吐出，客户端看到的就成了
	// 「流式变非流式」。这个头是 nginx 一族认的关闭缓冲开关。
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	flush, _ := w.(http.Flusher)

	enc := proto.NewClientStreamEncoder(clientCodec, req)
	var outText strings.Builder
	var startUsage ir.Usage // message_start 携带的 input/cache 用量，记账时与 delta 合并
	sum := newRespSummarizer(f.paramLog, requestIDFrom(ctx), cand.name, "sse", requestLogFrom(ctx).started)
	csum := newClientSummarizer(f.paramLog, requestIDFrom(ctx), clientCodec.Name(), true, requestLogFrom(ctx).started)
	// 编码失败的错误帧也是终止帧：置位后后续事件一律不再下发，enc.Finish()
	// 的收尾帧同样跳过——否则错误帧之后还会漏出正文/finish 帧。
	terminalErr := false
	emit := func(events []ir.Event) bool {
		for _, ev := range events {
			if terminalErr {
				continue
			}
			if ev.Type == ir.EvMessageStart && ev.Usage != nil {
				startUsage = *ev.Usage
			}
			ev = f.estimateUsageOnEvent(req, &outText, ev, cand.name)
			if onUsage != nil && ev.Type == ir.EvMessageDelta {
				merged := startUsage
				if ev.Usage != nil {
					merged.MergeNonZero(*ev.Usage)
				}
				if ev.Usage != nil || merged.TotalInput()+merged.OutputTokens != 0 {
					onUsage(&merged)
				}
			}
			sum.observe(ev)
			csum.observe(ev)
			frames, err := enc.Encode(ev)
			if err != nil {
				csum.encErr()
				frames = [][]byte{clientCodec.RenderStreamError(&ir.Error{Type: ir.ErrTypeUpstream, Message: err.Error()})}
				terminalErr = true
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

	var skippedFrames int
	feed := func(ev SSEEvent) bool {
		events, err := dec.Feed(ev.Event, ev.Data)
		if err != nil {
			// 单帧畸形（如某条 data 不是合法 JSON）不能升级成 EvError 发出去
			// 还继续转：错误帧在客户端是终止语义（chat 族还会自带 [DONE]），
			// 之后再到正文字节就是「[DONE] 之后又收到 data」的非法流。sub2api
			// 同款口径：跳过坏帧、流不断。损耗计数在收尾注记里报出。
			skippedFrames++
			return true
		}
		return emit(events)
	}

	if !emit(firstEvents) {
		sum.log() // 客户端断开，流已中断——按已发部分出摘要
		csum.log()
		return true, nil
	}
	for {
		ev, ok, rerr := er.Next()
		if rerr != nil {
			emit([]ir.Event{{Type: ir.EvError, Err: &ir.Error{Type: ir.ErrTypeConnection, Message: "upstream stream read: " + rerr.Error()}}})
			break
		}
		if !ok {
			break
		}
		if !feed(ev) {
			sum.log() // 客户端断开，流已中断——按已发部分出摘要
			csum.log()
			return true, nil
		}
	}
	emit(dec.Finish())
	f.recordTruncation(dec, cand.name)
	if !terminalErr {
		fin := enc.Finish()
		csum.framesAdd(len(fin))
		for _, fr := range fin {
			n, _ := w.Write(fr)
			csum.wrote(n)
		}
	}
	// 响应侧损耗收尾：流已开始，头写不了，落 SSE 注释帧 + 日志。
	respNotes := decoderNotes(dec)
	respNotes = append(respNotes, enc.Notes()...)
	if skippedFrames > 0 {
		respNotes = append(respNotes, fmt.Sprintf("skipped %d malformed upstream SSE frame(s); their content is lost to the receiving side", skippedFrames))
	}
	logRespNotes(clientCodec.Name(), respNotes)
	for _, fr := range proto.SSENoteFrames(respNotes) {
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
func (f *Forwarder) collectUpstreamToClient(ctx context.Context, cancel context.CancelFunc, w http.ResponseWriter, clientCodec proto.InboundCodec, cand candidate, req *ir.Request, dec proto.StreamDecoder, body io.Reader, onUsage func(*ir.Usage)) (bool, *ir.Error) {
	resp, aggNotes, aerr := f.aggregateUpstream(ctx, cand, req, dec, body, cancel)
	if aerr != nil {
		return false, aerr
	}
	f.estimateUsageOnResponse(req, resp, cand.name)
	if onUsage != nil {
		onUsage(&resp.Usage)
	}
	sum := newRespSummarizer(f.paramLog, requestIDFrom(ctx), cand.name, "sse", requestLogFrom(ctx).started)
	sum.fill(resp)
	sum.log()
	csum := newClientSummarizer(f.paramLog, requestIDFrom(ctx), clientCodec.Name(), false, requestLogFrom(ctx).started)
	csum.fill(resp)
	writeResponse(w, clientCodec, req, resp, false, aggNotes, csum)
	csum.log()
	return true, nil
}

// aggregateUpstream 消费上游流并聚合为完整 IR 响应：首事件超时、
// web_search 拦截（先于调用方可能紧跟的严格工具校验）、截断上报。
// 不向客户端写任何字节。聚合失败的可重试性按规范类型判（未写字节可换上游重发，
// 但换谁都会被同样拒绝的错误不重试）。
// cancel 供首事件超时杀掉阻塞中的 body 读取（openUpstream 返回的）。
// 第二个返回值是聚合期损耗注记（畸形工具参数挪键）——挪键发生在客户端
// 编码之前，编码器扫描响应体已看不出，必须由这里带出去。
func (f *Forwarder) aggregateUpstream(ctx context.Context, cand candidate, req *ir.Request, dec proto.StreamDecoder, body io.Reader, cancel context.CancelFunc) (*ir.Response, []string, *ir.Error) {
	er := NewEventReader(body)
	first, ok, firstErr := f.awaitFirstEvent(er, cancel, f.candidateFirstTokenTimeout(cand, req))
	if firstErr != nil {
		return nil, nil, firstErr
	}
	if !ok {
		return nil, nil, &ir.Error{StatusCode: 502, Type: ir.ErrTypeConnection, Message: "upstream closed stream without any event", Retryable: true}
	}

	agg := ir.NewAggregator()
	firstEvents, err := dec.Feed(first.Event, first.Data)
	if err != nil {
		return nil, nil, &ir.Error{StatusCode: 502, Type: ir.ErrTypeConnection, Message: "upstream first event decode: " + err.Error(), Retryable: true}
	}
	if len(firstEvents) == 0 {
		return nil, nil, &ir.Error{StatusCode: 502, Type: ir.ErrTypeConnection, Message: "upstream first event produced no IR event", Retryable: true}
	}
	for _, e := range firstEvents {
		agg.Feed(e)
	}
	var skippedFrames int
	feed := func(ev SSEEvent) {
		events, err := dec.Feed(ev.Event, ev.Data)
		if err != nil {
			// 与流式路径同口径（R103/sub2api 缓冲路径同款 continue）：单帧畸形
			// 跳帧续流、计数收尾报出；升级成 EvError 会让整轮失败并换账号重发
			// 一份上游垃圾——同一份垃圾，流式客户端丢一帧，非流式客户端 502。
			skippedFrames++
			return
		}
		for _, e := range events {
			agg.Feed(e)
		}
	}
	for {
		ev, ok, rerr := er.Next()
		if rerr != nil {
			agg.Feed(ir.Event{Type: ir.EvError, Err: &ir.Error{Type: ir.ErrTypeConnection, Message: "upstream stream read: " + rerr.Error()}})
			break
		}
		if !ok {
			break
		}
		feed(ev)
	}
	for _, e := range dec.Finish() {
		agg.Feed(e)
	}
	f.recordTruncation(dec, cand.name)
	resp, aggErr := agg.Finish()
	if aggErr != nil {
		// 未写任何字节，换上游重发是**安全**的——但安全不等于有用。可重试性按规范
		// 类型判（与解码器共用 StreamRetryable 同一张表），不再一律置真：非法请求 /
		// 未找到 / 内容过滤换谁都会被同样拒绝，强制可重试只会把账号池白烧一遍
		// （实测一个 invalid_request 打满全部目标才停下）。传输与解码失败是
		// connection_error，照旧可重试，换账号确实可能成功；upstream_error 只承载
		// 未映射状态码（多为客户端请求自身造成），不可重试。
		aggErr.Retryable = ir.StreamRetryable(aggErr.Type)
		return nil, nil, aggErr
	}
	notes := decoderNotes(dec)
	notes = append(notes, agg.Notes()...)
	if skippedFrames > 0 {
		notes = append(notes, fmt.Sprintf("skipped %d malformed upstream SSE frame(s); their content is lost to the receiving side", skippedFrames))
	}
	return resp, notes, nil
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
// 而客户端要流式）把完整响应合成为一次性事件流。respNotes 是聚合阶段已记下
// 的响应侧损耗（聚合器把畸形参数挪键发生在编码之前，扫响应体已看不出来）。
// req 只在 clientStream 那条路上用到：合成事件流时客户端的呈现意图（要不要
// usage 帧）得跟着走，非流式路径可以为 nil。
func writeResponse(w http.ResponseWriter, clientCodec proto.InboundCodec, req *ir.Request, resp *ir.Response, clientStream bool, respNotes []string, summary ...*clientSummarizer) {
	var clientSummary *clientSummarizer
	if len(summary) > 0 {
		clientSummary = summary[0]
	}
	if !clientStream {
		body, err := clientCodec.EncodeResponse(resp)
		if err != nil {
			if clientSummary != nil {
				clientSummary.encErr()
			}
			writeError(w, clientCodec, ir.NewHTTPError(500, "encode response: "+err.Error()))
			return
		}
		// 头还能写：损耗注记并入 X-ModelSurge-Notes（请求侧注记可能已在里面）。
		writeRespNotes(w, clientCodec.Name(), append(respNotes, clientCodec.ResponseNotes(resp)...))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		n, _ := w.Write(body)
		if clientSummary != nil {
			clientSummary.wrote(n)
		}
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // 与真流式出口同一套 SSE 头
	w.WriteHeader(200)
	enc := proto.NewClientStreamEncoder(clientCodec, req)
	for _, ev := range EventsFromResponse(resp) {
		frames, err := enc.Encode(ev)
		if err != nil {
			if clientSummary != nil {
				clientSummary.encErr()
			}
			frames = [][]byte{clientCodec.RenderStreamError(&ir.Error{Type: ir.ErrTypeUpstream, Message: err.Error()})}
		}
		if clientSummary != nil {
			clientSummary.framesAdd(len(frames))
		}
		for _, fr := range frames {
			n, _ := w.Write(fr)
			if clientSummary != nil {
				clientSummary.wrote(n)
			}
		}
	}
	finished := enc.Finish()
	if clientSummary != nil {
		clientSummary.framesAdd(len(finished))
	}
	for _, fr := range finished {
		n, _ := w.Write(fr)
		if clientSummary != nil {
			clientSummary.wrote(n)
		}
	}
	// 编码过程实际发生的损耗（签名门控/参数挪键）由编码器计数；加上聚合
	// 阶段的注记一起落 SSE 注释帧——头已发出，写不进 X-ModelSurge-Notes。
	streamNotes := append(respNotes, enc.Notes()...)
	logRespNotes(clientCodec.Name(), streamNotes)
	for _, fr := range proto.SSENoteFrames(streamNotes) {
		n, _ := w.Write(fr)
		if clientSummary != nil {
			clientSummary.wrote(n)
		}
	}
	if flush, ok := w.(http.Flusher); ok {
		flush.Flush()
	}
}

// EventsFromResponse 把完整响应合成为一次性 IR 事件序列
// （用于"上游返回非流式 JSON 而客户端要 SSE"的兜底路径）。
func EventsFromResponse(resp *ir.Response) []ir.Event {
	events := []ir.Event{{
		Type: ir.EvMessageStart, MessageID: resp.ID, Model: resp.Model,
		ServiceTier: resp.ServiceTier, SystemFingerprint: resp.SystemFingerprint,
		Container: resp.Container, Audio: resp.Audio,
	}}
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
		// server_tool_use 同拆：anthropic 编码器开块恒写 input {}，查询串只认
		// input_json_delta；不拆的话「上游非流式、客户端流式」路径下发的调用
		// 块永远带着空查询。
		if blk.Type == ir.BlockServerToolUse && blk.ServerToolUse != nil && len(blk.ServerToolUse.Input) > 0 {
			input := string(blk.ServerToolUse.Input)
			blk.ServerToolUse.Input = nil
			events = append(events,
				ir.Event{Type: ir.EvBlockStart, Index: i, Block: &blk},
				ir.Event{Type: ir.EvToolInput, Index: i, Text: input},
				ir.Event{Type: ir.EvBlockStop, Index: i})
			continue
		}
		// 引用改走 EvCitation：流式协议一律在正文之后单独下发标注
		// （Anthropic 的 citations_delta、Chat 的 delta.annotations），
		// 留在块开始上会让编码器在正文还没发出时就写出偏移量。
		cites := blk.Citations
		blk.Citations = nil
		// 全块透传（server_tool_use / web_search_tool_result 的载荷在块上）
		events = append(events, ir.Event{Type: ir.EvBlockStart, Index: i, Block: &blk})
		switch blk.Type {
		case ir.BlockText, ir.BlockRefusal:
			// 拒绝块也用 Text 承载。漏掉这一档会让「上游非流式、客户端流式」
			// 这条路径只发出空的块开合，拒绝正文整条不见。
			if blk.Text != "" {
				events = append(events, ir.Event{Type: ir.EvTextDelta, Index: i, Text: blk.Text})
			}
			if len(cites) > 0 {
				events = append(events, ir.Event{Type: ir.EvCitation, Index: i, Citations: cites})
			}
		case ir.BlockThinking:
			if blk.Thinking != nil {
				if blk.Thinking.Text != "" {
					events = append(events, ir.Event{Type: ir.EvThinkingDelta, Index: i, Text: blk.Thinking.Text})
				}
				if blk.Thinking.Signature != "" {
					// 来源必须一起回放：这条路径是把已解码的响应重放成事件流，
					// 丢了来源等于把上游真签名降级成来源不明。
					events = append(events, ir.Event{Type: ir.EvSigDelta, Index: i,
						Text: blk.Thinking.Signature, SignatureFrom: blk.Thinking.SignatureFrom})
				}
			}
		}
		events = append(events, ir.Event{Type: ir.EvBlockStop, Index: i})
	}
	u := resp.Usage
	events = append(events,
		ir.Event{Type: ir.EvMessageDelta, StopReason: resp.StopReason, StopSequence: resp.StopSequence, StopDetails: resp.StopDetails, Usage: &u},
		ir.Event{Type: ir.EvMessageStop})
	return events
}

func writeError(w http.ResponseWriter, clientCodec proto.InboundCodec, e *ir.Error) {
	status, body := clientCodec.RenderError(e)
	w.Header().Set("Content-Type", "application/json")
	setRetryAfter(w, e)
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func (f *Forwarder) writeClientError(ctx context.Context, w http.ResponseWriter, clientCodec proto.InboundCodec, stream bool, e *ir.Error) {
	status, body := clientCodec.RenderError(e)
	w.Header().Set("Content-Type", "application/json")
	setRetryAfter(w, e)
	w.WriteHeader(status)
	written, _ := w.Write(body)
	if f.paramLog {
		log.Printf("agent phase=client_out request_id=%s proto=%s stream=%t status=%d bytes=%d error=true latency=%s", requestIDFrom(ctx), clientCodec.Name(), stream, status, written, requestLatencyFrom(ctx))
	}
}

// setRetryAfter 把上游的退避提示原样转给客户端。只在错误响应上写：正常响应里
// 上游没给过这个头，凭空造一个会让客户端以为被限流。
func setRetryAfter(w http.ResponseWriter, e *ir.Error) {
	if e != nil && e.RetryAfter != "" {
		w.Header().Set("Retry-After", e.RetryAfter)
	}
}

// sanitizeRetryAfter 校验上游给的 Retry-After 值。RFC 9110 只允许两种形态：
// 非负十进制秒数（delay-seconds）或 HTTP 日期。上游是外部边界，值可能带控制字符或
// 随意文本，直接透传等于把上游的话写进我们自己的响应头，所以不认识的形态一律丢掉
// （丢掉只是少了个提示，客户端退回自己的默认退避）。
func sanitizeRetryAfter(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	if n, err := strconv.Atoi(v); err == nil {
		if n < 0 {
			return ""
		}
		return v
	}
	if _, err := http.ParseTime(v); err == nil {
		return v
	}
	return ""
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

// CountTokens 处理 Anthropic count_tokens 请求：本地按 IR 通用估算返回。
func (f *Forwarder) CountTokens(req *ir.Request) (int, []byte) {
	est := ir.EstimateRequestTokens(req)
	return 200, []byte(fmt.Sprintf(`{"input_tokens":%d}`, est))
}
func triedIDs(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for id := range m {
		out = append(out, id)
	}
	return out
}
func replayDispatchError(err error) *ir.Error {
	if e, ok := err.(replayv1.Error); ok {
		switch e.Code {
		case replayv1.CodeUnauthorized:
			return &ir.Error{StatusCode: http.StatusUnauthorized, Type: ir.ErrTypeAuth, Message: e.Message}
		case replayv1.CodeNotFound:
			return &ir.Error{StatusCode: http.StatusNotFound, Type: ir.ErrTypeInvalidReq, Message: e.Message}
		case replayv1.CodeInvalidRequest:
			return &ir.Error{StatusCode: http.StatusBadRequest, Type: ir.ErrTypeInvalidReq, Message: e.Message}
		case replayv1.CodeContextTooLarge:
			return &ir.Error{StatusCode: http.StatusRequestEntityTooLarge, Type: ir.ErrTypeInvalidReq, Message: e.Message}
		default:
			return &ir.Error{StatusCode: http.StatusServiceUnavailable, Type: ir.ErrTypeUpstream, Message: e.Message, Retryable: e.Retryable}
		}
	}
	return &ir.Error{StatusCode: http.StatusServiceUnavailable, Type: ir.ErrTypeConnection, Message: "replay unavailable", Retryable: true}
}
