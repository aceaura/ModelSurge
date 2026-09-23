package relay

import (
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// R65：anthropic 顶层 cache_control 糖与 inference_geo 的跨族诊断。
// 顶层糖官方语义=自动一个缓存断点，跨族丢失时计入缓存断点维度；
// inference_geo 是独立的地理偏好维度，外族全无对应。

func TestDiagnoseTopCacheCtlCountedOffAnthropic(t *testing.T) {
	req := &ir.Request{TopCacheCtl: "ephemeral", TopCacheTTL: "1h"}
	for _, name := range []string{"openai-chat", "openai-responses"} {
		got := strings.Join(Diagnose(req, name, capsOf(t, name)), "; ")
		if !strings.Contains(got, "dropped 1 cache breakpoint(s)") {
			t.Errorf("%s 顶层糖应计入断点数：%q", name, got)
		}
	}
	if notes := Diagnose(req, "anthropic", capsOf(t, "anthropic")); len(notes) != 0 {
		t.Errorf("anthropic 自家接得住，误报：%v", notes)
	}
}

// 顶层糖与块级断点并存时计数相加（1 块级 + 1 顶层糖 = 2）。
func TestDiagnoseTopCacheCtlAddsToBlockCount(t *testing.T) {
	req := &ir.Request{
		TopCacheCtl: "ephemeral",
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{
			{Type: ir.BlockText, Text: "hi", CacheCtl: "ephemeral"},
		}}},
	}
	got := strings.Join(Diagnose(req, "openai-chat", capsOf(t, "openai-chat")), "; ")
	if !strings.Contains(got, "dropped 2 cache breakpoint(s)") {
		t.Errorf("块级+顶层糖应计 2：%q", got)
	}
}

func TestDiagnoseInferenceGeoDroppedOffAnthropic(t *testing.T) {
	req := &ir.Request{InferenceGeo: "us"}
	for _, name := range []string{"openai-chat", "openai-responses"} {
		got := strings.Join(Diagnose(req, name, capsOf(t, name)), "; ")
		if !strings.Contains(got, "dropped inference_geo") {
			t.Errorf("%s 应报 inference_geo 丢失：%q", name, got)
		}
		// 区域偏好值不回显（客户端自选值不入诊断）。
		if strings.Contains(got, `"us"`) || strings.Contains(got, " us") {
			t.Errorf("%s 不应回显 geo 值：%q", name, got)
		}
	}
	if notes := Diagnose(req, "anthropic", capsOf(t, "anthropic")); len(notes) != 0 {
		t.Errorf("anthropic 自家接得住，误报：%v", notes)
	}
}

func TestDiagnoseTopPrefsSilentWhenAbsent(t *testing.T) {
	req := &ir.Request{}
	for _, name := range []string{"anthropic", "openai-chat", "openai-responses"} {
		if notes := Diagnose(req, name, capsOf(t, name)); len(notes) != 0 {
			t.Errorf("%s 空请求误报：%v", name, notes)
		}
	}
}
