package relay

import (
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
	"github.com/aceaura/ModelSurge/agent/proto"
	_ "github.com/aceaura/ModelSurge/agent/proto/anthropic"
	_ "github.com/aceaura/ModelSurge/agent/proto/openaichat"
	_ "github.com/aceaura/ModelSurge/agent/proto/openairesponses"
)

// 无诊断时不得留下空值头：Set(k, "") 会让客户端看到「有说明但为空」。
func TestWriteLossyNotesSkipsEmpty(t *testing.T) {
	w := httptest.NewRecorder()
	writeLossyNotes(w, "target", nil)
	if _, ok := w.Header()["X-Modelsurge-Notes"]; ok {
		t.Errorf("无诊断时不应写头：%v", w.Header())
	}
	writeLossyNotes(w, "target", []string{"a", "b"})
	if got := w.Header().Get("X-ModelSurge-Notes"); got != "a; b" {
		t.Errorf("多条诊断应以 \"; \" 连接，得到 %q", got)
	}
}

func capsOf(t *testing.T, name string) proto.Capabilities {
	t.Helper()
	c, err := proto.GetOutbound(name)
	if err != nil {
		t.Fatalf("GetOutbound(%q): %v", name, err)
	}
	return c.Caps()
}

// capsWithout 取 base 出站的真实能力声明，再把 fields 指定的布尔位逐个关掉，
// 用来覆盖「能力缺失」那一侧的分支。四个出站各自的能力声明是既定事实，
// 没有哪一个在所有维度上都缺；手写一份全假的 Capabilities 会让测试脱离真实
// 声明，位名打错也只能到运行期才发现。按位翻、其余照抄真实声明，才能把诊断
// 结论归因到那几位上。
//
// 调用时 protoName 要传 base 本名：diagnose.go 里另有一批按协议名特判的
// 分支，换名字会连带改变它们的走向，测到的就不再是能力位了。
func capsWithout(t *testing.T, base string, fields ...string) proto.Capabilities {
	t.Helper()
	c := capsOf(t, base)
	for _, field := range fields {
		v := reflect.ValueOf(&c).Elem().FieldByName(field)
		if !v.IsValid() || v.Kind() != reflect.Bool {
			t.Fatalf("Capabilities 没有布尔位 %q", field)
		}
		if !v.Bool() {
			t.Fatalf("%s 的 %s 本来就是假，翻它测不出缺失分支", base, field)
		}
		v.SetBool(false)
	}
	return c
}

func imageReq(data, url string) *ir.Request {
	return &ir.Request{Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{
		{Type: ir.BlockImage, Image: &ir.Image{MediaType: "image/png", Data: data, URL: url}},
	}}}}
}

// 真实 Caps 下的图片形态诊断：四个出站的 Images 与 ImageURLs 全真，所以两条
// 分支都恒假。缺能力那一侧只能靠翻转能力位覆盖（见下两条测试），否则诊断
// 代码里那两个分支会长期无人走过、改坏了也没人知道。
func TestDiagnoseImageFormPerProtocol(t *testing.T) {
	for _, name := range []string{"anthropic", "openai-chat", "openai-responses"} {
		caps := capsOf(t, name)
		if !caps.Images {
			t.Fatalf("%s: Images 为假会让 URL 形态诊断被整协议分支吞掉", name)
		}
		base64Only := Diagnose(imageReq("AAAA", ""), name, caps)
		if len(base64Only) != 0 {
			t.Errorf("%s: base64 图片不应有诊断，得到 %v", name, base64Only)
		}
		urlOnly := Diagnose(imageReq("", "https://example.com/a.png"), name, caps)
		if len(urlOnly) != 0 {
			t.Errorf("%s 支持 URL 图片，不应报告：%v", name, urlOnly)
		}
	}
}

// 上游只收 base64、不收远程 URL：URL 形态的那一张要单独报出来。
func TestDiagnoseImageURLsUnsupportedReported(t *testing.T) {
	const base = "anthropic"
	caps := capsWithout(t, base, "ImageURLs")
	got := strings.Join(Diagnose(imageReq("", "https://example.com/a.png"), base, caps), "; ")
	if !strings.Contains(got, "inline base64 only") {
		t.Errorf("丢弃 URL 图片未报告：%q", got)
	}
}

// 同一块同时带 base64 与 URL：base64 能送达，不该报丢弃。
// （这是 Data=="" 那一半判据的唯一活口——只看 URL 非空会误报。）
func TestDiagnoseImageWithBothFormsNotDropped(t *testing.T) {
	const base = "anthropic"
	req := imageReq("AAAA", "https://example.com/a.png")
	if notes := Diagnose(req, base, capsWithout(t, base, "ImageURLs")); len(notes) != 0 {
		t.Errorf("同时带 base64 的图片能送达，不应报丢弃：%v", notes)
	}
}

// 两种图片同时出现且上游只收 base64：只报被丢的那一张。
func TestDiagnoseImageURLCountOnlyURLForm(t *testing.T) {
	req := &ir.Request{Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{
		{Type: ir.BlockImage, Image: &ir.Image{Data: "AAAA"}},
		{Type: ir.BlockImage, Image: &ir.Image{URL: "https://example.com/a.png"}},
		{Type: ir.BlockImage, Image: &ir.Image{URL: "https://example.com/b.png"}},
	}}}}
	const base = "anthropic"
	got := strings.Join(Diagnose(req, base, capsWithout(t, base, "ImageURLs")), "; ")
	if !strings.Contains(got, "dropped 2 image(s)") {
		t.Errorf("应只计 URL 形态的 2 张，得到 %q", got)
	}
}

// 整协议无图片能力时，仍走旧分支，不重复报 URL 形态。
func TestDiagnoseNoImagesSupersedesURLForm(t *testing.T) {
	caps := proto.Capabilities{Images: false, ImageURLs: false, Sampling: true}
	got := Diagnose(imageReq("", "https://example.com/a.png"), "hypothetical", caps)
	if len(got) != 1 || !strings.Contains(got[0], "no image input") {
		t.Errorf("无图片能力应只出一条整协议诊断，得到 %v", got)
	}
}

// 采样参数装不下时必须逐一点名：客户端调的参数全部无效，只报一句「有损」
// 读者无从判断该把哪个参数从请求里摘掉。
func TestDiagnoseSamplingDroppedWhenUnsupported(t *testing.T) {
	temp, topP := 0.7, 0.9
	req := &ir.Request{
		Temperature:   &temp,
		TopP:          &topP,
		StopSequences: []string{"STOP"},
		MaxTokens:     4096,
	}
	const base = "anthropic"
	got := strings.Join(Diagnose(req, base, capsWithout(t, base, "Sampling")), "; ")
	for _, want := range []string{"temperature", "top_p", "stop_sequences", "max_tokens"} {
		if !strings.Contains(got, want) {
			t.Errorf("缺 %s：%q", want, got)
		}
	}
	// 三个出站都能送采样参数，不应误报
	for _, name := range []string{"anthropic", "openai-chat", "openai-responses"} {
		if notes := Diagnose(req, name, capsOf(t, name)); len(notes) != 0 {
			t.Errorf("%s 支持采样参数，不应报告：%v", name, notes)
		}
	}
}

// 未设置的采样参数不列入；一个都没设时整条诊断不出现。
func TestDiagnoseSamplingPartialAndEmpty(t *testing.T) {
	const base = "anthropic"
	caps := capsWithout(t, base, "Sampling")
	temp := 0.3
	got := strings.Join(Diagnose(&ir.Request{Temperature: &temp}, base, caps), "; ")
	if !strings.Contains(got, "temperature") {
		t.Fatalf("应报 temperature：%q", got)
	}
	for _, absent := range []string{"top_p", "stop_sequences", "max_tokens"} {
		if strings.Contains(got, absent) {
			t.Errorf("未设置的 %s 不应出现：%q", absent, got)
		}
	}
	if notes := Diagnose(&ir.Request{}, base, caps); len(notes) != 0 {
		t.Errorf("无采样参数时不应报告：%v", notes)
	}
}

// top_k 只有 Anthropic 有原生字段；OpenAI 两系都要报丢弃。
func TestDiagnoseTopKPerProtocol(t *testing.T) {
	k := 40
	req := &ir.Request{TopK: &k}
	if notes := Diagnose(req, "anthropic", capsOf(t, "anthropic")); len(notes) != 0 {
		t.Errorf("anthropic 原生支持 top_k：%v", notes)
	}
	for _, name := range []string{"openai-chat", "openai-responses"} {
		got := strings.Join(Diagnose(req, name, capsOf(t, name)), "; ")
		if !strings.Contains(got, "top_k") {
			t.Errorf("%s 丢弃 top_k 未报告：%q", name, got)
		}
	}
	if notes := Diagnose(&ir.Request{}, "openai-chat", capsOf(t, "openai-chat")); len(notes) != 0 {
		t.Errorf("未设 top_k 不应报告：%v", notes)
	}
}

func TestDiagnoseDisableParallelPerProtocol(t *testing.T) {
	req := &ir.Request{ToolChoice: &ir.ToolChoice{Mode: ir.ChoiceAuto, DisableParallel: true}}
	for _, name := range []string{"anthropic", "openai-chat", "openai-responses"} {
		if notes := Diagnose(req, name, capsOf(t, name)); len(notes) != 0 {
			t.Errorf("%s 能表达禁止并行，不应报告：%v", name, notes)
		}
	}
	const base = "anthropic"
	got := strings.Join(Diagnose(req, base, capsWithout(t, base, "ParallelToolCalls")), "; ")
	if !strings.Contains(got, "parallel tool call") {
		t.Errorf("无法表达禁止并行时未报告：%q", got)
	}
	// 未禁止并行时不报
	allow := &ir.Request{ToolChoice: &ir.ToolChoice{Mode: ir.ChoiceAuto}}
	if notes := Diagnose(allow, base, capsWithout(t, base, "ParallelToolCalls")); len(notes) != 0 {
		t.Errorf("默认允许并行不应报告：%v", notes)
	}
}

// 能力位矩阵：把四个出站的真实声明钉死，任一 codec 改声明都要在此处显式更新。
func toolErrReq(isError bool) *ir.Request {
	return &ir.Request{Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{
		{Type: ir.BlockToolResult, ToolResult: &ir.ToolResult{
			ToolUseID: "toolu_1", IsError: isError,
			Content: []ir.Block{{Type: ir.BlockText, Text: "boom"}},
		}},
	}}}}
}

// 失败标志装不下时必须报出来：OpenAI 两系的载荷里没有这一维，静默丢掉会让
// 模型把报错当成正常返回值。anthropic 能表达，不得误报。
func TestDiagnoseToolResultErrorPerProtocol(t *testing.T) {
	const want = "cannot mark a tool call as failed"
	for name, shouldWarn := range map[string]bool{
		"anthropic":   false,
		"openai-chat": true, "openai-responses": true,
	} {
		t.Run(name, func(t *testing.T) {
			notes := strings.Join(Diagnose(toolErrReq(true), name, capsOf(t, name)), "; ")
			if got := strings.Contains(notes, want); got != shouldWarn {
				t.Errorf("含失败诊断 = %v, want %v；notes=%q", got, shouldWarn, notes)
			}
			if shouldWarn && !strings.Contains(notes, "1 tool result") {
				t.Errorf("未报出条数：%q", notes)
			}
		})
	}
}

// 结果没标失败时不得报：恒报会让这条诊断退化成噪声。
func TestDiagnoseToolResultSuccessNotReported(t *testing.T) {
	notes := strings.Join(Diagnose(toolErrReq(false), "openai-chat", capsOf(t, "openai-chat")), "; ")
	if strings.Contains(notes, "cannot mark a tool call as failed") {
		t.Errorf("成功结果不应触发诊断：%q", notes)
	}
}

func TestOutboundCapabilityMatrix(t *testing.T) {
	want := map[string]proto.Capabilities{
		// 媒体三位刻意不一致：anthropic 有 document 但没有音频入口，OpenAI 两系
		// 音频文档都有。三者全真或全假都是漏洞。
		"anthropic": {ThinkingSignature: true, Images: true, HostedTools: true, ThinkingForcedToolChoice: true,
			ImageURLs: true, Sampling: true, TopK: true, ParallelToolCalls: true, ToolResultError: true,
			// output_config.format 只有 json_schema 形态：结构化输出有槽位但纯 JSON 模式没有
			StructuredOutput: true, StructuredOutputSchemaOnly: true,
			Documents: true, Audio: false, Video: false, Refusal: false, Citations: true,
			ToolInputObject: true, UserID: true, ServiceTier: true, ToolStrict: true},
		// 调参五位刻意不一致：Chat 全有，Responses 只有对数概率且没有独立开关
		// （LogProbsViaTopN），anthropic 一个都没有。全真或全假都是漏洞。
		"openai-chat": {ThinkingSignature: false, Images: true, HostedTools: false, ThinkingForcedToolChoice: false,
			ImageURLs: true, Sampling: true, TopK: false, ParallelToolCalls: true, ToolResultError: false,
			StructuredOutput: true,
			Documents:        true, Audio: true, Video: false, Refusal: true, Citations: true,
			Penalties: true, Seed: true, Candidates: true, LogProbs: true, LogitBias: true, UserID: true,
			ServiceTier: true, PromptCacheKey: true, OpenAIExtras: true, ToolStrict: true},
		// 参数槽位刻意不一致：anthropic 是 JSON 对象槽，OpenAI 两系是字符串槽。
		"openai-responses": {ThinkingSignature: true, Images: true, HostedTools: true, ThinkingForcedToolChoice: true,
			ImageURLs: true, Sampling: true, TopK: false, ParallelToolCalls: true, ToolResultError: false,
			StructuredOutput: true,
			Documents:        true, Audio: true, Video: false, Refusal: true, Citations: true,
			LogProbs: true, LogProbsViaTopN: true, UserID: true, ResponseChain: true, ResponsesExtras: true,
			ServiceTier: true, PromptCacheKey: true, OpenAIExtras: true, ToolStrict: true},
	}
	for name, exp := range want {
		if got := capsOf(t, name); got != exp {
			t.Errorf("%s caps = %+v, want %+v", name, got, exp)
		}
	}
	// codex 是 openai-responses 的同形别名，能力必须一致
	if capsOf(t, "codex") != want["openai-responses"] {
		t.Error("codex 别名的能力声明与 openai-responses 不一致")
	}
	// 表必须覆盖全部已注册出站（codex 是别名，归 openai-responses 那条断言）：
	// 新注册一个 codec 却忘了在这里钉住能力，矩阵测试会静默少测一族。
	pinned := map[string]bool{"codex": true}
	for name := range want {
		pinned[name] = true
	}
	for _, name := range proto.OutboundNames() {
		if !pinned[name] {
			t.Errorf("出站 %q 未钉进能力矩阵", name)
		}
	}
}

// 结构化输出装不下时必须报出来：客户端会直接 JSON.parse 响应，拿到自由文本
// 是硬失败。四个出站都有这一维（anthropic 2026 起有 output_config.format），
// 所以缺失那一侧只能翻能力位覆盖。
func TestDiagnoseStructuredOutputPerProtocol(t *testing.T) {
	const want = "no structured output field"
	for _, name := range []string{"anthropic", "openai-chat", "openai-responses"} {
		t.Run(name, func(t *testing.T) {
			notes := strings.Join(Diagnose(formatReq(true), name, capsOf(t, name)), "; ")
			if strings.Contains(notes, want) {
				t.Errorf("%s 有结构化输出槽位，误报：%q", name, notes)
			}
		})
	}
	for _, name := range []string{"anthropic", "openai-chat", "openai-responses"} {
		t.Run(name+"/缺失", func(t *testing.T) {
			notes := strings.Join(Diagnose(formatReq(true), name, capsWithout(t, name, "StructuredOutput")), "; ")
			if !strings.Contains(notes, want) {
				t.Errorf("缺结构化输出槽位时未报：%q", notes)
			}
		})
	}
}

// 两档语义分开报：带 schema 与只要求合法 JSON，读者的补救动作不同
// （前者要把 schema 写进提示，后者只需一句话要求输出 JSON）。
// 无槽位时两档都装不下；anthropic 只装得下 schema 档，纯 JSON 模式要报受限措辞。
func TestDiagnoseDistinguishesSchemaFromJSONMode(t *testing.T) {
	const base = "anthropic"
	noSlot := capsWithout(t, base, "StructuredOutput")
	withSchema := strings.Join(Diagnose(formatReq(true), base, noSlot), "; ")
	if !strings.Contains(withSchema, "JSON schema constraint") {
		t.Errorf("带 schema 未报成 schema 约束：%q", withSchema)
	}
	jsonMode := strings.Join(Diagnose(formatReq(false), base, noSlot), "; ")
	if !strings.Contains(jsonMode, "JSON output mode") {
		t.Errorf("无 schema 未报成 JSON 模式：%q", jsonMode)
	}
	if strings.Contains(jsonMode, "schema") {
		t.Errorf("无 schema 却报成 schema 约束：%q", jsonMode)
	}
	// anthropic 的 output_config.format 只有 json_schema 一种 type：
	// schema 档直接送达不报；纯 JSON 模式档要说清「只接 schema 约束形态」。
	if notes := Diagnose(formatReq(true), base, capsOf(t, base)); len(notes) != 0 {
		t.Errorf("anthropic 接得住 schema 档，误报：%v", notes)
	}
	anthJSONMode := strings.Join(Diagnose(formatReq(false), base, capsOf(t, base)), "; ")
	if !strings.Contains(anthJSONMode, "only schema-constrained structured output") {
		t.Errorf("anthropic 纯 JSON 模式未报受限措辞：%q", anthJSONMode)
	}
}

// 客户端没要求结构化输出时不得报：恒报会让这条诊断退化成噪声。
func TestDiagnoseNoStructuredOutputNotReported(t *testing.T) {
	req := formatReq(true)
	req.ResponseFormat = nil
	notes := strings.Join(Diagnose(req, "anthropic", capsOf(t, "anthropic")), "; ")
	if strings.Contains(notes, "structured output") {
		t.Errorf("无诉求不应触发诊断：%q", notes)
	}
}

func formatReq(withSchema bool) *ir.Request {
	f := &ir.ResponseFormat{Name: "weather"}
	if withSchema {
		f.Schema = json.RawMessage(`{"type":"object"}`)
	}
	return &ir.Request{
		Model:          "m",
		MaxTokens:      100,
		Messages:       []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}},
		ResponseFormat: f,
	}
}
