package gemini

import (
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// R105：gemini 入站的四个静默蒸发面。thinkingLevel（Gemini 3 新代表达）、
// googleSearchRetrieval 等托管工具声明形态、executableCode/codeExecutionResult
// 历史部件、labels/speechConfig/mediaResolution 专属声明键——此前全部
// 解码即丢，连「客户端给过」都无从知晓。

// thinkingLevel 是「要思考」的表态：只给 level 时 budget 缺省为 0，
// 按 budget!=0 判会把显式要思考翻成关闭。
func TestThinkingLevelMapsToEffort(t *testing.T) {
	body := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],` +
		`"generationConfig":{"thinkingConfig":{"thinkingLevel":"high"}}}`)
	req, err := New().DecodeRequest(body)
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if req.Thinking == nil || !req.Thinking.Enabled {
		t.Fatalf("只给 thinkingLevel 被判成不思考：%+v", req.Thinking)
	}
	if req.Thinking.Effort != "high" {
		t.Errorf("thinkingLevel 没进 effort：%q", req.Thinking.Effort)
	}
}

// 显式 budget=0（关思考）不带 level 时口径不变。
func TestThinkingBudgetZeroStillDisables(t *testing.T) {
	body := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],` +
		`"generationConfig":{"thinkingConfig":{"thinkingBudget":0}}}`)
	req, err := New().DecodeRequest(body)
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if req.Thinking == nil || req.Thinking.Enabled {
		t.Fatalf("显式 budget=0 被翻成开思考：%+v", req.Thinking)
	}
}

// googleSearchRetrieval 是旧版托管搜索声明，语义同 googleSearch；
// urlContext/fileSearch/googleMaps 无跨族映射，收下让出站按未映射丢弃
// 并报诊断，而不是解码即蒸发。
func TestHostedToolDeclarationForms(t *testing.T) {
	body := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"tools":[` +
		`{"googleSearchRetrieval":{}},{"urlContext":{}},{"fileSearch":{}},{"googleMaps":{}}]}`)
	req, err := New().DecodeRequest(body)
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	var hosted []string
	for _, tl := range req.Tools {
		hosted = append(hosted, tl.Hosted+"|"+tl.HostedType)
	}
	want := []string{
		ir.HostedWebSearch + "|google_search",
		"url_context|urlContext",
		"file_search|fileSearch",
		"google_maps|googleMaps",
	}
	if len(hosted) != len(want) {
		t.Fatalf("托管工具声明被丢：got %v want %v", hosted, want)
	}
	for i := range want {
		if hosted[i] != want[i] {
			t.Errorf("第 %d 条不对：got %q want %q", i, hosted[i], want[i])
		}
	}
}

// executableCode / codeExecutionResult 历史部件归不透明块：降级成文本是
// 编造正文，静默丢弃连「这里有过一段代码」都不留。
func TestCodeExecutionPartsGoOpaque(t *testing.T) {
	body := []byte(`{"contents":[{"role":"model","parts":[` +
		`{"executableCode":{"language":"PYTHON","code":"print(1)"}},` +
		`{"codeExecutionResult":{"outcome":"OUTCOME_OK","output":"1\n"}}]}]}`)
	req, err := New().DecodeRequest(body)
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if len(req.Messages) != 1 || len(req.Messages[0].Content) != 2 {
		t.Fatalf("部件数量不对：%+v", req.Messages)
	}
	for i, wt := range []string{"executableCode", "codeExecutionResult"} {
		b := req.Messages[0].Content[i]
		if b.Type != ir.BlockOpaque || b.Opaque.WireType != wt || b.Opaque.From != Name {
			t.Errorf("%s 没归本族不透明块：%+v", wt, b)
		}
	}
	if !strings.Contains(string(req.Messages[0].Content[0].Opaque.Body), "print(1)") {
		t.Errorf("代码原文没逐字保留：%s", req.Messages[0].Content[0].Opaque.Body)
	}
}

// labels / speechConfig / mediaResolution：收下键名让 Diagnose 报得出，
// 值不建模。
func TestGeminiExtrasCollected(t *testing.T) {
	body := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],` +
		`"labels":{"team":"a"},"generationConfig":{` +
		`"speechConfig":{"voiceConfig":{}},"mediaResolution":"MEDIA_RESOLUTION_HIGH"}}`)
	req, err := New().DecodeRequest(body)
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	got := strings.Join(req.GeminiExtras, ",")
	for _, k := range []string{"labels", "speechConfig", "mediaResolution"} {
		if !strings.Contains(got, k) {
			t.Errorf("%s 没收下：%v", k, req.GeminiExtras)
		}
	}
}
