package relay

import (
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
	"github.com/aceaura/ModelSurge/agent/proto"
)

// R100 请求侧诊断：tool_choice 的三态。三者都不是「少个字段」那么轻——前两态是
// 接收侧必 400 的形状，第三态是客户端明令禁止的工具被照样递到模型面前。
//
//   - 白名单落空：客户端说「只能调这些」，而这些名字一个都没声明。无从收窄，
//     整个工具列表原样发出，模型随时可能调一个客户端没有实现来承接的函数。
//   - 指名调用指着未声明的工具：normalize 已回落成 auto，这一轮能跑完，但客户端
//     要的强制没了，必须报出来。
//   - 零工具却带着要求有工具的 tool_choice：tools 键因空数组被省略，线上就是
//     「必须调工具，但没有工具」。normalize 已整条删掉，同样要报。
//
// 收窄成功时一律静默：那是白名单被如实实现，不是损耗。

// r100Notes 取真实能力声明下的诊断结论，拼成一句便于断言。
func r100Notes(t *testing.T, req *ir.Request, outbound string) string {
	t.Helper()
	return strings.Join(Diagnose(req, outbound, capsOf(t, outbound)), "; ")
}

func r100Req(tc *ir.ToolChoice, tools ...string) *ir.Request {
	req := &ir.Request{ToolChoice: tc, Messages: []ir.Message{
		{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}}}
	for _, n := range tools {
		req.Tools = append(req.Tools, ir.Tool{Name: n})
	}
	return req
}

// ---- 白名单 ----

// 白名单落空必须报，且在四个出站上都报：这一态与目标协议的能力无关，谁都装不下
// 一个「一个名字都对不上」的白名单。
func TestDiagnoseReportsUnenforceableAllowlist(t *testing.T) {
	req := r100Req(&ir.ToolChoice{Mode: ir.ChoiceAny, AllowedTools: []string{"nosuch"}},
		"alpha", "beta", "gamma")
	for _, outbound := range []string{"anthropic", "codex", "openai-chat", "openai-responses"} {
		got := r100Notes(t, req, outbound)
		if !strings.Contains(got, "allowlist") {
			t.Errorf("%s 没报出白名单落空：%q", outbound, got)
		}
		if !strings.Contains(got, "1 name(s)") {
			t.Errorf("%s 没说清落空的白名单有几项：%q", outbound, got)
		}
		if !strings.Contains(got, "may call tools the client excluded") {
			t.Errorf("%s 的措辞没说清后果是被禁工具照样可调：%q", outbound, got)
		}
	}
}

// 收窄成功时静默。这一条与上一条同等重要：白名单被如实实现不是损耗，照报会让
// 每一次正常请求都带上一条假注记，读者很快就学会忽略这个头。
func TestDiagnoseSilentWhenAllowlistIsEnforceable(t *testing.T) {
	for _, tc := range []*ir.ToolChoice{
		{Mode: ir.ChoiceAny, AllowedTools: []string{"alpha", "beta"}},
		{Mode: ir.ChoiceAuto, AllowedTools: []string{"alpha", "nosuch"}},
	} {
		req := r100Req(tc, "alpha", "beta", "gamma")
		for _, outbound := range []string{"anthropic", "codex", "openai-chat", "openai-responses"} {
			if got := r100Notes(t, req, outbound); strings.Contains(got, "allowlist") {
				t.Errorf("白名单 %v 可以收窄，%s 却报了损耗：%q", tc.AllowedTools, outbound, got)
			}
		}
	}
}

// 指名与禁止两档不靠收窄实现（前者上游只会调那一个，后者一个都不调），不得误报成
// 「限制失效」。
func TestDiagnoseSilentOnAllowlistWithForcedOrNoneMode(t *testing.T) {
	for _, mode := range []ir.ChoiceMode{ir.ChoiceTool, ir.ChoiceNone} {
		req := r100Req(&ir.ToolChoice{Mode: mode, ToolName: "alpha", AllowedTools: []string{"nosuch"}},
			"alpha", "beta")
		for _, outbound := range []string{"anthropic", "codex", "openai-chat", "openai-responses"} {
			if got := r100Notes(t, req, outbound); strings.Contains(got, "allowlist") {
				t.Errorf("%s 档不该报白名单损耗，%s 报了：%q", mode, outbound, got)
			}
		}
	}
}

// 托管工具不计入交集：白名单只写托管工具的名字时，非托管工具一个都对不上，
// 收窄会把它们全删光，故按落空处理并报出。
func TestDiagnoseAllowlistGapIgnoresHostedTools(t *testing.T) {
	req := &ir.Request{
		ToolChoice: &ir.ToolChoice{Mode: ir.ChoiceAny, AllowedTools: []string{"google_search"}},
		Tools: []ir.Tool{
			{Name: "google_search", Hosted: ir.HostedWebSearch},
			{Name: "alpha"},
		},
	}
	if got := r100Notes(t, req, "anthropic"); !strings.Contains(got, "allowlist") {
		t.Errorf("白名单只命中托管工具时应按落空报出：%q", got)
	}
}

// ---- 指名未声明 ----

// 指名调用指着未声明的工具：回落成 auto 是补救，但客户端要的强制没了，必须报，
// 且要把那个名字说出来——读者得知道是哪个名字写错了。
func TestDiagnoseReportsForcedUndeclaredTool(t *testing.T) {
	req := r100Req(&ir.ToolChoice{Mode: ir.ChoiceTool, ToolName: "nosuch"}, "alpha", "beta")
	for _, outbound := range []string{"anthropic", "codex", "openai-chat", "openai-responses"} {
		got := r100Notes(t, req, outbound)
		if !strings.Contains(got, `"nosuch"`) {
			t.Errorf("%s 没说出是哪个名字没声明：%q", outbound, got)
		}
		if !strings.Contains(got, "downgraded tool_choice to auto") {
			t.Errorf("%s 没说清补救是回落成 auto：%q", outbound, got)
		}
	}
}

// 指名已声明的工具时静默：那是能用的请求。
func TestDiagnoseSilentOnForcedDeclaredTool(t *testing.T) {
	req := r100Req(&ir.ToolChoice{Mode: ir.ChoiceTool, ToolName: "beta"}, "alpha", "beta")
	for _, outbound := range []string{"anthropic", "codex", "openai-chat", "openai-responses"} {
		if got := r100Notes(t, req, outbound); got != "" {
			t.Errorf("指名已声明工具不该有任何注记，%s 报了：%q", outbound, got)
		}
	}
}

// 回落与思考降级是同一次改动的两个说法，只能报一条。两条都出等于把一次回落说两遍，
// 读者会以为是两处独立的降级。
func TestDiagnoseForcedUndeclaredSupersedesThinkingDowngrade(t *testing.T) {
	req := r100Req(&ir.ToolChoice{Mode: ir.ChoiceTool, ToolName: "nosuch"}, "alpha")
	req.Thinking = &ir.ThinkingConfig{Enabled: true, Effort: "high"}
	got := Diagnose(req, "openai-chat", capsOf(t, "openai-chat"))
	if len(got) != 1 {
		t.Fatalf("应只出一条注记，得到 %d 条：%v", len(got), got)
	}
	if !strings.Contains(got[0], `"nosuch"`) {
		t.Errorf("留下的那条该是未声明指名，而不是思考降级：%q", got[0])
	}
}

// ---- 零工具 ----

// 零工具 + 要求有工具的 tool_choice：报出删改，并说清是哪个档位。
func TestDiagnoseReportsToolChoiceWithoutTools(t *testing.T) {
	for _, mode := range []ir.ChoiceMode{ir.ChoiceAny, ir.ChoiceTool} {
		req := r100Req(&ir.ToolChoice{Mode: mode, ToolName: "alpha"})
		for _, outbound := range []string{"anthropic", "codex", "openai-chat", "openai-responses"} {
			got := r100Notes(t, req, outbound)
			if !strings.Contains(got, "declares no tools") {
				t.Errorf("%s 档在 %s 上没报出零工具冲突：%q", mode, outbound, got)
			}
		}
	}
}

// auto 与 none 在零工具下是接收侧接受的形状，且与不带 tool_choice 同义：不报。
// 这条钉住判据只认 any/tool 两档。
func TestDiagnoseSilentOnZeroToolsWithPassiveChoice(t *testing.T) {
	for _, mode := range []ir.ChoiceMode{ir.ChoiceAuto, ir.ChoiceNone} {
		req := r100Req(&ir.ToolChoice{Mode: mode})
		for _, outbound := range []string{"anthropic", "codex", "openai-chat", "openai-responses"} {
			if got := r100Notes(t, req, outbound); got != "" {
				t.Errorf("%s 档零工具不该有注记，%s 报了：%q", mode, outbound, got)
			}
		}
	}
}

// 零工具那一态盖住白名单与指名两条：tool_choice 已被整条删掉，再报「白名单落空」
// 或「指名回落」就是把同一次删改说三遍。
func TestDiagnoseZeroToolsSupersedesOtherToolChoiceNotes(t *testing.T) {
	req := r100Req(&ir.ToolChoice{Mode: ir.ChoiceAny, AllowedTools: []string{"nosuch"}})
	req.Thinking = &ir.ThinkingConfig{Enabled: true, Effort: "high"}
	got := Diagnose(req, "openai-chat", capsOf(t, "openai-chat"))
	if len(got) != 1 || !strings.Contains(got[0], "declares no tools") {
		t.Fatalf("零工具应只出一条注记，得到 %v", got)
	}
}

// 能力矩阵绊线：这三条注记都不看能力位（四个出站同口径），所以 capsOf 取到的真实
// 声明里任何一位翻转都不该改变结论。手写一份全假的 Capabilities 会让测试脱离真实
// 声明，故按真实声明逐家跑一遍。
func TestToolChoiceNotesAreCapabilityIndependent(t *testing.T) {
	req := r100Req(&ir.ToolChoice{Mode: ir.ChoiceAny, AllowedTools: []string{"nosuch"}}, "alpha")
	var first string
	for _, outbound := range []string{"anthropic", "codex", "openai-chat", "openai-responses"} {
		c, err := proto.GetOutbound(outbound)
		if err != nil {
			t.Fatalf("GetOutbound(%q): %v", outbound, err)
		}
		got := strings.Join(Diagnose(req, outbound, c.Caps()), "; ")
		if first == "" {
			first = got
		} else if got != first {
			t.Errorf("%s 的结论与第一家不同，说明掺进了能力位判断：\n%q\n%q", outbound, first, got)
		}
	}
}
