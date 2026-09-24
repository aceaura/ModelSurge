package anthropic

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// R104：托管工具定义上的修饰字段（cache_control/strict/defer_loading/
// eager_input_streaming/allowed_callers）此前只挂在函数工具分支，托管分支
// 解码即丢。同族回写一个键也不能少。
func TestEncodeRequestHostedToolKeepsModifiers(t *testing.T) {
	strict := true
	eager := false
	out, err := codec{}.EncodeRequest(&ir.Request{
		Model: "claude", MaxTokens: 64,
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}},
		Tools: []ir.Tool{{
			Hosted: "web_search", HostedType: "web_search_20250305", Name: "web_search",
			CacheCtl: "ephemeral", CacheTTL: "1h",
			Strict: &strict, DeferLoading: true, EagerInputStreaming: &eager,
			AllowedCallers: []string{"direct"},
			HostedParams:   &ir.HostedParams{MaxUses: 3},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Tools []struct {
			Type                string          `json:"type"`
			CacheControl        json.RawMessage `json:"cache_control"`
			Strict              *bool           `json:"strict"`
			DeferLoading        bool            `json:"defer_loading"`
			EagerInputStreaming *bool           `json:"eager_input_streaming"`
			AllowedCallers      []string        `json:"allowed_callers"`
			MaxUses             int             `json:"max_uses"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(out, &wire); err != nil {
		t.Fatal(err)
	}
	if len(wire.Tools) != 1 {
		t.Fatalf("托管工具没编出来：%s", out)
	}
	tool := wire.Tools[0]
	if tool.Type != "web_search_20250305" {
		t.Errorf("同族原生类型名没回写：%q", tool.Type)
	}
	if !strings.Contains(string(tool.CacheControl), `"ttl":"1h"`) {
		t.Errorf("cache_control 丢了：%s", tool.CacheControl)
	}
	if tool.Strict == nil || !*tool.Strict {
		t.Errorf("strict 丢了：%+v", tool.Strict)
	}
	if !tool.DeferLoading {
		t.Error("defer_loading 丢了")
	}
	if tool.EagerInputStreaming == nil || *tool.EagerInputStreaming {
		t.Errorf("eager_input_streaming 显式 false 丢了：%+v", tool.EagerInputStreaming)
	}
	if len(tool.AllowedCallers) != 1 || tool.AllowedCallers[0] != "direct" {
		t.Errorf("allowed_callers 丢了：%v", tool.AllowedCallers)
	}
	if tool.MaxUses != 3 {
		t.Errorf("托管参数丢了：%d", tool.MaxUses)
	}
}

// R104：未识别种类的跨族托管工具（如 responses 的 file_search）没有同族原生名
// 可用时，canonical 原名写进 type 是必 400 的形状，整块丢弃比拒整轮诚实
// （Diagnose 已按 no cross-protocol mapping 报出）。
func TestEncodeRequestDropsUnmappedCrossFamilyHosted(t *testing.T) {
	out, err := codec{}.EncodeRequest(&ir.Request{
		Model: "claude", MaxTokens: 64,
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}},
		Tools: []ir.Tool{
			{Hosted: "file_search", HostedType: "file_search", Name: "file_search"},
			{Hosted: "web_search", HostedType: "web_search_20250305", Name: "web_search"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Tools []struct {
			Type string `json:"type"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(out, &wire); err != nil {
		t.Fatal(err)
	}
	if len(wire.Tools) != 1 || wire.Tools[0].Type != "web_search_20250305" {
		t.Fatalf("跨族托管工具没被丢弃/同族没保留：%s", out)
	}
}

// R105（乙2）：未建模的托管工具声明参数——computer 的 display_width_px/
// display_height_px（官方 Required）、web_fetch 的 citations/max_content_tokens——
// 解码即丢会让同族往返编出缺 Required 键的非法定义。同族回写整块回吐原文。
func TestHostedToolRawRoundTripKeepsUnmodeledParams(t *testing.T) {
	body := []byte(`{"model":"claude","max_tokens":64,"messages":[{"role":"user","content":"hi"}],` +
		`"tools":[` +
		`{"type":"computer_20250124","name":"computer","display_width_px":1024,"display_height_px":768},` +
		`{"type":"web_fetch_20250910","name":"web_fetch","max_uses":2,"citations":{"enabled":true},"max_content_tokens":4096}` +
		`]}`)
	req, err := codec{}.DecodeRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Tools) != 2 {
		t.Fatalf("托管工具没解出来：%+v", req.Tools)
	}
	// 走一遍 Clone（R98 判据：JSON 往返后原文槽位不得变字面量 null 也不得丢）。
	out, err := codec{}.EncodeRequest(req.Clone())
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{
		`"display_width_px":1024`, `"display_height_px":768`,
		`"citations":{"enabled":true}`, `"max_content_tokens":4096`, `"max_uses":2`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("未建模声明参数丢了 %s：%s", want, s)
		}
	}
	if strings.Contains(s, `"Raw":`) || strings.Contains(s, `null`) {
		t.Errorf("原文槽位泄漏或伪造 null：%s", s)
	}
}

// 原文回吐不得盖住同族的版本指名：原文里的 type 是客户端指名的版本，
// 硬编码默认版本把它偷换掉就是另一种丢。
func TestHostedToolRawKeepsClientPinnedVersion(t *testing.T) {
	body := []byte(`{"model":"claude","max_tokens":64,"messages":[{"role":"user","content":"hi"}],` +
		`"tools":[{"type":"web_search_20260201","name":"web_search","max_uses":1}]}`)
	req, err := codec{}.DecodeRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	out, err := codec{}.EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"type":"web_search_20260201"`) {
		t.Errorf("客户端指名版本被默认版本偷换：%s", out)
	}
}

// 跨族出站忽略 HostedRaw：外族原生名没有可信槽位，原文块不得泄漏。
// （responses 出站只认 HostedType 同族名；anthropic 原文到不了那边。）
func TestHostedToolRawIgnoredCrossFamily(t *testing.T) {
	req := &ir.Request{
		Model: "claude", MaxTokens: 64,
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}},
		Tools: []ir.Tool{{
			Hosted: "web_search", HostedType: "web_search_preview", Name: "web_search",
			HostedRaw: json.RawMessage(`{"type":"web_search_20250305","name":"web_search","max_uses":9}`),
		}},
	}
	out, err := codec{}.EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	// 外族 HostedType（responses 的 web_search_preview）回落默认带版本名；
	// HostedRaw 是同族通道，HostedType 不是同族时不得回吐原文
	// （泄漏标记是原文独有的 "max_uses":9——fallback 类型名恰与原文相同不算泄漏）。
	if strings.Contains(s, `"max_uses":9`) {
		t.Errorf("HostedRaw 在非同族来路上泄漏：%s", s)
	}
	if !strings.Contains(s, `"type":"web_search_20250305"`) {
		t.Errorf("外族来路没回落默认带版本名：%s", s)
	}
}
