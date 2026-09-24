package proto_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
	_ "github.com/aceaura/ModelSurge/agent/proto/anthropic"
	_ "github.com/aceaura/ModelSurge/agent/proto/gemini"
	_ "github.com/aceaura/ModelSurge/agent/proto/openaichat"
	_ "github.com/aceaura/ModelSurge/agent/proto/openairesponses"
)

// R100：tool_choice 的三类实测缺口（探针日志 r100_probe3.log / r100_probe4.log），
// 都不是照参考仓臆测：
//
//  1. 工具白名单解出即丢。Gemini 的 functionCallingConfig.allowedFunctionNames
//     只被用来把 ANY+单项折成指名调用，其余形态一律扔掉：客户端写明「必须调工具，
//     但只能是 alpha、beta」，出站把 gamma 一并声明出去，tool_choice 只剩
//     "required"。模型调到 gamma 时客户端根本没有那个函数可执行，请求与注记里都
//     看不出任何异常。Responses/codex 一族的同维度形态
//     tool_choice:{"type":"allowed_tools","mode":"required","tools":[…]} 更糟：
//     解码器的 map 分支只认 name，这个形态没有 name，于是整条 tool_choice 返回
//     nil——连内层的 required 也一起没了。
//
//  2. 指名调用指着一个没声明的工具，被原样编上线。四个出站都会把 ChoiceTool 写成
//     tool_choice，而接收侧要求那个名字必须落在 tools 里，这是必 400 的形状；报错
//     只说 tool_choice 无效，读者看不出起因是白名单里写了个没声明的名字。
//
//  3. 一个工具都没声明时 tool_choice 仍被原样写出，tools 键则因空数组被省略。
//     线上形状就是「指名调用一个不存在的函数」或「必须调工具，但没有工具」，四个
//     出站全部命中。这一态多半不是客户端写错的，而是本层自己造出来的：规整流水线
//     在无工具时已经把整段工具历史渲染成文本，请求彻底去工具化了，tool_choice 却
//     留在原地。
//
// 修法：ir.ToolChoice 增 AllowedTools 一维，两个解码器把它填上；四个出站都没有
// 白名单槽位，故靠收窄已声明工具等价实现（normalize.EnforceToolAllowlist），
// 收窄判据是 ir.Request.AllowlistNarrow 这一个纯函数，relay.Diagnose 报损耗用的
// 也是它，两处不可能漂移。2 与 3 由 normalize 回落/删除，Diagnose 报出。

// r100Outbound agent 侧的四个出站（codex 与 openai-responses 同形，各自独立断言）。
var r100Outbound = []string{"anthropic", "codex", "openai-chat", "openai-responses"}

// r100Gemini 造一条声明了 alpha/beta/gamma 三个函数的 Gemini 入站请求，
// toolConfig 原样拼进去，便于逐形态取证。
func r100Gemini(t *testing.T, toolConfig string) *ir.Request {
	t.Helper()
	body := `{"model":"m","contents":[{"role":"user","parts":[{"text":"hi"}]}],` +
		`"tools":[{"functionDeclarations":[` +
		`{"name":"alpha","parameters":{"type":"OBJECT"}},` +
		`{"name":"beta","parameters":{"type":"OBJECT"}},` +
		`{"name":"gamma","parameters":{"type":"OBJECT"}}]}],` +
		`"toolConfig":` + toolConfig + `}`
	return decodeIn(t, "gemini", body)
}

// r100ToolNames 出站 body 里声明了哪些工具名。按 key 数而不按字面量整段比对：
// 编码器换字段顺序或补一个键都不该让断言失效，「gamma 还在线上」才是结论本身。
func r100ToolNames(t *testing.T, body string) map[string]bool {
	t.Helper()
	var env struct {
		Tools []struct {
			Name     string `json:"name"`
			Function *struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("解析出站 body: %v\n%s", err, body)
	}
	out := map[string]bool{}
	for _, tl := range env.Tools {
		if tl.Name != "" {
			out[tl.Name] = true
		}
		if tl.Function != nil && tl.Function.Name != "" {
			out[tl.Function.Name] = true
		}
	}
	return out
}

// ---- 1. 白名单入 IR ----

// allowedFunctionNames 是独立于 mode 的一维，四个 mode 分支都得把它带进 IR。
// 此前只有 ANY+单项被间接用到（折成指名调用），其余全部解出即丢。
func TestGeminiAllowlistReachesIRInEveryMode(t *testing.T) {
	cases := []struct {
		label      string
		toolConfig string
		mode       ir.ChoiceMode
		allowed    []string
	}{
		{"ANY+双项", `{"functionCallingConfig":{"mode":"ANY","allowedFunctionNames":["alpha","beta"]}}`,
			ir.ChoiceAny, []string{"alpha", "beta"}},
		// ANY+单项折成指名调用是等价变换（必须调 ∧ 只能调 alpha ⇒ 必须调 alpha），
		// 且降级路径更温和：名字没声明时回落成 auto（模型可以不调），若保留 any 则
		// 变成「必须调一个被禁的工具」。白名单本身仍要带上，回落之后才看得出客户端
		// 原本要的是什么。
		{"ANY+单项折指名", `{"functionCallingConfig":{"mode":"ANY","allowedFunctionNames":["alpha"]}}`,
			ir.ChoiceTool, []string{"alpha"}},
		{"AUTO+双项", `{"functionCallingConfig":{"mode":"AUTO","allowedFunctionNames":["alpha","beta"]}}`,
			ir.ChoiceAuto, []string{"alpha", "beta"}},
		{"小写 any", `{"functionCallingConfig":{"mode":"any","allowedFunctionNames":["alpha","beta"]}}`,
			ir.ChoiceAny, []string{"alpha", "beta"}},
		{"缺 mode 走 auto", `{"functionCallingConfig":{"allowedFunctionNames":["alpha","beta"]}}`,
			ir.ChoiceAuto, []string{"alpha", "beta"}},
		{"NONE+白名单", `{"functionCallingConfig":{"mode":"NONE","allowedFunctionNames":["alpha"]}}`,
			ir.ChoiceNone, []string{"alpha"}},
	}
	for _, c := range cases {
		req := r100Gemini(t, c.toolConfig)
		if req.ToolChoice == nil {
			t.Fatalf("[%s] ToolChoice 为 nil", c.label)
		}
		if req.ToolChoice.Mode != c.mode {
			t.Errorf("[%s] Mode = %q, want %q", c.label, req.ToolChoice.Mode, c.mode)
		}
		if strings.Join(req.ToolChoice.AllowedTools, ",") != strings.Join(c.allowed, ",") {
			t.Errorf("[%s] AllowedTools = %v, want %v", c.label, req.ToolChoice.AllowedTools, c.allowed)
		}
	}
}

// 没有白名单时不得凭空造一个空切片出来：那一维缺省就该是缺省，Clone 也不许把它
// 写成 null 之外的东西（R98 的同一纪律）。
func TestGeminiNoAllowlistStaysAbsent(t *testing.T) {
	req := r100Gemini(t, `{"functionCallingConfig":{"mode":"ANY"}}`)
	if req.ToolChoice.AllowedTools != nil {
		t.Errorf("无白名单却解出了 %v", req.ToolChoice.AllowedTools)
	}
}

// Responses/codex 一族的 allowed_tools 形态：type 说的是「这是一条选择策略」而不是
// 某个已声明工具的类型，内层 mode 才说要不要必须调。此前 map 分支只认 name，
// 这个形态没有 name，于是整条 tool_choice 被丢成 nil。
func TestResponsesAllowedToolsShapeDecodes(t *testing.T) {
	cases := []struct {
		label   string
		choice  string
		mode    ir.ChoiceMode
		allowed []string
	}{
		{"required+函数条目", `{"type":"allowed_tools","mode":"required","tools":[{"type":"function","name":"alpha"},{"type":"function","name":"beta"}]}`,
			ir.ChoiceAny, []string{"alpha", "beta"}},
		{"auto+函数条目", `{"type":"allowed_tools","mode":"auto","tools":[{"type":"function","name":"alpha"}]}`,
			ir.ChoiceAuto, []string{"alpha"}},
		{"缺 mode 走 auto", `{"type":"allowed_tools","tools":[{"type":"function","name":"alpha"}]}`,
			ir.ChoiceAuto, []string{"alpha"}},
		{"裸字符串条目", `{"type":"allowed_tools","mode":"required","tools":["alpha","beta"]}`,
			ir.ChoiceAny, []string{"alpha", "beta"}},
		{"嵌套 function 条目", `{"type":"allowed_tools","mode":"required","tools":[{"function":{"name":"alpha"}}]}`,
			ir.ChoiceAny, []string{"alpha"}},
	}
	for _, c := range cases {
		body := `{"model":"m","input":[{"role":"user","content":"hi"}],"tool_choice":` + c.choice + `}`
		req := decodeIn(t, "openai-responses", body)
		if req.ToolChoice == nil {
			t.Fatalf("[%s] 整条 tool_choice 被丢了", c.label)
		}
		if req.ToolChoice.Mode != c.mode {
			t.Errorf("[%s] Mode = %q, want %q", c.label, req.ToolChoice.Mode, c.mode)
		}
		if strings.Join(req.ToolChoice.AllowedTools, ",") != strings.Join(c.allowed, ",") {
			t.Errorf("[%s] AllowedTools = %v, want %v", c.label, req.ToolChoice.AllowedTools, c.allowed)
		}
	}
}

// 认不出来的条目宁可当没有：凭空捏一个名字进白名单，收窄就会删掉本该保留的工具，
// 那比少收窄一处严重得多。
func TestResponsesAllowedToolsIgnoresUnreadableEntries(t *testing.T) {
	body := `{"model":"m","input":[{"role":"user","content":"hi"}],` +
		`"tool_choice":{"type":"allowed_tools","mode":"required","tools":[42,null,{"type":"function"},{"name":""},"alpha"]}}`
	req := decodeIn(t, "openai-responses", body)
	if got := strings.Join(req.ToolChoice.AllowedTools, ","); got != "alpha" {
		t.Errorf("AllowedTools = %q, want 只留下读得出名字的 alpha", got)
	}
}

// ---- 2. 白名单靠收窄落地：四个出站都不得把被禁的工具递上去 ----

// 这是本轮的主结论。客户端说「只能调 alpha、beta」，gamma 就不该出现在任何一个
// 出站的 tools 里——它在，模型就能调它，而客户端没有那个函数可执行。
func TestAllowlistNarrowsDeclaredToolsOnEveryOutbound(t *testing.T) {
	req := r100Gemini(t, `{"functionCallingConfig":{"mode":"ANY","allowedFunctionNames":["alpha","beta"]}}`)
	for _, name := range r100Outbound {
		body := encodeOut(t, name, req)
		got := r100ToolNames(t, body)
		if got["gamma"] {
			t.Errorf("%s 出站把白名单排除的 gamma 也声明了出去：%s", name, body)
		}
		if !got["alpha"] || !got["beta"] {
			t.Errorf("%s 出站把白名单内的工具也删了：%v", name, got)
		}
		// 「必须调工具」这一维不能被收窄顺手吃掉。
		if name == "anthropic" {
			if !strings.Contains(body, `"type":"any"`) {
				t.Errorf("%s 出站丢了强制调用：%s", name, body)
			}
		} else if !strings.Contains(body, `"required"`) {
			t.Errorf("%s 出站丢了强制调用：%s", name, body)
		}
	}
}

// AUTO 档同样要收窄：白名单与 mode 正交，「可以不调」不等于「什么都能调」。
func TestAllowlistNarrowsUnderAutoMode(t *testing.T) {
	req := r100Gemini(t, `{"functionCallingConfig":{"mode":"AUTO","allowedFunctionNames":["gamma"]}}`)
	for _, name := range r100Outbound {
		if got := r100ToolNames(t, encodeOut(t, name, req)); got["alpha"] || got["beta"] {
			t.Errorf("%s 出站没按 AUTO 档的白名单收窄：%v", name, got)
		}
	}
}

// 托管工具一律保留。Gemini 的 allowedFunctionNames 管的是 functionDeclarations，
// 把 google_search 连带删掉是删了客户端声明过的东西，那是比白名单失效更糟的结果。
// 白名单给两项（其中一项未声明）以避免落进 ANY+单项折指名那一档——指名档不收窄，
// 测不出托管工具的保留。
func TestAllowlistKeepsHostedTools(t *testing.T) {
	body := `{"model":"m","contents":[{"role":"user","parts":[{"text":"hi"}]}],` +
		`"tools":[{"googleSearch":{}},{"functionDeclarations":[` +
		`{"name":"alpha","parameters":{"type":"OBJECT"}},` +
		`{"name":"gamma","parameters":{"type":"OBJECT"}}]}],` +
		`"toolConfig":{"functionCallingConfig":{"mode":"ANY","allowedFunctionNames":["alpha","beta"]}}}`
	req := decodeIn(t, "gemini", body)
	out := encodeOut(t, "anthropic", req)
	if !strings.Contains(out, "web_search") {
		t.Errorf("托管搜索工具被白名单连带删了：%s", out)
	}
	if got := r100ToolNames(t, out); got["gamma"] || !got["alpha"] {
		t.Errorf("收窄结果 = %v, want 托管 + alpha，gamma 出局", got)
	}
}

// 白名单与已声明工具全无交集时不得收窄到零个工具。收窄到零会触发
// StripToolsIfNoTools，把整段工具历史改写成文本——那是比白名单失效大得多的破坏。
// 这种情况原样发出，落差由 relay.Diagnose 报出。
// 白名单给两项：ANY+单项会折成指名调用，走不到收窄这条路径（变异实测抓出来的）。
func TestAllowlistNeverNarrowsToZeroTools(t *testing.T) {
	req := r100Gemini(t, `{"functionCallingConfig":{"mode":"ANY","allowedFunctionNames":["nosuch","alsono"]}}`)
	for _, name := range r100Outbound {
		if got := r100ToolNames(t, encodeOut(t, name, req)); len(got) != 3 {
			t.Errorf("%s 出站收窄到了 %v，白名单落空时三个工具都该原样保留", name, got)
		}
	}
}

// 白名单里有未声明者、但至少一个命中时，按命中者收窄即可：没声明的名字压根不存在，
// 模型调不到它，客户端要的限制照样成立，这不是损耗。
func TestAllowlistPartialMatchStillNarrows(t *testing.T) {
	req := r100Gemini(t, `{"functionCallingConfig":{"mode":"ANY","allowedFunctionNames":["alpha","nosuch"]}}`)
	for _, name := range r100Outbound {
		got := r100ToolNames(t, encodeOut(t, name, req))
		if !got["alpha"] || got["beta"] || got["gamma"] {
			t.Errorf("%s 出站收窄结果 = %v, want 只剩 alpha", name, got)
		}
	}
}

// 指名调用与禁止调用都不收窄：前者上游本来就只会调那一个，后者一个都不调，多声明
// 的工具调不到，删掉只是白丢客户端的声明。
func TestAllowlistDoesNotNarrowForcedOrNoneModes(t *testing.T) {
	for _, tc := range []string{
		`{"functionCallingConfig":{"mode":"ANY","allowedFunctionNames":["alpha"]}}`,
		`{"functionCallingConfig":{"mode":"NONE","allowedFunctionNames":["alpha"]}}`,
	} {
		req := r100Gemini(t, tc)
		for _, name := range r100Outbound {
			if got := r100ToolNames(t, encodeOut(t, name, req)); len(got) != 3 {
				t.Errorf("%s 在 %s 下收窄成了 %v，这两档不该动工具列表", name, tc, got)
			}
		}
	}
}

// 收窄只动工具声明，不动历史：被排除工具的历史调用与结果必须照原样过（块型保持
// tool_use / tool_result），否则上游会看到一段被改写成文本的历史，与它自己上一轮
// 发出的调用对不上。
func TestAllowlistNarrowingLeavesToolHistoryIntact(t *testing.T) {
	body := `{"model":"m","contents":[` +
		`{"role":"user","parts":[{"text":"run gamma"}]},` +
		`{"role":"model","parts":[{"functionCall":{"name":"gamma","args":{"x":1}}}]},` +
		`{"role":"user","parts":[{"functionResponse":{"name":"gamma","response":{"result":"ok"}}}]}` +
		`],"tools":[{"functionDeclarations":[` +
		`{"name":"alpha","parameters":{"type":"OBJECT"}},` +
		`{"name":"gamma","parameters":{"type":"OBJECT"}}]}],` +
		`"toolConfig":{"functionCallingConfig":{"mode":"AUTO","allowedFunctionNames":["alpha"]}}}`
	req := decodeIn(t, "gemini", body)
	for _, name := range r100Outbound {
		out := encodeOut(t, name, req)
		if got := r100ToolNames(t, out); got["gamma"] {
			t.Errorf("%s 出站仍声明 gamma：%v", name, got)
		}
		if strings.Contains(out, "[Tool:") || strings.Contains(out, "[Tool Result") {
			t.Errorf("%s 把 gamma 的历史改写成了文本：%s", name, out)
		}
		if !strings.Contains(out, "ok") {
			t.Errorf("%s 丢了 gamma 历史里的工具结果正文：%s", name, out)
		}
	}
}

// ---- 3. 两个必 400 的形状不得上线 ----

// 指名调用指着一个没声明的工具：回落成 auto，那个名字一个出站都不许出现。
func TestForcedUndeclaredToolNeverReachesTheWire(t *testing.T) {
	req := r100Gemini(t, `{"functionCallingConfig":{"mode":"ANY","allowedFunctionNames":["nosuch"]}}`)
	for _, name := range r100Outbound {
		out := encodeOut(t, name, req)
		if strings.Contains(out, "nosuch") {
			t.Errorf("%s 出站把未声明的指名工具写上了线（接收侧必 400）：%s", name, out)
		}
		if !strings.Contains(out, "auto") {
			t.Errorf("%s 出站没回落成 auto：%s", name, out)
		}
	}
}

// 指名已声明的工具时不许回落——那是能用的请求，改坏它比不改更糟。
func TestForcedDeclaredToolIsNotRelaxed(t *testing.T) {
	req := r100Gemini(t, `{"functionCallingConfig":{"mode":"ANY","allowedFunctionNames":["beta"]}}`)
	// ANY+单项折成指名调用，beta 已声明，须原样保留。
	if req.ToolChoice.Mode != ir.ChoiceTool || req.ToolChoice.ToolName != "beta" {
		t.Fatalf("入站没折成指名 beta：%+v", req.ToolChoice)
	}
	for _, name := range r100Outbound {
		if out := encodeOut(t, name, req); !strings.Contains(out, "beta") {
			t.Errorf("%s 出站丢了指名调用：%s", name, out)
		}
	}
}

// 一个工具都没声明时，要求有工具的 tool_choice 必须整条删掉。tools 键会因空数组
// 被省略，留着 tool_choice 就是「必须调工具，但没有工具」——四个出站全部命中。
func TestToolChoiceDroppedWhenNoToolsDeclared(t *testing.T) {
	for _, choice := range []string{`"required"`, `{"type":"function","function":{"name":"alpha"}}`} {
		body := `{"model":"m","messages":[{"role":"user","content":"hi"}],"tool_choice":` + choice + `}`
		req := decodeIn(t, "openai-chat", body)
		if len(req.Tools) != 0 {
			t.Fatalf("夹具应零工具，得到 %v", req.Tools)
		}
		for _, name := range r100Outbound {
			if out := encodeOut(t, name, req); strings.Contains(out, "tool_choice") {
				t.Errorf("%s 在零工具下仍写出 tool_choice（接收侧必 400）：%s", name, out)
			}
		}
	}
}

// auto 与 none 在零工具下是接收侧接受的形状，且与「不带 tool_choice」同义：不删，
// 也不报损耗。这条钉住判据只认 any/tool 两档，免得被放宽成「零工具就一律删」。
func TestToolChoiceAutoSurvivesZeroTools(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}],"tool_choice":"auto"}`
	req := decodeIn(t, "openai-chat", body)
	for _, name := range r100Outbound {
		if out := encodeOut(t, name, req); !strings.Contains(out, "auto") {
			t.Errorf("%s 把零工具下的 auto 也删了：%s", name, out)
		}
	}
}

// 本层自己造出这一态的路径也要盖住：声明了工具但历史里有工具痕迹、且工具列表随后
// 被清空的情形之外，最常见的是客户端发了 tool_choice 却没发 tools，同时历史里带着
// 上一轮的工具调用。此时历史被渲染成文本，tool_choice 也必须一起消失。
func TestToolChoiceDroppedAlongWithStrippedToolHistory(t *testing.T) {
	req := &ir.Request{Model: "m", MaxTokens: 16,
		ToolChoice: &ir.ToolChoice{Mode: ir.ChoiceTool, ToolName: "alpha"},
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}},
			{Role: ir.RoleAssistant, Content: []ir.Block{{Type: ir.BlockToolUse,
				ToolUse: &ir.ToolUse{ID: "t1", Name: "alpha", Input: json.RawMessage(`{}`)}}}},
			{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockToolResult,
				ToolResult: &ir.ToolResult{ToolUseID: "t1",
					Content: []ir.Block{{Type: ir.BlockText, Text: "ok"}}}}}},
		}}
	for _, name := range r100Outbound {
		out := encodeOut(t, name, req)
		if strings.Contains(out, "tool_choice") {
			t.Errorf("%s 去工具化之后仍写出 tool_choice：%s", name, out)
		}
		// 历史照旧渲染成文本，正文一个字都不许丢。
		if !strings.Contains(out, "ok") || !strings.Contains(out, "hi") {
			t.Errorf("%s 历史正文丢了：%s", name, out)
		}
	}
}

// ---- 4. 不得凭空发明线上形状 ----

// 白名单靠收窄实现，不是靠发一个没核实过的键。四个出站都不许写出 allowed_tools /
// allowedFunctionNames：前者是 codex 后端的选择策略扩展，发给真 OpenAI 会被按
// tool_choice 类型校验拒掉；后者是 Gemini 的键名，四家出站没有一家认。
func TestNoAllowlistKeyIsInventedOnOutbound(t *testing.T) {
	req := r100Gemini(t, `{"functionCallingConfig":{"mode":"ANY","allowedFunctionNames":["alpha","beta"]}}`)
	for _, name := range r100Outbound {
		out := encodeOut(t, name, req)
		for _, key := range []string{"allowed_tools", "allowedFunctionNames", "allowed_tool_names"} {
			if strings.Contains(out, key) {
				t.Errorf("%s 出站发明了白名单键 %s：%s", name, key, out)
			}
		}
	}
}

// 同族往返也不发明：Responses 客户端发来的 allowed_tools 投回 Responses 上游时
// 同样走收窄，而不是把那个扩展形态原样抄回去。
func TestAllowlistNotEchoedOnSameFamilyRoundTrip(t *testing.T) {
	body := `{"model":"m","input":[{"role":"user","content":"hi"}],` +
		`"tools":[{"type":"function","name":"alpha","parameters":{"type":"object"}},` +
		`{"type":"function","name":"gamma","parameters":{"type":"object"}}],` +
		`"tool_choice":{"type":"allowed_tools","mode":"required","tools":[{"type":"function","name":"alpha"}]}}`
	req := decodeIn(t, "openai-responses", body)
	out := encodeOut(t, "openai-responses", req)
	if strings.Contains(out, "allowed_tools") {
		t.Errorf("同族往返抄回了扩展形态：%s", out)
	}
	if got := r100ToolNames(t, out); got["gamma"] || !got["alpha"] {
		t.Errorf("同族往返没按白名单收窄：%v", out)
	}
}
