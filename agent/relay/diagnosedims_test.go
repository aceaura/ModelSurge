package relay

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
	"github.com/aceaura/ModelSurge/agent/proto"
	_ "github.com/aceaura/ModelSurge/agent/proto/anthropic"
	_ "github.com/aceaura/ModelSurge/agent/proto/kiro"
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

func imageReq(data, url string) *ir.Request {
	return &ir.Request{Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{
		{Type: ir.BlockImage, Image: &ir.Image{MediaType: "image/png", Data: data, URL: url}},
	}}}}
}

// 真实 Caps 下的图片形态诊断：四个出站的 Images 全真，所以「整协议无图片」
// 分支恒假；真正会丢的是 URL 形态（只有 kiro 装不下）。
func TestDiagnoseImageFormPerProtocol(t *testing.T) {
	for _, name := range []string{"anthropic", "openai-chat", "openai-responses", "kiro"} {
		caps := capsOf(t, name)
		if !caps.Images {
			t.Fatalf("%s: Images 为假会让 URL 形态诊断被整协议分支吞掉", name)
		}
		base64Only := Diagnose(imageReq("AAAA", ""), name, caps)
		if len(base64Only) != 0 {
			t.Errorf("%s: base64 图片不应有诊断，得到 %v", name, base64Only)
		}
		urlOnly := strings.Join(Diagnose(imageReq("", "https://example.com/a.png"), name, caps), "; ")
		if name == "kiro" {
			if !strings.Contains(urlOnly, "inline base64 only") {
				t.Errorf("kiro 丢弃 URL 图片未报告：%q", urlOnly)
			}
			continue
		}
		if urlOnly != "" {
			t.Errorf("%s 支持 URL 图片，不应报告：%q", name, urlOnly)
		}
	}
}

// 同一块同时带 base64 与 URL：base64 能送达，不该报丢弃。
// （这是 Data=="" 那一半判据的唯一活口——只看 URL 非空会误报。）
func TestDiagnoseImageWithBothFormsNotDropped(t *testing.T) {
	req := imageReq("AAAA", "https://example.com/a.png")
	if notes := Diagnose(req, "kiro", capsOf(t, "kiro")); len(notes) != 0 {
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
	got := strings.Join(Diagnose(req, "kiro", capsOf(t, "kiro")), "; ")
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

func TestDiagnoseSamplingDroppedOnKiro(t *testing.T) {
	temp, topP := 0.7, 0.9
	req := &ir.Request{
		Temperature:   &temp,
		TopP:          &topP,
		StopSequences: []string{"STOP"},
		MaxTokens:     4096,
	}
	got := strings.Join(Diagnose(req, "kiro", capsOf(t, "kiro")), "; ")
	for _, want := range []string{"temperature", "top_p", "stop_sequences", "max_tokens"} {
		if !strings.Contains(got, want) {
			t.Errorf("缺 %s：%q", want, got)
		}
	}
	// 三个 OpenAI/Anthropic 出站都能送采样参数，不应误报
	for _, name := range []string{"anthropic", "openai-chat", "openai-responses"} {
		if notes := Diagnose(req, name, capsOf(t, name)); len(notes) != 0 {
			t.Errorf("%s 支持采样参数，不应报告：%v", name, notes)
		}
	}
}

// 未设置的采样参数不列入；一个都没设时整条诊断不出现。
func TestDiagnoseSamplingPartialAndEmpty(t *testing.T) {
	kiro := capsOf(t, "kiro")
	temp := 0.3
	got := strings.Join(Diagnose(&ir.Request{Temperature: &temp}, "kiro", kiro), "; ")
	if !strings.Contains(got, "temperature") {
		t.Fatalf("应报 temperature：%q", got)
	}
	for _, absent := range []string{"top_p", "stop_sequences", "max_tokens"} {
		if strings.Contains(got, absent) {
			t.Errorf("未设置的 %s 不应出现：%q", absent, got)
		}
	}
	if notes := Diagnose(&ir.Request{}, "kiro", kiro); len(notes) != 0 {
		t.Errorf("无采样参数时不应报告：%v", notes)
	}
}

// top_k 只有 Anthropic 有原生字段；OpenAI 两系与 kiro 都要报丢弃。
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
	// kiro 会同时报采样与 top_k 两条（两者是不同维度，不合并）
	got := Diagnose(req, "kiro", capsOf(t, "kiro"))
	if len(got) != 1 || !strings.Contains(got[0], "top_k") {
		t.Errorf("kiro 仅设 top_k 时应只出 top_k 一条：%v", got)
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
	got := strings.Join(Diagnose(req, "kiro", capsOf(t, "kiro")), "; ")
	if !strings.Contains(got, "parallel tool call") {
		t.Errorf("kiro 无法表达禁止并行，未报告：%q", got)
	}
	// 未禁止并行时不报
	allow := &ir.Request{ToolChoice: &ir.ToolChoice{Mode: ir.ChoiceAuto}}
	if notes := Diagnose(allow, "kiro", capsOf(t, "kiro")); len(notes) != 0 {
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
// 模型把报错当成正常返回值。anthropic 与 kiro 能表达，不得误报。
func TestDiagnoseToolResultErrorPerProtocol(t *testing.T) {
	const want = "cannot mark a tool call as failed"
	for name, shouldWarn := range map[string]bool{
		"anthropic": false, "kiro": false,
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
		// 音频文档都有，kiro 一个都没有。三者全真或全假都是漏洞。
		"anthropic": {ThinkingSignature: true, Images: true, HostedTools: true, ThinkingForcedToolChoice: true,
			ImageURLs: true, Sampling: true, TopK: true, ParallelToolCalls: true, ToolResultError: true,
			StructuredOutput: false,
			Documents:        true, Audio: false, Video: false, Refusal: false, Citations: true,
			ToolInputObject: true, UserID: true},
		// 调参五位刻意不一致：Chat 全有，Responses 只有对数概率且没有独立开关
		// （LogProbsViaTopN），anthropic 与 kiro 一个都没有。全真或全假都是漏洞。
		"openai-chat": {ThinkingSignature: false, Images: true, HostedTools: false, ThinkingForcedToolChoice: false,
			ImageURLs: true, Sampling: true, TopK: false, ParallelToolCalls: true, ToolResultError: false,
			StructuredOutput: true,
			Documents:        true, Audio: true, Video: false, Refusal: true, Citations: true,
			Penalties: true, Seed: true, Candidates: true, LogProbs: true, LogitBias: true, UserID: true},
		"openai-responses": {ThinkingSignature: true, Images: true, HostedTools: true, ThinkingForcedToolChoice: true,
			ImageURLs: true, Sampling: true, TopK: false, ParallelToolCalls: true, ToolResultError: false,
			StructuredOutput: true,
			Documents:        true, Audio: true, Video: false, Refusal: true, Citations: true,
			LogProbs: true, LogProbsViaTopN: true, UserID: true, ResponseChain: true},
		"kiro": {ThinkingSignature: false, Images: true, HostedTools: true, ThinkingForcedToolChoice: true,
			ImageURLs: false, Sampling: false, TopK: false, ParallelToolCalls: false, ToolResultError: true,
			StructuredOutput: false,
			Documents:        false, Audio: false, Video: false, Refusal: false, Citations: false,
			// 参数槽位刻意不一致：anthropic/kiro 是 JSON 对象槽，OpenAI 两系是字符串槽。
			ToolInputObject: true},
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
}

// 结构化输出装不下时必须报出来：客户端会直接 JSON.parse 响应，拿到自由文本
// 是硬失败。anthropic 与 kiro 的载荷里没有这一维，OpenAI 两系有，不得误报。
func TestDiagnoseStructuredOutputPerProtocol(t *testing.T) {
	const want = "no structured output field"
	for name, shouldWarn := range map[string]bool{
		"anthropic": true, "kiro": true,
		"openai-chat": false, "openai-responses": false,
	} {
		t.Run(name, func(t *testing.T) {
			notes := strings.Join(Diagnose(formatReq(true), name, capsOf(t, name)), "; ")
			if got := strings.Contains(notes, want); got != shouldWarn {
				t.Errorf("含结构化输出诊断 = %v, want %v；notes=%q", got, shouldWarn, notes)
			}
		})
	}
}

// 两档语义分开报：带 schema 与只要求合法 JSON，读者的补救动作不同
// （前者要把 schema 写进提示，后者只需一句话要求输出 JSON）。
func TestDiagnoseDistinguishesSchemaFromJSONMode(t *testing.T) {
	withSchema := strings.Join(Diagnose(formatReq(true), "anthropic", capsOf(t, "anthropic")), "; ")
	if !strings.Contains(withSchema, "JSON schema constraint") {
		t.Errorf("带 schema 未报成 schema 约束：%q", withSchema)
	}
	jsonMode := strings.Join(Diagnose(formatReq(false), "anthropic", capsOf(t, "anthropic")), "; ")
	if !strings.Contains(jsonMode, "JSON output mode") {
		t.Errorf("无 schema 未报成 JSON 模式：%q", jsonMode)
	}
	if strings.Contains(jsonMode, "schema") {
		t.Errorf("无 schema 却报成 schema 约束：%q", jsonMode)
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
