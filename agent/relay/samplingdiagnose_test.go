package relay

import (
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// 调参维度装不下时必须报出来，装得下时不得误报。少报会让客户端以为参数生效了
// （温度调了没反应、seed 给了结果还在变），误报则让读者去改一个本来没问题的调用。
func TestDiagnoseSamplingPerProtocol(t *testing.T) {
	pp, seed, n, top := 0.25, 4242, 3, 7
	yes := true
	cases := []struct {
		dim    string
		want   string
		mutate func(*ir.Request)
		// warns 每个上游协议是否应报这一维
		warns map[string]bool
	}{
		{"presence_penalty", "no penalty parameter",
			func(r *ir.Request) { r.PresencePenalty = &pp },
			map[string]bool{"anthropic": true, "kiro": true, "openai-responses": true, "openai-chat": false}},
		{"seed", "no seed parameter",
			func(r *ir.Request) { r.Seed = &seed },
			map[string]bool{"anthropic": true, "kiro": true, "openai-responses": true, "openai-chat": false}},
		{"n", "no multi-candidate parameter",
			func(r *ir.Request) { r.Candidates = &n },
			map[string]bool{"anthropic": true, "kiro": true, "openai-responses": true, "openai-chat": false}},
		{"logprobs", "no log probability parameter",
			func(r *ir.Request) { r.LogProbs, r.TopLogProbs = &yes, &top },
			map[string]bool{"anthropic": true, "kiro": true, "openai-responses": false, "openai-chat": false}},
		{"logit_bias", "no logit bias parameter",
			func(r *ir.Request) { r.LogitBias = map[string]float64{"1234": -50} },
			map[string]bool{"anthropic": true, "kiro": true, "openai-responses": true, "openai-chat": false}},
	}
	for _, c := range cases {
		for name, shouldWarn := range c.warns {
			t.Run(c.dim+"/"+name, func(t *testing.T) {
				req := &ir.Request{Model: "m", MaxTokens: 64,
					Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}}}
				c.mutate(req)
				notes := strings.Join(Diagnose(req, name, capsOf(t, name)), "\n")
				if got := strings.Contains(notes, c.want); got != shouldWarn {
					t.Errorf("报出=%v, want %v；notes=%q", got, shouldWarn, notes)
				}
			})
		}
	}
}

// 客户端没给调参值时一条都不能报：噪音会把真正的有损点埋掉。
func TestDiagnoseSamplingSilentWhenAbsent(t *testing.T) {
	req := &ir.Request{Model: "m", MaxTokens: 64,
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}}}
	for _, name := range []string{"anthropic", "openai-chat", "openai-responses", "kiro"} {
		for _, note := range Diagnose(req, name, capsOf(t, name)) {
			for _, dim := range []string{"penalty", "seed", "candidate", "log probability", "logit bias"} {
				if strings.Contains(note, dim) {
					t.Errorf("%s 没给参数却报了 %q", name, note)
				}
			}
		}
	}
}
