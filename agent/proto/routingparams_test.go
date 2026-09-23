package proto_test

import (
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
	"github.com/aceaura/ModelSurge/agent/proto"
	_ "github.com/aceaura/ModelSurge/agent/proto/anthropic"
	_ "github.com/aceaura/ModelSurge/agent/proto/gemini"
	_ "github.com/aceaura/ModelSurge/agent/proto/openaichat"
	_ "github.com/aceaura/ModelSurge/agent/proto/openairesponses"
)

// R58 service_tier / prompt_cache_key 贯通：三家值集不同（anthropic
// auto/standard_only；chat auto/default/flex/scale/priority/fast；responses
// 另有 ultrafast），IR 保留原值，跨族映射在出站按目标值集进行。
// 参考仓对照：cc-switch 跨族直接静默丢；new-api 只当覆盖参数白名单。

const r58CacheKeyClue = "pck-r58-clue-7e2"

func TestServiceTierDecodeIntoIR(t *testing.T) {
	for _, c := range []struct{ name, body, want string }{
		{"anthropic", `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hi"}],"service_tier":"standard_only"}`, "standard_only"},
		{"openai-chat", `{"model":"m","messages":[{"role":"user","content":"hi"}],"service_tier":"flex","prompt_cache_key":"` + r58CacheKeyClue + `"}`, "flex"},
		{"openai-responses", `{"model":"m","input":"hi","service_tier":"ultrafast","prompt_cache_key":"` + r58CacheKeyClue + `"}`, "ultrafast"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, err := proto.MustInbound(c.name).DecodeRequest([]byte(c.body))
			if err != nil {
				t.Fatalf("DecodeRequest: %v", err)
			}
			if r.ServiceTier != c.want {
				t.Errorf("ServiceTier = %q, want %q", r.ServiceTier, c.want)
			}
		})
	}
	// 缓存键只有 OpenAI 两系有
	for _, name := range []string{"openai-chat", "openai-responses"} {
		r, _ := proto.MustInbound(name).DecodeRequest([]byte(
			`{"model":"m","input":"hi","messages":[{"role":"user","content":"hi"}],"prompt_cache_key":"` + r58CacheKeyClue + `"}`))
		if r.PromptCacheKey != r58CacheKeyClue {
			t.Errorf("%s PromptCacheKey = %q", name, r.PromptCacheKey)
		}
	}
}

// 同族回写：三族的合法档位各自原样回家；缓存键两系原样回家。
func TestServiceTierRoundTripSameFamily(t *testing.T) {
	for _, c := range []struct{ name, tier string }{
		{"anthropic", "standard_only"}, {"openai-chat", "flex"}, {"openai-responses", "ultrafast"}, {"codex", "priority"},
	} {
		t.Run(c.name, func(t *testing.T) {
			req := &ir.Request{Model: "m", MaxTokens: 100, ServiceTier: c.tier,
				Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}}}
			out, err := proto.MustOutbound(c.name).EncodeRequest(req)
			if err != nil {
				t.Fatalf("EncodeRequest: %v", err)
			}
			if !strings.Contains(string(out), `"service_tier":"`+c.tier+`"`) {
				t.Errorf("%s: 档位 %q 未回写: %s", c.name, c.tier, out)
			}
		})
	}
	req := &ir.Request{Model: "m", MaxTokens: 100, PromptCacheKey: r58CacheKeyClue,
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}}}
	for _, name := range []string{"openai-chat", "openai-responses", "codex"} {
		out, _ := proto.MustOutbound(name).EncodeRequest(req)
		if !strings.Contains(string(out), r58CacheKeyClue) {
			t.Errorf("%s: 缓存键未回写: %s", name, out)
		}
	}
}

// 跨族映射：default↔standard_only 互译（同为标准容量语义）；
// anthropic 值集 provably 装不下 flex/priority，chat 装不下 ultrafast——
// 载荷里一个字符都不能出现（丢的部分由诊断报出，见 relay 侧测试）。
func TestServiceTierCrossFamilyMapping(t *testing.T) {
	mkReq := func(tier string) *ir.Request {
		return &ir.Request{Model: "m", MaxTokens: 100, ServiceTier: tier,
			Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}}}
	}
	for _, c := range []struct {
		tier, target, wantKey string // wantKey 为空=该值不得出现
	}{
		{"auto", "anthropic", `"service_tier":"auto"`},
		{"auto", "openai-chat", `"service_tier":"auto"`},
		{"default", "anthropic", `"service_tier":"standard_only"`},
		{"standard_only", "openai-chat", `"service_tier":"default"`},
		{"standard_only", "openai-responses", `"service_tier":"default"`},
		{"flex", "openai-responses", `"service_tier":"flex"`},
		{"flex", "anthropic", ""},
		{"priority", "anthropic", ""},
		{"ultrafast", "openai-chat", ""},
	} {
		t.Run(c.tier+"->"+c.target, func(t *testing.T) {
			out, err := proto.MustOutbound(c.target).EncodeRequest(mkReq(c.tier))
			if err != nil {
				t.Fatalf("EncodeRequest: %v", err)
			}
			body := string(out)
			if c.wantKey == "" {
				if strings.Contains(body, "service_tier") {
					t.Errorf("装不下的 %q 泄漏进 %s 载荷: %s", c.tier, c.target, body)
				}
			} else if !strings.Contains(body, c.wantKey) {
				t.Errorf("映射结果 %q 未出现: %s", c.wantKey, body)
			}
		})
	}
}

// 族内不认识的值原样透传：可能是核对 SDK 之后官方新增的档位，
// 丢了比让上游照实 400 更糟（只报不拒）。anthropic 值集 provably 只有
// 两个，不认识的跨族值仍丢。
func TestServiceTierUnknownValuePassthroughInFamily(t *testing.T) {
	req := &ir.Request{Model: "m", MaxTokens: 100, ServiceTier: "futuretier-x",
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}}}
	out, _ := proto.MustOutbound("openai-chat").EncodeRequest(req)
	if !strings.Contains(string(out), "futuretier-x") {
		t.Errorf("族内未知档位应原样透传: %s", out)
	}
	out, _ = proto.MustOutbound("anthropic").EncodeRequest(req)
	if strings.Contains(string(out), "futuretier-x") {
		t.Errorf("anthropic 值集装不下的未知档位不应透传: %s", out)
	}
}

// anthropic 既没有缓存键槽位，值集也装不下 priority：两项都一个字符都不进载荷
// （丢的部分由诊断报出）。
func TestRoutingParamsNeverLeakToAnthropic(t *testing.T) {
	req := &ir.Request{Model: "m", MaxTokens: 100, ServiceTier: "priority", PromptCacheKey: r58CacheKeyClue,
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}}}
	out, err := proto.MustOutbound("anthropic").EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	body := string(out)
	for _, probe := range []string{"service_tier", "prompt_cache_key", "priority", r58CacheKeyClue} {
		if strings.Contains(body, probe) {
			t.Errorf("anthropic 泄漏 %q: %s", probe, body)
		}
	}
}

// 全缺省：IR 零值，出站不多一个键。
func TestRoutingParamsAbsentStayZero(t *testing.T) {
	r, err := proto.MustInbound("openai-chat").DecodeRequest([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if r.ServiceTier != "" || r.PromptCacheKey != "" {
		t.Errorf("缺省请求解出了东西：%+v", r)
	}
	for _, name := range []string{"anthropic", "openai-chat", "openai-responses"} {
		out, _ := proto.MustOutbound(name).EncodeRequest(&ir.Request{Model: "m", MaxTokens: 100,
			Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}}})
		body := string(out)
		if strings.Contains(body, `"service_tier"`) || strings.Contains(body, `"prompt_cache_key"`) {
			t.Errorf("%s 缺省时发明键: %s", name, body)
		}
	}
}

// 映射表自身的单元测试：所有既定规则逐格钉死。
func TestMapServiceTierTable(t *testing.T) {
	for _, c := range []struct {
		tier, target, want string
		ok                 bool
	}{
		{"auto", "anthropic", "auto", true},
		{"auto", "openai-chat", "auto", true},
		{"auto", "openai-responses", "auto", true},
		{"standard_only", "anthropic", "standard_only", true},
		{"standard_only", "openai-chat", "default", true},
		{"standard_only", "openai-responses", "default", true},
		{"standard_only", "codex", "default", true},
		{"default", "anthropic", "standard_only", true},
		{"flex", "anthropic", "", false},
		{"scale", "anthropic", "", false},
		{"priority", "anthropic", "", false},
		{"fast", "anthropic", "", false},
		{"ultrafast", "anthropic", "", false},
		{"ultrafast", "openai-chat", "", false},
		{"ultrafast", "openai-responses", "ultrafast", true},
		{"flex", "openai-chat", "flex", true},
		{"flex", "openai-responses", "flex", true},
	} {
		got, ok := proto.MapServiceTier(c.tier, c.target)
		if ok != c.ok || got != c.want {
			t.Errorf("MapServiceTier(%q,%q) = (%q,%v), want (%q,%v)", c.tier, c.target, got, ok, c.want, c.ok)
		}
	}
}
