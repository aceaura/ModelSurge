package proto_test

import (
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
	"github.com/aceaura/ModelSurge/agent/proto"
)

// R60：缓存断点跨族零泄漏。断点是 anthropic 专属维度，OpenAI 两系与 kiro
// 的载荷里一个字符都不能出现（丢的部分由诊断报出，见 relay 侧测试）。

func TestCacheBreakpointsNeverLeakToOtherFamilies(t *testing.T) {
	req := &ir.Request{Model: "m", MaxTokens: 100,
		System: []ir.Block{{Type: ir.BlockText, Text: "sys", CacheCtl: "ephemeral", CacheTTL: "1h"}},
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{
			{Type: ir.BlockText, Text: "hi", CacheCtl: "ephemeral", CacheTTL: "1h"}}}},
		Tools: []ir.Tool{{Name: "ping", InputSchema: []byte(`{"type":"object"}`),
			CacheCtl: "ephemeral", CacheTTL: "1h"}}}
	for _, name := range []string{"openai-chat", "openai-responses", "kiro"} {
		out, err := proto.MustOutbound(name).EncodeRequest(req)
		if err != nil {
			t.Fatalf("%s EncodeRequest: %v", name, err)
		}
		body := string(out)
		for _, probe := range []string{"cache_control", "ephemeral", `"1h"`} {
			if strings.Contains(body, probe) {
				t.Errorf("%s 泄漏 %q: %s", name, probe, body)
			}
		}
	}
	// anthropic 自家必须留住全部三个断点
	out, err := proto.MustOutbound("anthropic").EncodeRequest(req)
	if err != nil {
		t.Fatalf("anthropic EncodeRequest: %v", err)
	}
	if n := strings.Count(string(out), `"cache_control"`); n != 3 {
		t.Errorf("anthropic 断点应全部回写（3 处），实得 %d: %s", n, out)
	}
}
