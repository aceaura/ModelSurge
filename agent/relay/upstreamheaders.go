// upstreamheaders.go 上游成功响应里值得回传给客户端的头。
//
// 白名单，而不是像 new-api 的 IOCopyBytesGracefully 那样整表复制：复制一切就得
// 同时接管 framing 头（Content-Length / Transfer-Encoding / Connection），而我们
// 交给客户端的 body 是重新编码过的（协议可能都换了），长度与上游无关——照抄
// Content-Length 会让客户端按错误长度截断或挂死。白名单天然不碰这些头。
//
// 只覆盖成功路径。非 2xx 时客户端需要的是「多久后重试」，那一条已由 R86 的
// Retry-After 走 ir.Error 送到；限流头要在错误路径上回传就得给 ir.Error 加一个
// 头集合并改动所有错误构造点，收益不抵面积。
package relay

import (
	"net/http"
	"strings"
)

// forwardedUpstreamHeaders 精确名（小写）白名单。x-request-id 是厂商侧的关联
// 键：客户端报障时拿它才能对上厂商日志，本仓日志里只有自己的 request_id。
var forwardedUpstreamHeaders = map[string]bool{
	"x-request-id": true,
}

// forwardedUpstreamHeaderPrefixes 前缀白名单：两家厂商的限流头族。客户端
// （Claude Code / Codex CLI 一类）用这些头做自适应退避与配额展示，全丢的话它们
// 只能当作「无限制」，然后在 429 上硬撞。
//
// 多账号复用下这些数字描述的是本轮实际服务的那个上游账号、不是客户端自己的
// 配额。刻意照传：有信号优于无信号，单账号部署下就是准确值，而 sub2api 那样
// 全部吞掉会让客户端彻底失去退避依据。
var forwardedUpstreamHeaderPrefixes = []string{
	"x-ratelimit-",
	"anthropic-ratelimit-",
}

// forwardUpstreamHeaders 把上游响应里白名单内的头复制到客户端响应，同名多值
// 整族带走。必须在第一次 Write/WriteHeader 之前调用：之后 net/http 已经把状态行
// 与头刷出去，再设的值客户端收不到。
//
// 用替换而非追加：换账号重试时前一次尝试的头必须被最后一次成功的上游覆盖，
// 否则客户端会收到两个互相矛盾的 x-request-id。
func forwardUpstreamHeaders(w http.ResponseWriter, resp *http.Response) {
	dst := w.Header()
	for key, vals := range resp.Header {
		if !forwardedUpstreamHeader(key) {
			continue
		}
		dst[key] = vals
	}
}

func forwardedUpstreamHeader(key string) bool {
	lower := strings.ToLower(key)
	if forwardedUpstreamHeaders[lower] {
		return true
	}
	for _, p := range forwardedUpstreamHeaderPrefixes {
		if strings.HasPrefix(lower, p) {
			return true
		}
	}
	return false
}
