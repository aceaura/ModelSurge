package relay

import (
	"context"
	"io"
	"log"
	"strings"
	"time"

	"github.com/aceaura/ModelSurge/agent/ir"
	"github.com/aceaura/ModelSurge/agent/proto"
	"github.com/aceaura/ModelSurge/replay/contract/replayv1"

	"github.com/google/uuid"
)

// headerCompacted 第二档续命成功响应的观测标记头（2.6）。
const headerCompacted = "X-ModelSurge-Compacted"

// autoCompactState 第二档循环状态（forwardRemote 循环局部，无跨请求状态）。
type autoCompactState struct {
	src    *ir.Request // 原始全量历史快照（每轮压缩都从原始历史切分）
	k      int         // 保留轮数（首轮 2，第二轮 0；4.4 单调递减）
	calls  int         // 已发起压缩调用数（≤2）
	header bool        // 续命响应需带标记头
}

// tryAutoCompact 第二档触发与执行：切分原始历史、构造压缩调用、重建历史。
// 返回 ok=true 时已产出压缩后请求（调用方重置 tried/attempts 并重新 dispatch
// 原模型）；ok=false 表示不可行或失败（调用方按原失败返回）。
func (f *Forwarder) tryAutoCompact(ctx context.Context, clientCodec proto.InboundCodec, req *ir.Request, compressModel, clientKey, requestID string, auto *autoCompactState) (*ir.Request, bool) {
	if auto.src == nil {
		auto.src = req.Clone()
		auto.k = 2
	} else if auto.calls >= 2 || auto.k == 0 {
		return nil, false // 压缩预算耗尽 / K 已到底：按原失败返回（2.3）
	} else {
		auto.k = 0
	}
	keepFrom := ir.SplitForCompact(auto.src.Messages, auto.k)
	if keepFrom == 0 {
		// 无旧历史可压缩（对话只剩当前轮，2.5）——压缩无从缩小
		log.Printf("agent phase=auto_compact request_id=%s result=skip model=%s reason=no_old_history", requestID, req.Model)
		return nil, false
	}
	compReq := ir.BuildCompactRequest(auto.src.Messages[:keepFrom])
	compReq.Model = compressModel
	auto.calls++
	log.Printf("agent phase=auto_compact request_id=%s result=trigger model=%s compress_model=%s round=%d k=%d", requestID, auto.src.Model, compressModel, auto.calls, auto.k)
	summary, ok := f.runCompactCall(ctx, clientCodec, compReq, auto.src.Model, compressModel, clientKey, requestID)
	if !ok {
		log.Printf("agent phase=auto_compact request_id=%s result=abort model=%s compress_model=%s round=%d", requestID, auto.src.Model, compressModel, auto.calls)
		return nil, false // 压缩调用失败按原失败返回（2.4）
	}
	newReq := ir.BuildCompactedHistory(auto.src, summary, keepFrom)
	auto.header = true
	log.Printf("agent phase=auto_compact request_id=%s result=redispatch model=%s compress_model=%s round=%d est_tokens=%d", requestID, auto.src.Model, compressModel, auto.calls, estimateReqTokens(clientCodec, newReq))
	return newReq, true
}

// estimateReqTokens 估算输入+输出预算（kiro 入站有专用 tokenizer 时用之）。
func estimateReqTokens(clientCodec proto.InboundCodec, req *ir.Request) int {
	if te, ok := clientCodec.(interface{ EstimateRequestTokens(*ir.Request) int }); ok {
		return te.EstimateRequestTokens(req) + req.MaxTokens
	}
	return ir.EstimateRequestTokens(req) + req.MaxTokens
}

// isContextTooLargeDispatch dispatch 错误是否调度层窗口过滤超限（第二档
// dispatch 路径触发条件）。
func isContextTooLargeDispatch(err error) bool {
	e, ok := err.(replayv1.Error)
	return ok && e.Code == replayv1.CodeContextTooLarge
}

// runCompactCall 执行一次内部压缩调用（第二档）：压缩请求经完整
// dispatch→上游聚合链路拿到 summary 文本。不向客户端写任何字节；
// 任何失败返回 ok=false（调用方按原失败返回，2.4）。
// kiro 目标不支持（数据面特殊、复用代价高），跳过换下一候选。
func (f *Forwarder) runCompactCall(ctx context.Context, clientCodec proto.InboundCodec, compReq *ir.Request, origModel, compressModel, clientKey, requestID string) (string, bool) {
	tried := map[string]bool{}
	for {
		if ctx.Err() != nil {
			return "", false
		}
		lease, err := f.replay.Dispatch(ctx, replayv1.DispatchRequest{
			Model:           compressModel,
			InboundProtocol: clientCodec.Name(),
			ClientKey:       clientKey,
			RequestID:       requestID,
			TriedIDs:        triedIDs(tried),
			EstTokens:       ir.EstimateRequestTokens(compReq) + compReq.MaxTokens,
			CompressOf:      origModel,
		})
		if err != nil {
			log.Printf("agent phase=auto_compact request_id=%s result=dispatch_fail compress_model=%s error=%s", requestID, compressModel, err)
			return "", false
		}
		cand, cerr := resolvedCandidate(lease)
		if cerr != nil || cand.protocol == "kiro" {
			if cerr == nil {
				log.Printf("agent phase=auto_compact request_id=%s result=skip_target target=%s reason=kiro_unsupported", requestID, lease.TargetID)
			}
			tried[lease.TargetID] = true
			continue
		}
		summary, usage, aerr := f.fetchSummary(ctx, cand, compReq)
		report := replayv1.ResultReport{
			ReportID:  uuid.NewString(),
			RequestID: requestID,
			GroupID:   lease.GroupID,
			TargetID:  lease.TargetID,
			Outcome:   "normal",
			Usage:     usage,
			At:        time.Now(),
		}
		if aerr != nil {
			report.Outcome = "abnormal"
			report.Status = aerr.StatusCode
			report.Reason = aerr.Reason
			report.Message = excerpt(aerr.Message)
			_ = f.finishReport(clientCodec.Name(), compressModel, report)
			log.Printf("agent phase=auto_compact request_id=%s result=attempt_fail compress_model=%s target=%s status=%d", requestID, compressModel, lease.TargetID, aerr.StatusCode)
			tried[lease.TargetID] = true
			continue
		}
		_ = f.finishReport(clientCodec.Name(), compressModel, report)
		return summary, true
	}
}

// fetchSummary 对单个上游执行压缩调用并聚合为 summary 文本。
// 复用 attempt 的上游预处理（native 模型改写、覆盖、流式请求），
// 但聚合结果不写客户端。
func (f *Forwarder) fetchSummary(ctx context.Context, cand candidate, req *ir.Request) (string, replayv1.Usage, *ir.Error) {
	upReq := req.Clone()
	upReq.Model = cand.native
	upReq.Stream = true
	cand.ov.Apply(upReq)
	if cl, ok := cand.codec.(interface{ ClampThinking(*ir.Request) }); ok {
		cl.ClampThinking(upReq)
	}
	resp, cancel, uerr := f.openUpstream(ctx, cand, upReq)
	if uerr != nil {
		return "", replayv1.Usage{}, uerr
	}
	defer resp.Body.Close()
	defer cancel()

	var upBody io.Reader = resp.Body
	isSSE := strings.Contains(resp.Header.Get("Content-Type"), "event-stream")
	if bw, ok := cand.codec.(interface{ WrapResponseBody(io.Reader) io.Reader }); ok {
		upBody = bw.WrapResponseBody(resp.Body)
		isSSE = true
	}
	var irResp *ir.Response
	if !isSSE {
		full, readErr := io.ReadAll(upBody)
		if readErr != nil {
			return "", replayv1.Usage{}, &ir.Error{StatusCode: 502, Type: ir.ErrTypeUpstream, Message: readErr.Error(), Retryable: true}
		}
		decoded, decErr := cand.codec.DecodeResponse(full)
		if decErr != nil {
			return "", replayv1.Usage{}, &ir.Error{StatusCode: 502, Type: ir.ErrTypeUpstream, Message: "decode upstream response: " + decErr.Error(), Retryable: true}
		}
		irResp = decoded
	} else {
		aggregated, aerr := f.aggregateUpstream(ctx, cand, upReq, f.newDecoder(cand, upReq), upBody, cancel)
		if aerr != nil {
			return "", replayv1.Usage{}, aerr
		}
		irResp = aggregated
	}
	usage := replayv1.Usage{InputTokens: int64(irResp.Usage.InputTokens), OutputTokens: int64(irResp.Usage.OutputTokens), CacheRead: int64(irResp.Usage.CacheReadTokens), CacheCreation: int64(irResp.Usage.CacheCreationTokens)}
	var sb strings.Builder
	for _, b := range irResp.Content {
		if b.Type == ir.BlockText {
			sb.WriteString(b.Text)
		}
	}
	if strings.TrimSpace(sb.String()) == "" {
		return "", usage, &ir.Error{StatusCode: 502, Type: ir.ErrTypeUpstream, Message: "compress model returned empty summary", Retryable: true}
	}
	return sb.String(), usage, nil
}
