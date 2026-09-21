package ir

import (
	"strings"
	"testing"
)

// EffortFromBudget 四个分档 + 负值动态思考 + 零值，边界值逐个钉。
// 阈值是与 new-api 对齐的具体数字，改动会静默改变跨协议行为，所以断言
// 落在边界两侧（1024/1025、8192/8193）而不只是区间中点。
func TestEffortFromBudget(t *testing.T) {
	cases := []struct {
		budget int
		want   string
	}{
		{0, EffortNone},
		{-1, EffortHigh},     // Gemini 动态思考：尽量想
		{-32768, EffortHigh}, // 任意负值同解
		{1, EffortLow},
		{1024, EffortLow}, // 边界含在 low
		{1025, EffortMedium},
		{4096, EffortMedium},
		{8192, EffortMedium}, // 边界含在 medium
		{8193, EffortHigh},
		{32768, EffortHigh},
	}
	for _, c := range cases {
		if got := EffortFromBudget(c.budget); got != c.want {
			t.Errorf("EffortFromBudget(%d) = %q, want %q", c.budget, got, c.want)
		}
	}
}

// BudgetFromEffort 五个已知档位各按百分比算，且 xhigh 与 max 同档。
func TestBudgetFromEffortPercentages(t *testing.T) {
	const maxTokens = 100000 // 取整百分比不产生舍入，便于逐档钉死数值
	cases := []struct {
		effort string
		want   int
	}{
		{EffortMinimal, 5000},
		{EffortLow, 20000},
		{EffortMedium, 50000},
		{EffortHigh, 80000},
		{EffortXHigh, 95000},
		{EffortMax, 95000}, // 与 xhigh 同档（最深思考没有更高档）
	}
	for _, c := range cases {
		if got := BudgetFromEffort(c.effort, maxTokens); got != c.want {
			t.Errorf("BudgetFromEffort(%q, %d) = %d, want %d", c.effort, maxTokens, got, c.want)
		}
	}
}

// 未知档位返回 0（调用方据此不动预算），而不是猜一个值。
// 照 cc-switch 的理由：猜值可能误吞采样参数。
func TestBudgetFromEffortRejectsUnknown(t *testing.T) {
	for _, effort := range []string{"", "none", "ultra", "HIGH", "medium ", "5"} {
		if got := BudgetFromEffort(effort, 100000); got != 0 {
			t.Errorf("BudgetFromEffort(%q) = %d, want 0 (unknown effort must not be guessed)", effort, got)
		}
	}
}

// 无 max_tokens 时回落固定缺省而非报错（本项目判据：只报有损，从不拒请求）。
func TestBudgetFromEffortWithoutMaxTokens(t *testing.T) {
	for _, maxTokens := range []int{0, -1} {
		got := BudgetFromEffort(EffortHigh, maxTokens)
		if got != DefaultThinkingBudget {
			t.Errorf("BudgetFromEffort(high, %d) = %d, want %d", maxTokens, got, DefaultThinkingBudget)
		}
	}
	// 未知档位在无 max_tokens 时仍必须返回 0——缺 max_tokens 不该让未知档位
	// 变成「可以猜」。这两条判断的先后顺序由本用例钉住。
	if got := BudgetFromEffort("ultra", 0); got != 0 {
		t.Errorf("BudgetFromEffort(ultra, 0) = %d, want 0", got)
	}
}

// 小 max_tokens 上按百分比会算出低于 Anthropic 协议下限 1024 的值，须抬到下限；
// 而抬升后又可能 >= max_tokens，须再夹到 max_tokens-1。两步的先后顺序在这里钉住。
func TestBudgetFromEffortClampsToProtocolBounds(t *testing.T) {
	cases := []struct {
		name      string
		effort    string
		maxTokens int
		want      int
	}{
		// 200 * 5% = 10，抬到 1024，但 1024 >= 200 故夹到 199
		{"tiny max_tokens clamps below limit", EffortMinimal, 200, 199},
		// 4096 * 5% = 204，抬到 1024，1024 < 4096 故保留
		{"raised to minimum", EffortMinimal, 4096, MinThinkingBudget},
		// 2000 * 95% = 1900，不低于下限且小于上限，原样
		{"within bounds untouched", EffortXHigh, 2000, 1900},
		// 1024 * 95% = 972，抬到 1024，1024 >= 1024 故夹到 1023
		{"exactly at limit clamps down", EffortXHigh, 1024, 1023},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := BudgetFromEffort(c.effort, c.maxTokens)
			if got != c.want {
				t.Fatalf("BudgetFromEffort(%q, %d) = %d, want %d", c.effort, c.maxTokens, got, c.want)
			}
			// 不变式：算出来的预算永远严格小于 max_tokens（Anthropic 硬约束）
			if got >= c.maxTokens {
				t.Fatalf("budget %d must stay below max_tokens %d", got, c.maxTokens)
			}
		})
	}
}

// CompleteThinking 的输入空间穷举：Thinking 为 nil / 未启用 / 只给 effort /
// 只给 budget / 两者都给 / 两者都空。每一格都要有断言——只给一侧的两格是
// 本轮修的缺口，其余四格是必须保持不动的既有行为。
func TestCompleteThinkingGrid(t *testing.T) {
	cases := []struct {
		name       string
		thinking   *ThinkingConfig
		maxTokens  int
		wantEffort string
		wantBudget int
		wantNotes  int
	}{
		{
			name:     "nil thinking untouched",
			thinking: nil,
		},
		{
			// 明确关闭：不补全。补了会让「关掉推理」的请求带上推理配置。
			name:       "disabled untouched",
			thinking:   &ThinkingConfig{Enabled: false, Effort: EffortHigh},
			maxTokens:  100000,
			wantEffort: EffortHigh,
			wantBudget: 0,
		},
		{
			name:       "effort only derives budget",
			thinking:   &ThinkingConfig{Enabled: true, Effort: EffortHigh},
			maxTokens:  100000,
			wantEffort: EffortHigh,
			wantBudget: 80000,
			wantNotes:  1,
		},
		{
			name:       "budget only derives effort",
			thinking:   &ThinkingConfig{Enabled: true, BudgetTokens: 32768},
			maxTokens:  100000,
			wantEffort: EffortHigh,
			wantBudget: 32768,
			wantNotes:  1,
		},
		{
			// 两侧都给：客户端同时表达了两种风格，改写任一侧都是发明意图。
			name:       "both stated untouched",
			thinking:   &ThinkingConfig{Enabled: true, Effort: EffortLow, BudgetTokens: 60000},
			maxTokens:  100000,
			wantEffort: EffortLow,
			wantBudget: 60000,
		},
		{
			// 两侧都空：没有可派生的来源，出站各自的缺省仍然生效。
			name:      "neither stated untouched",
			thinking:  &ThinkingConfig{Enabled: true},
			maxTokens: 100000,
		},
		{
			// 未知档位无法换算，预算保持为零而不是被猜出来的值。
			name:       "unknown effort leaves budget zero",
			thinking:   &ThinkingConfig{Enabled: true, Effort: "ultra"},
			maxTokens:  100000,
			wantEffort: "ultra",
			wantBudget: 0,
		},
		{
			// 负预算是动态思考，派生出最高档且预算原样保留（不可改写成正数，
			// 那会把「上游自行决定」变成一个具体上限）。
			name:       "dynamic budget derives high effort",
			thinking:   &ThinkingConfig{Enabled: true, BudgetTokens: -1},
			maxTokens:  100000,
			wantEffort: EffortHigh,
			wantBudget: -1,
			wantNotes:  1,
		},
		{
			// effort 派生预算时无 max_tokens：落到固定缺省。
			name:       "effort only without max_tokens",
			thinking:   &ThinkingConfig{Enabled: true, Effort: EffortMedium},
			wantEffort: EffortMedium,
			wantBudget: DefaultThinkingBudget,
			wantNotes:  1,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := &Request{MaxTokens: c.maxTokens, Thinking: c.thinking}
			notes := CompleteThinking(r)
			if len(notes) != c.wantNotes {
				t.Fatalf("notes = %d (%v), want %d", len(notes), notes, c.wantNotes)
			}
			if c.thinking == nil {
				if r.Thinking != nil {
					t.Fatalf("nil thinking must stay nil")
				}
				return
			}
			if r.Thinking.Effort != c.wantEffort {
				t.Errorf("effort = %q, want %q", r.Thinking.Effort, c.wantEffort)
			}
			if r.Thinking.BudgetTokens != c.wantBudget {
				t.Errorf("budget = %d, want %d", r.Thinking.BudgetTokens, c.wantBudget)
			}
		})
	}
}

// 说明必须点出派生的方向与具体值：只说「补全了推理配置」的话，运维无法判断
// 发出去的是哪个数，而这一维直接决定上游计费。
func TestCompleteThinkingNotesNameBothSides(t *testing.T) {
	t.Run("effort to budget", func(t *testing.T) {
		r := &Request{MaxTokens: 100000, Thinking: &ThinkingConfig{Enabled: true, Effort: EffortHigh}}
		notes := CompleteThinking(r)
		if len(notes) != 1 {
			t.Fatalf("notes = %v", notes)
		}
		for _, want := range []string{"derived", "80000", "high"} {
			if !strings.Contains(notes[0], want) {
				t.Errorf("note %q must mention %q", notes[0], want)
			}
		}
	})
	t.Run("budget to effort", func(t *testing.T) {
		r := &Request{MaxTokens: 100000, Thinking: &ThinkingConfig{Enabled: true, BudgetTokens: 32768}}
		notes := CompleteThinking(r)
		if len(notes) != 1 {
			t.Fatalf("notes = %v", notes)
		}
		for _, want := range []string{"derived", "32768", "high"} {
			if !strings.Contains(notes[0], want) {
				t.Errorf("note %q must mention %q", notes[0], want)
			}
		}
	})
}

// nil 请求不 panic（调用点在四条上游路径上，将来可能在更早的位置调用）。
func TestCompleteThinkingNilRequest(t *testing.T) {
	if notes := CompleteThinking(nil); notes != nil {
		t.Fatalf("notes = %v, want nil", notes)
	}
}

// 换算是幂等的：补全后再调一次不产生新说明也不改值。
// 重试路径上每个目标都各自 Clone 后调用，但 autocompact 的 src 可能被复用。
func TestCompleteThinkingIsIdempotent(t *testing.T) {
	r := &Request{MaxTokens: 100000, Thinking: &ThinkingConfig{Enabled: true, Effort: EffortHigh}}
	if notes := CompleteThinking(r); len(notes) != 1 {
		t.Fatalf("first pass notes = %v, want 1", notes)
	}
	budget, effort := r.Thinking.BudgetTokens, r.Thinking.Effort
	if notes := CompleteThinking(r); len(notes) != 0 {
		t.Fatalf("second pass notes = %v, want none", notes)
	}
	if r.Thinking.BudgetTokens != budget || r.Thinking.Effort != effort {
		t.Fatalf("second pass changed values: %d/%q -> %d/%q", budget, effort, r.Thinking.BudgetTokens, r.Thinking.Effort)
	}
}
