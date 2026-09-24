package gemini

import (
	"strings"
	"testing"
)

// R109-B6 Tool 的 retrieval/exaAiSearch 声明收下：照 urlContext 同款「未识别
// 托管」模式进 IR，出站按未映射丢弃并报诊断，不再解码即蒸发。
func TestR109HostedRetrievalExaCaptured(t *testing.T) {
	req, err := New().DecodeRequest([]byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],` +
		`"tools":[{"retrieval":{}},{"exaAiSearch":{}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	var hosted []string
	for _, tl := range req.Tools {
		hosted = append(hosted, tl.Hosted+":"+tl.HostedType)
	}
	joined := strings.Join(hosted, ",")
	if !strings.Contains(joined, "retrieval:retrieval") || !strings.Contains(joined, "exa_ai_search:exaAiSearch") {
		t.Fatalf("retrieval/exaAiSearch 没收下：%v", hosted)
	}
}

// R109-B5 toolConfig.retrievalConfig / includeServerSideToolInvocations 键名捕获：
// 没有 IR 槽位，收键名让 Diagnose 报得出。
func TestR109ToolConfigExtras(t *testing.T) {
	req, err := New().DecodeRequest([]byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],` +
		`"toolConfig":{"retrievalConfig":{"latLng":{"latitude":1,"longitude":2}},` +
		`"includeServerSideToolInvocations":true}}`))
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(req.GeminiExtras, ",")
	if !strings.Contains(joined, "toolConfig.retrievalConfig") ||
		!strings.Contains(joined, "toolConfig.includeServerSideToolInvocations") {
		t.Fatalf("toolConfig 键名没捕获：%v", req.GeminiExtras)
	}

	// 没给时闭嘴。
	quiet, err := New().DecodeRequest([]byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],` +
		`"toolConfig":{"functionCallingConfig":{"mode":"AUTO"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range quiet.GeminiExtras {
		if strings.HasPrefix(e, "toolConfig.") {
			t.Fatalf("缺席误报：%v", quiet.GeminiExtras)
		}
	}
}

// R109-B7 googleSearchRetrieval.dynamicRetrievalConfig 与
// generationConfig.audioTranscriptionConfig 键名捕获；旧版声明仍归一
// google_search 语义不受收键影响。
func TestR109DynamicRetrievalAndAudioTranscriptionKeys(t *testing.T) {
	req, err := New().DecodeRequest([]byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],` +
		`"tools":[{"googleSearchRetrieval":{"dynamicRetrievalConfig":{"mode":"MODE_DYNAMIC","dynamicThreshold":0.7}}}],` +
		`"generationConfig":{"audioTranscriptionConfig":{}}}`))
	if err != nil {
		t.Fatal(err)
	}
	var foundSearch bool
	for _, tl := range req.Tools {
		if tl.HostedType == "google_search" {
			foundSearch = true
		}
	}
	if !foundSearch {
		t.Fatalf("googleSearchRetrieval 应归一 google_search：%+v", req.Tools)
	}
	joined := strings.Join(req.GeminiExtras, ",")
	if !strings.Contains(joined, "googleSearchRetrieval.dynamicRetrievalConfig") {
		t.Fatalf("dynamicRetrievalConfig 键名没捕获：%v", req.GeminiExtras)
	}
	if !strings.Contains(joined, "audioTranscriptionConfig") {
		t.Fatalf("audioTranscriptionConfig 键名没捕获：%v", req.GeminiExtras)
	}

	// 空声明（{}）不造键名。
	quiet, err := New().DecodeRequest([]byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],` +
		`"tools":[{"googleSearchRetrieval":{}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range quiet.GeminiExtras {
		if strings.Contains(e, "dynamicRetrievalConfig") {
			t.Fatalf("空声明误报：%v", quiet.GeminiExtras)
		}
	}
}
