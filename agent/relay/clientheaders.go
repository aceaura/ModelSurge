// clientheaders.go 客户端入站请求里需要转交上游的 wire 头（目前只有 anthropic-beta）。
//
// 走 context 而不是 ir.Request：ir.Request 是跨协议的规范请求，Clone 是 JSON 往返，
// 而 kiro 数据面把整个 ir.Request 序列化进信封交给 Replay——放进去等于把一个只对
// anthropic wire 有意义的头塞进 kiro 载荷与所有出站编码器的视野。它和 request id
// 同源同寿命（一次请求、只给上游看），按同一套 ctx 载体走。
package relay

import (
	"context"
	"net/http"
	"strings"
)

// oauthBeta Claude Code 用订阅 OAuth 登录时发的 beta。本仓的 anthropic 账号一律是
// api-key（endpoint 的 anthropic 分支写死 x-api-key；OAuth 形态只有 codex 一族，
// 走的不是 anthropic wire），把它转交给 api-key 端点或第三方兼容端点只会招 4xx。
// sub2api 同样按账号形态区别处置：OAuth 账号缺了就补，Vertex 直接剥掉。
const oauthBeta = "oauth-2025-04-20"

type clientHeaderContextKey struct{}

type clientHeaders struct {
	anthropicBetas []string
}

// WithClientHeaders 从入站 HTTP 头里取出需要转交上游的部分挂到 ctx。server 层在
// 调 Forward 之前包一次；relay 内部测试可以直接构造 ctx。
func WithClientHeaders(ctx context.Context, h http.Header) context.Context {
	return context.WithValue(ctx, clientHeaderContextKey{}, clientHeaders{
		anthropicBetas: parseAnthropicBetas(h.Values("anthropic-beta")),
	})
}

func clientHeadersFrom(ctx context.Context) clientHeaders {
	v, _ := ctx.Value(clientHeaderContextKey{}).(clientHeaders)
	return v
}

// parseAnthropicBetas 拆逗号列表（同一个头也可能重复出现多行），去空白、去重、
// 剔掉 oauth beta 与非法 token，保留客户端给的顺序。
func parseAnthropicBetas(lines []string) []string {
	var out []string
	seen := make(map[string]bool)
	for _, line := range lines {
		for _, raw := range strings.Split(line, ",") {
			tok := strings.TrimSpace(raw)
			if tok == oauthBeta || !validWireToken(tok) || seen[tok] {
				continue
			}
			seen[tok] = true
			out = append(out, tok)
		}
	}
	return out
}

// validWireToken 头值的字符集校验。beta id 形如 context-1m-2025-08-07，合法取值
// 全落在这个集合里；卡掉空白与控制字符也就卡掉了头注入——这些值最终要拼回出站
// 请求头，而 http.Header.Set 本身不做校验。
func validWireToken(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

// applyClientWireHeaders 把客户端的 anthropic-beta 补到出站请求上，与账号级自定义
// 头合并去重（运维为某个目标钉死的 beta 不能被客户端顶掉，反之客户端要的能力也
// 不能因为账号钉了一个就全丢）。
//
// 只对 anthropic wire 的候选生效：beta 是 Anthropic 私有词汇，转交给
// openai/gemini/codex 上游轻则被忽略、重则 400（sub2api 给 Vertex 单列白名单、
// 未知 token 直接 400 就是同一个教训）。跨协议时这些能力由各 codec 的参数换算
// 负责，不是头。
func applyClientWireHeaders(ctx context.Context, dst http.Header, upstreamProto string) {
	betas := clientHeadersFrom(ctx).anthropicBetas
	if len(betas) == 0 || upstreamProto != "anthropic" {
		return
	}
	merged := make([]string, 0, len(betas)+1)
	seen := make(map[string]bool, len(betas)+1)
	for _, line := range dst.Values("anthropic-beta") {
		for _, raw := range strings.Split(line, ",") {
			if tok := strings.TrimSpace(raw); tok != "" && !seen[tok] {
				seen[tok] = true
				merged = append(merged, tok)
			}
		}
	}
	for _, tok := range betas {
		if !seen[tok] {
			seen[tok] = true
			merged = append(merged, tok)
		}
	}
	dst.Set("anthropic-beta", strings.Join(merged, ","))
}
