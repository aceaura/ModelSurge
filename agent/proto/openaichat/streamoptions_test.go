// streamoptions_test.go stream_options.include_usage 的两端语义：入站按客户端
// 原意解析，出站恒注入。两端刻意不对称，各自锁一条测试。
package openaichat

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

func chatReq(stream, optIn bool) *ir.Request {
	return &ir.Request{
		Model: "m", Stream: stream, IncludeUsage: optIn,
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}},
	}
}

// 入站：客户端有没有要 usage 帧必须能被区分出来。此前 stream_options 整个字段
// 虽然在 request 结构里，却从没被读过——三种写法解出的 IR 完全一样，出站编码器
// 只能一律发帧，没要的客户端收到一个 choices 为空数组的 chunk。
func TestDecodeStreamOptionsIncludeUsage(t *testing.T) {
	const base = `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]`
	for _, c := range []struct {
		name string
		body string
		want bool
	}{
		{"字段缺失", base + `}`, false},
		{"显式 true", base + `,"stream_options":{"include_usage":true}}`, true},
		{"显式 false", base + `,"stream_options":{"include_usage":false}}`, false},
		{"空对象", base + `,"stream_options":{}}`, false},
		{"null", base + `,"stream_options":null}`, false},
		// continuous_usage_stats（每帧都带 usage）没有对应实现，不能顺带把
		// include_usage 也当成真——那会发一个客户端没要的流末帧。
		{"只有其他键", base + `,"stream_options":{"continuous_usage_stats":true}}`, false},
		{"两者都给", base + `,"stream_options":{"continuous_usage_stats":true,"include_usage":true}}`, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := codec{}.DecodeRequest([]byte(c.body))
			if err != nil {
				t.Fatalf("DecodeRequest: %v", err)
			}
			if got.IncludeUsage != c.want {
				t.Fatalf("IncludeUsage=%v, want %v", got.IncludeUsage, c.want)
			}
			if !got.Stream {
				t.Fatalf("stream 标志被 stream_options 解析带偏")
			}
		})
	}
}

// 出站：恒注入 include_usage，与客户端意图无关。Agent 记账（ResultReport 的
// usage）依赖上游回报用量，客户端要不要看是另一回事，由客户端侧编码器决定。
// 这条锁死两端的解耦：日后有人把两处"统一"成一个开关，没 opt-in 的请求就会
// 静默丢掉记账数据。
func TestEncodeRequestAlwaysAsksUpstreamForUsage(t *testing.T) {
	for _, optIn := range []bool{false, true} {
		body, err := codec{}.EncodeRequest(chatReq(true, optIn))
		if err != nil {
			t.Fatalf("EncodeRequest: %v", err)
		}
		var got struct {
			Stream        bool `json:"stream"`
			StreamOptions *struct {
				IncludeUsage bool `json:"include_usage"`
			} `json:"stream_options"`
		}
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("unmarshal: %v\n%s", err, body)
		}
		if !got.Stream {
			t.Fatalf("optIn=%v: stream 标志丢了：%s", optIn, body)
		}
		if got.StreamOptions == nil || !got.StreamOptions.IncludeUsage {
			t.Fatalf("optIn=%v: 出站没强制 include_usage：%s", optIn, body)
		}
	}
}

// 非流式请求不该带 stream_options：它只对 SSE 有意义，发给上游是无效参数。
func TestEncodeNonStreamRequestOmitsStreamOptions(t *testing.T) {
	body, err := codec{}.EncodeRequest(chatReq(false, true))
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if strings.Contains(string(body), "stream_options") {
		t.Fatalf("非流式请求带了 stream_options：%s", body)
	}
}
