package proto_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
	"github.com/aceaura/ModelSurge/agent/proto"
)

// 所有出口协议都必须把规范错误类型带到线上：客户端与 SDK 的重试逻辑靠它
// 区分「过滤拒绝」（重试必然再被拒）与「上游抖动」（值得换个上游）。

func inboundNames() []string {
	return []string{"anthropic", "gemini", "openai-chat", "openai-responses"}
}

func TestRenderErrorCarriesCanonicalType(t *testing.T) {
	e := &ir.Error{
		StatusCode: 400, Type: ir.ErrTypeContentFilter,
		Code: "cyber_policy", Message: "blocked by policy",
	}
	for _, name := range inboundNames() {
		t.Run(name, func(t *testing.T) {
			status, body := proto.MustInbound(name).RenderError(e)
			if status != 400 {
				t.Errorf("status = %d, want 400", status)
			}
			if !strings.Contains(string(body), ir.ErrTypeContentFilter) {
				t.Errorf("错误体里没有规范类型：%s", body)
			}
			if !strings.Contains(string(body), "blocked by policy") {
				t.Errorf("错误体里没有消息：%s", body)
			}
		})
	}
}

// 流内错误的 StatusCode 为零（relay 以 EvError 形式构造时不填），
// 不可重试的类别不得退化成 500。
func TestRenderErrorInfersStatusForStreamErrors(t *testing.T) {
	for _, tc := range []struct {
		typ    string
		status int
	}{
		{ir.ErrTypeContentFilter, 400},
		{ir.ErrTypeInvalidReq, 400},
		{ir.ErrTypeRateLimit, 429},
		{ir.ErrTypeUpstream, 500},
	} {
		for _, name := range inboundNames() {
			t.Run(tc.typ+"/"+name, func(t *testing.T) {
				status, _ := proto.MustInbound(name).RenderError(&ir.Error{Type: tc.typ, Message: "m"})
				if status != tc.status {
					t.Errorf("status = %d, want %d", status, tc.status)
				}
			})
		}
	}
}

// 上游错误码（如 kiro 的 INVALID_MODEL_ID、responses 的 cyber_policy）
// 原样保留，不得在渲染时丢掉。
func TestRenderErrorCarriesUpstreamCode(t *testing.T) {
	e := &ir.Error{StatusCode: 429, Type: ir.ErrTypeRateLimit, Code: "quota_exhausted", Message: "slow down"}
	// anthropic 的错误体外形里没有 code 字段（官方只有 type + message），
	// 这不是丢失而是协议里就没有位置，不参与断言。
	for _, name := range []string{"gemini", "openai-chat", "openai-responses"} {
		t.Run(name, func(t *testing.T) {
			_, body := proto.MustInbound(name).RenderError(e)
			if !strings.Contains(string(body), "quota_exhausted") {
				t.Errorf("上游错误码丢失：%s", body)
			}
		})
	}
}

// Gemini 的三字段错误体装不下类型与码，必须落进 details。
func TestGeminiErrorDetailsShape(t *testing.T) {
	c := proto.MustInbound("gemini")
	_, body := c.RenderError(&ir.Error{
		Type: ir.ErrTypeContentFilter, Code: "cyber_policy", Message: "blocked",
	})
	var got struct {
		Error struct {
			Code    int    `json:"code"`
			Status  string `json:"status"`
			Message string `json:"message"`
			Details []struct {
				Type     string            `json:"@type"`
				Reason   string            `json:"reason"`
				Domain   string            `json:"domain"`
				Metadata map[string]string `json:"metadata"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("解析 gemini 错误体：%v\n%s", err, body)
	}
	if got.Error.Code != 400 || got.Error.Status != "INVALID_ARGUMENT" {
		t.Errorf("code/status = %d/%q，want 400/INVALID_ARGUMENT", got.Error.Code, got.Error.Status)
	}
	if len(got.Error.Details) != 1 {
		t.Fatalf("details 应恰有一条：%s", body)
	}
	d := got.Error.Details[0]
	if d.Reason != ir.ErrTypeContentFilter {
		t.Errorf("details.reason = %q，want %q", d.Reason, ir.ErrTypeContentFilter)
	}
	if d.Type == "" || d.Domain == "" {
		t.Errorf("details 缺 @type/domain：%s", body)
	}
	if d.Metadata["upstream_code"] != "cyber_policy" {
		t.Errorf("details.metadata 缺上游码：%s", body)
	}
}

// 没有类型也没有码时不得凭空造出 details：空壳会让客户端以为有结构化信息。
func TestGeminiErrorOmitsEmptyDetails(t *testing.T) {
	_, body := proto.MustInbound("gemini").RenderError(&ir.Error{StatusCode: 502, Message: "bad gateway"})
	if strings.Contains(string(body), "details") {
		t.Errorf("无类型无码时不应有 details：%s", body)
	}
}

// 流式错误帧同样要带类型：断流后客户端只能从这一帧判断该不该重试。
func TestRenderStreamErrorCarriesCanonicalType(t *testing.T) {
	e := &ir.Error{Type: ir.ErrTypeContentFilter, Code: "cyber_policy", Message: "blocked"}
	for _, name := range inboundNames() {
		t.Run(name, func(t *testing.T) {
			frame := string(proto.MustInbound(name).RenderStreamError(e))
			if !strings.Contains(frame, ir.ErrTypeContentFilter) {
				t.Errorf("流式错误帧里没有规范类型：%s", frame)
			}
			// 帧里若带状态码，也得是按类型反推的那个，不能退化成 500：
			// 客户端对断流的重试判断只有这一帧可依据。
			if strings.Contains(frame, `"code":500`) {
				t.Errorf("流式错误帧的状态码退化成 500：%s", frame)
			}
			if !strings.HasPrefix(frame, "event:") && !strings.HasPrefix(frame, "data:") {
				t.Errorf("流式错误帧不是 SSE 外形：%q", frame)
			}
		})
	}
}
