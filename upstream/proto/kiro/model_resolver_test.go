package kiro

import "testing"

// normalize 各形态（样例取自 KiroaaS model_resolver.py docstring）。
func TestNormalizeModelName(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"claude-haiku-4-5-20251001", "claude-haiku-4.5"},
		{"claude-haiku-4-5-latest", "claude-haiku-4.5"},
		{"claude-sonnet-4-5", "claude-sonnet-4.5"},
		{"claude-opus-4-5", "claude-opus-4.5"},
		{"claude-sonnet-4", "claude-sonnet-4"},
		{"claude-sonnet-4-20250514", "claude-sonnet-4"},
		{"claude-3-7-sonnet", "claude-3.7-sonnet"},
		{"claude-3-7-sonnet-20250219", "claude-3.7-sonnet"},
		{"claude-4.5-opus-high", "claude-opus-4.5"},
		{"claude-4.5-sonnet-low", "claude-sonnet-4.5"},
		{"claude-haiku-4.5-20251001", "claude-haiku-4.5"},
		{"auto", "auto"},
		{"CLAUDE-SONNET-4-5", "claude-sonnet-4.5"}, // 大写归一
		{"claude-sonnet-4-5[1m]", "claude-sonnet-4.5"},
		{"claude-sonnet-4-5[200K]", "claude-sonnet-4.5"},
		{"glm-5", "glm-5"},                         // 非 Claude 原样
		{"CLAUDE-3-7-SONNET", "claude-3.7-sonnet"}, // 已带 8 位日期不误匹配
	} {
		if got := NormalizeModelName(tc.in); got != tc.want {
			t.Errorf("normalize(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// fakeCache 供管线测试。
type fakeCache struct{ models map[string]bool }

func (f *fakeCache) IsValid(id string) bool { return f.models[id] }
func (f *fakeCache) AllModelIDs() []string  { return []string{"claude-sonnet-4.5", "glm-5"} }

// 四层解析管线：别名 -> 规范化 -> 缓存 -> 隐藏 -> 直通。
func TestResolvePipeline(t *testing.T) {
	cache := &fakeCache{models: map[string]bool{"claude-sonnet-4.5": true, "glm-5": true, "auto": true}}
	r := NewModelResolver(cache,
		map[string]string{"claude-3.7-sonnet": "auto"}, // 隐藏模型：旧模型映射到 auto
		map[string]string{"auto-kiro": "auto"},
		[]string{"auto"},
	)

	// 层 0+1+2：别名 -> 规范化 -> 缓存命中
	res := r.Resolve("auto-kiro")
	if res.InternalID != "auto" || res.Source != "cache" || !res.IsVerified {
		t.Errorf("auto-kiro = %+v", res)
	}
	// 横线形态规范化后命中缓存
	res = r.Resolve("claude-sonnet-4-5")
	if res.InternalID != "claude-sonnet-4.5" || res.Source != "cache" || !res.IsVerified {
		t.Errorf("claude-sonnet-4-5 = %+v", res)
	}
	// 层 3：隐藏模型
	res = r.Resolve("claude-3-7-sonnet")
	if res.InternalID != "auto" || res.Source != "hidden" || !res.IsVerified {
		t.Errorf("claude-3-7-sonnet = %+v", res)
	}
	// 层 4：直通（未知模型交给 Kiro 裁决）
	res = r.Resolve("gpt-9-turbo")
	if res.InternalID != "gpt-9-turbo" || res.Source != "passthrough" || res.IsVerified {
		t.Errorf("gpt-9-turbo = %+v", res)
	}

	// /v1/models：缓存 - hidden_from_list + 别名
	models := r.AvailableModels()
	want := []string{"auto-kiro", "claude-3.7-sonnet", "claude-sonnet-4.5", "glm-5"}
	if len(models) != len(want) {
		t.Fatalf("models = %v, want %v", models, want)
	}
	for i := range want {
		if models[i] != want[i] {
			t.Errorf("models[%d] = %s, want %s", i, models[i], want[i])
		}
	}
}

// 点/横线展示转换：Claude 系横线，其他原样。
func TestDashifyClaudeID(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"claude-sonnet-4.6", "claude-sonnet-4-6"},
		{"claude-3.7-sonnet", "claude-3-7-sonnet"},
		{"claude-opus-4", "claude-opus-4"},
		{"auto", "auto"},
		{"glm-5.3", "glm-5.3"},
		{"CLAUDE-SONNET-4.5", "CLAUDE-SONNET-4-5"},
	} {
		if got := DashifyClaudeID(tc.in); got != tc.want {
			t.Errorf("dashify(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
