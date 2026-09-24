package proto_test

// R101：服务端托管工具声明贯通（原生类型名 + 参数）与流式保活帧。
//
// 审计证据：anthropic 入站把 web_search_20250305 归一成 web_search，版本串
// 被 CanonicalHosted 吞掉，同族回写被 nativeHosted 硬编码偷换；四参数
// （max_uses/allowed_domains/blocked_domains/user_location）在 DTO 上根本
// 没有槽位，解码边界即丢——同族往返都保不住，跨族更谈不上。responses 的
// web_search_preview 被压成 web_search，filters/user_location/search_context_size
// 同丢。gemini 的 google_search 原生名若直接写进别族 type 槽位是必 400 形状。
// 三族流式编码器把 EvPing 吞掉，长思考间隔下客户端读超时裸奔。

import (
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
	"github.com/aceaura/ModelSurge/agent/proto"
)

// anthropic 同族：版本串与四个声明参数逐键往返，一个都不许被偷换。
func TestAnthropicHostedToolVersionAndParamsRoundTrip(t *testing.T) {
	req := r100cDec(t, "anthropic",
		`{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}],`+
			`"tools":[{"type":"web_search_20250305","name":"web_search","max_uses":3,`+
			`"allowed_domains":["example.com"],"blocked_domains":["spam.example"],`+
			`"user_location":{"type":"approximate","city":"Shanghai"}}]}`)
	tool := req.Tools[0]
	if tool.Hosted != ir.HostedWebSearch || tool.HostedType != "web_search_20250305" {
		t.Fatalf("原生类型名没进 IR：%+v", tool)
	}
	p := tool.HostedParams
	if p == nil || p.MaxUses != 3 || len(p.AllowedDomains) != 1 || len(p.BlockedDomains) != 1 || len(p.UserLocation) == 0 {
		t.Fatalf("声明参数没进 IR：%+v", p)
	}

	out := r100bEnc(t, "anthropic", req)
	for _, want := range []string{
		`"type":"web_search_20250305"`, `"max_uses":3`,
		`"allowed_domains":["example.com"]`, `"blocked_domains":["spam.example"]`,
		`"user_location":{"type":"approximate","city":"Shanghai"}`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("anthropic 同族回写丢了 %s：%s", want, out)
		}
	}
}

// 客户端改了版本（更新的快照日期），同族回写不得拿硬编码默认版偷换。
func TestAnthropicHostedToolNewerVersionNotRewritten(t *testing.T) {
	req := r100cDec(t, "anthropic",
		`{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}],`+
			`"tools":[{"type":"web_search_20260309","name":"web_search"}]}`)
	out := r100bEnc(t, "anthropic", req)
	if !strings.Contains(out, `"type":"web_search_20260309"`) {
		t.Errorf("客户端指名的版本被默认版偷换：%s", out)
	}
	if strings.Contains(out, "20250305") {
		t.Errorf("硬编码默认版本泄漏上线：%s", out)
	}
}

// 没给参数的托管工具同族回写一个键也不造（缺省保持缺省）。
func TestAnthropicHostedToolWithoutParamsStaysLean(t *testing.T) {
	req := r100cDec(t, "anthropic",
		`{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}],`+
			`"tools":[{"type":"web_search_20250305","name":"web_search"}]}`)
	if req.Tools[0].HostedParams != nil {
		t.Fatalf("没给参数不该造出参数对象：%+v", req.Tools[0].HostedParams)
	}
	out := r100bEnc(t, "anthropic", req)
	for _, banned := range []string{"max_uses", "allowed_domains", "blocked_domains", "user_location"} {
		if strings.Contains(out, banned) {
			t.Errorf("没给的参数被造上线 %s：%s", banned, out)
		}
	}
}

// responses 同族：web_search_preview 类型与三维参数逐键往返（含 codex 别名）。
func TestResponsesHostedToolTypeAndParamsRoundTrip(t *testing.T) {
	req := r100cDec(t, "openai-responses",
		`{"model":"m","input":[{"role":"user","content":"hi"}],`+
			`"tools":[{"type":"web_search_preview","filters":{"allowed_domains":["example.com"]},`+
			`"user_location":{"type":"approximate","country":"CN"},"search_context_size":"high"}]}`)
	tool := req.Tools[0]
	if tool.Hosted != ir.HostedWebSearch || tool.HostedType != "web_search_preview" {
		t.Fatalf("原生类型名没进 IR：%+v", tool)
	}
	p := tool.HostedParams
	if p == nil || len(p.AllowedDomains) != 1 || len(p.UserLocation) == 0 || p.SearchContextSize != "high" {
		t.Fatalf("声明参数没进 IR：%+v", p)
	}

	for _, outbound := range []string{"openai-responses", "codex"} {
		out := r100bEnc(t, outbound, req)
		for _, want := range []string{
			`"type":"web_search_preview"`, `"allowed_domains":["example.com"]`,
			`"user_location":{"type":"approximate","country":"CN"}`, `"search_context_size":"high"`,
		} {
			if !strings.Contains(out, want) {
				t.Errorf("%s 同族回写丢了 %s：%s", outbound, want, out)
			}
		}
	}
}

// 朴素 web_search 不得被「升级」成 preview 形态。
func TestResponsesPlainWebSearchTypeNotUpgraded(t *testing.T) {
	req := r100cDec(t, "openai-responses",
		`{"model":"m","input":[{"role":"user","content":"hi"}],"tools":[{"type":"web_search"}]}`)
	out := r100bEnc(t, "openai-responses", req)
	if !strings.Contains(out, `"type":"web_search"`) || strings.Contains(out, "web_search_preview") {
		t.Errorf("朴素 web_search 类型被改写：%s", out)
	}
}

// 跨族 anthropic→responses：白名单与地理位置有槽直通，max_uses/blocked_domains
// 没有槽位不上线（损耗由 Diagnose 报）。
func TestAnthropicToResponsesHostedParamMapping(t *testing.T) {
	req := r100cDec(t, "anthropic",
		`{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}],`+
			`"tools":[{"type":"web_search_20250305","name":"web_search","max_uses":2,`+
			`"allowed_domains":["example.com"],"blocked_domains":["spam.example"],`+
			`"user_location":{"type":"approximate","city":"Shanghai"}}]}`)
	out := r100bEnc(t, "openai-responses", req)
	for _, want := range []string{
		`"type":"web_search"`, `"allowed_domains":["example.com"]`,
		`"user_location":{"type":"approximate","city":"Shanghai"}`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("跨族映射丢了 %s：%s", want, out)
		}
	}
	for _, banned := range []string{"max_uses", "blocked_domains", "web_search_20250305"} {
		if strings.Contains(out, banned) {
			t.Errorf("无槽位/外族原生名泄漏上线 %s：%s", banned, out)
		}
	}
}

// 跨族 responses→anthropic：白名单与地理位置直通，type 回落 anthropic 默认
// 带版本名，search_context_size 没有槽位不上线。
func TestResponsesToAnthropicHostedParamMapping(t *testing.T) {
	req := r100cDec(t, "openai-responses",
		`{"model":"m","input":[{"role":"user","content":"hi"}],`+
			`"tools":[{"type":"web_search_preview","filters":{"allowed_domains":["example.com"]},`+
			`"user_location":{"type":"approximate","country":"CN"},"search_context_size":"low"}]}`)
	out := r100bEnc(t, "anthropic", req)
	for _, want := range []string{
		`"type":"web_search_20250305"`, `"allowed_domains":["example.com"]`,
		`"user_location":{"type":"approximate","country":"CN"}`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("跨族映射丢了 %s：%s", want, out)
		}
	}
	for _, banned := range []string{"search_context_size", "web_search_preview"} {
		if strings.Contains(out, banned) {
			t.Errorf("无槽位/外族原生名泄漏上线 %s：%s", banned, out)
		}
	}
}

// gemini 入站原生名进 IR，但跨族出站必须回落目标族默认名：google_search 写进
// anthropic/responses 的 type 槽位是必 400 的形状；code_execution 同理须翻成
// 目标族的名字（responses 叫 code_interpreter）。
func TestGeminiHostedNativeNameNeverLeaksCrossFamily(t *testing.T) {
	req := r100cDec(t, "gemini",
		`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],`+
			`"tools":[{"googleSearch":{},"codeExecution":{}}]}`)
	if req.Tools[0].HostedType != "google_search" || req.Tools[1].HostedType != "code_execution" {
		t.Fatalf("gemini 原生名没进 IR：%+v", req.Tools)
	}

	outA := r100bEnc(t, "anthropic", req)
	for _, want := range []string{`"type":"web_search_20250305"`, `"type":"code_execution_20250522"`} {
		if !strings.Contains(outA, want) {
			t.Errorf("anthropic 出站没用本族默认名 %s：%s", want, outA)
		}
	}
	if strings.Contains(outA, "google_search") {
		t.Errorf("gemini 原生名泄漏进 anthropic type：%s", outA)
	}

	outR := r100bEnc(t, "openai-responses", req)
	for _, want := range []string{`"type":"web_search"`, `"type":"code_interpreter"`} {
		if !strings.Contains(outR, want) {
			t.Errorf("responses 出站没用本族默认名 %s：%s", want, outR)
		}
	}
	if strings.Contains(outR, `"type":"code_execution"`) || strings.Contains(outR, "google_search") {
		t.Errorf("gemini 原生名泄漏进 responses type：%s", outR)
	}
}

// 未识别的托管类型同族透传：responses 的 file_search 往返不丢。
func TestUnknownHostedTypePassthroughSameFamily(t *testing.T) {
	req := r100cDec(t, "openai-responses",
		`{"model":"m","input":[{"role":"user","content":"hi"}],"tools":[{"type":"file_search"}]}`)
	out := r100bEnc(t, "openai-responses", req)
	if !strings.Contains(out, `"type":"file_search"`) {
		t.Errorf("未知托管类型同族透传被丢：%s", out)
	}
}

// 流式保活帧：上游的 ping 在四族客户端出站都要有协议合法的表达。
// anthropic 有原生 ping 事件；其余三族发 SSE 注释行（协议合法、客户端解析器
// 跳过），吞掉等于让长思考间隔下的客户端读超时裸奔。
func TestPingFrameSurvivesOnAllFamilies(t *testing.T) {
	for _, name := range []string{"anthropic", "openai-chat", "openai-responses", "gemini"} {
		c, err := proto.GetInbound(name)
		if err != nil {
			t.Fatal(err)
		}
		enc := c.NewStreamEncoder()
		frames, err := enc.Encode(ir.Event{Type: ir.EvPing})
		if err != nil {
			t.Fatalf("%s 编码 ping：%v", name, err)
		}
		var out string
		for _, fr := range frames {
			out += string(fr)
		}
		if out == "" {
			t.Errorf("%s 把 ping 吞了，长间隔下客户端读超时裸奔", name)
		}
	}
}
