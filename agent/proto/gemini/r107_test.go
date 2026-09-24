package gemini

import (
	"strings"
	"testing"
)

// R107：generationConfig 六个不建模值的新键（图像生成/音频时间戳/公民问答/
// 路由/模型选择/安全审查）收进 GeminiExtras 让 Diagnose 报得出。
func TestGeminiR107ExtrasCollected(t *testing.T) {
	body := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],` +
		`"generationConfig":{` +
		`"imageConfig":{"aspectRatio":"1:1"},` +
		`"audioTimestamp":true,` +
		`"enableEnhancedCivicAnswers":true,` +
		`"routingConfig":{"autoMode":{}},` +
		`"modelSelectionConfig":{"featureSelection":"A"},` +
		`"modelArmorConfig":{"promptTemplateName":"x"}}}`)
	req, err := New().DecodeRequest(body)
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	got := strings.Join(req.GeminiExtras, ",")
	for _, k := range []string{
		"imageConfig", "audioTimestamp", "enableEnhancedCivicAnswers",
		"routingConfig", "modelSelectionConfig", "modelArmorConfig",
	} {
		if !strings.Contains(got, k) {
			t.Errorf("%s 没收下：%v", k, req.GeminiExtras)
		}
	}
}

// 值为 null 或整组缺席时不得登记键名——Diagnose 报出的是「声明被丢」，
// 空声明报出来就是误报。
func TestGeminiR107ExtrasNullAndAbsentSilent(t *testing.T) {
	body := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],` +
		`"generationConfig":{"imageConfig":null,"audioTimestamp":null}}`)
	req, err := New().DecodeRequest(body)
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if len(req.GeminiExtras) != 0 {
		t.Errorf("null 值不应登记：%v", req.GeminiExtras)
	}

	plain, err := New().DecodeRequest([]byte(
		`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if len(plain.GeminiExtras) != 0 {
		t.Errorf("无 generationConfig 不应登记：%v", plain.GeminiExtras)
	}
}
