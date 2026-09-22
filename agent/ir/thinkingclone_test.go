package ir

import (
	"encoding/json"
	"testing"
)

// Clone 走 JSON 往返：不带 omitempty 的 RawMessage 字段会在往返后从 nil 变成
// 非空 "null"。出站据此判断「客户端给过这一维」就会凭空写出一个对象——实测形态
// 是 anthropic 的 thinking:{type:"disabled"} 转 responses 出站多出
// "reasoning":{"context":null,"mode":null} 并顺带点名要 encrypted_content。
func TestCloneKeepsUnsetRawThinkingFieldsNil(t *testing.T) {
	c := (&Request{Thinking: &ThinkingConfig{Enabled: false}}).Clone()
	if c.Thinking == nil {
		t.Fatal("Clone 丢了 Thinking")
	}
	if len(c.Thinking.Context) != 0 {
		t.Errorf("Clone 后凭空长出 context=%q", c.Thinking.Context)
	}
	if len(c.Thinking.Mode) != 0 {
		t.Errorf("Clone 后凭空长出 mode=%q", c.Thinking.Mode)
	}
}

// omitempty 只该吃掉零值，不该吃掉真值：给过的原文必须逐字节活着。
func TestClonePreservesSetRawThinkingFields(t *testing.T) {
	c := (&Request{Thinking: &ThinkingConfig{
		Enabled: true, Effort: EffortHigh, Summary: "detailed",
		Context: json.RawMessage(`"all_turns"`), Mode: json.RawMessage(`"pro"`),
	}}).Clone()
	if string(c.Thinking.Context) != `"all_turns"` {
		t.Errorf("context = %q, want %q", c.Thinking.Context, `"all_turns"`)
	}
	if string(c.Thinking.Mode) != `"pro"` {
		t.Errorf("mode = %q, want %q", c.Thinking.Mode, `"pro"`)
	}
	if c.Thinking.Summary != "detailed" || c.Thinking.Effort != EffortHigh {
		t.Errorf("summary/effort = %q/%q，Clone 往返后失真", c.Thinking.Summary, c.Thinking.Effort)
	}
}
