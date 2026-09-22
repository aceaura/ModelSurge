package ir

import (
	"encoding/json"
	"strings"
	"testing"
)

// NormalizeToolInput 的四档语义：空=无参调用（ok），合法对象=字节级透传（ok），
// 非法 JSON=原文按字符串挪键（!ok），合法非对象=原值嵌入挪键（!ok）。
// 任何一档都不允许凭空清空——空 {} 会让工具不带参数执行。
func TestNormalizeToolInput(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantOK  bool
		wantRaw string // RawArgsKey 里的原文（仅 !ok 时断言）
	}{
		{"empty", ``, true, ""},
		{"object", `{"a":1}`, true, ""},
		{"object-whitespace", "{\n  \"a\": 1\n}", true, ""}, // 字节级透传，不压缩
		{"truncated", `{"city": "Par`, false, `{"city": "Par`},
		{"array", `[1,2]`, false, `[1,2]`},
		{"string", `"juststring"`, false, `"juststring"`},
		{"number", `123`, false, `123`},
		{"null", `null`, false, `null`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, ok := NormalizeToolInput(json.RawMessage(c.in))
			if ok != c.wantOK {
				t.Errorf("ok = %v, want %v (out=%s)", ok, c.wantOK, out)
			}
			if c.in == "" {
				if string(out) != `{}` {
					t.Errorf("空输入应规整为 {}，得到 %s", out)
				}
				return
			}
			if c.wantOK {
				if string(out) != c.in {
					t.Errorf("合法对象被改写字节: %s -> %s", c.in, out)
				}
				return
			}
			var m map[string]json.RawMessage
			if err := json.Unmarshal(out, &m); err != nil {
				t.Fatalf("规整结果不是合法 JSON: %s", out)
			}
			if len(m) != 1 {
				t.Errorf("挪键后混入别的键: %s", out)
			}
			raw, has := m[RawArgsKey]
			if !has {
				t.Fatalf("缺 %s 键: %s", RawArgsKey, out)
			}
			// 非法 JSON 按字符串挪键（值是带引号的 JSON 串），合法非对象按原值嵌入。
			if c.name == "truncated" {
				var s string
				if err := json.Unmarshal(raw, &s); err != nil || s != c.wantRaw {
					t.Errorf("原文 = %s, want 字符串 %q", raw, c.wantRaw)
				}
			} else if string(raw) != c.wantRaw {
				t.Errorf("原文 = %s, want %s", raw, c.wantRaw)
			}
		})
	}
}

// 键名本身要一眼可读：客户端从键名能推断发生了什么，不需要翻文档。
func TestRawArgsKeyIsSelfExplanatory(t *testing.T) {
	if !strings.Contains(RawArgsKey, "raw_args") {
		t.Errorf("键名 %q 不含 raw_args，不可自解释", RawArgsKey)
	}
}
