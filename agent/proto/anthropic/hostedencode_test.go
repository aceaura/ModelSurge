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
