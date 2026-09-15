// stream_decoder_test.go 上游 OpenAI 流解码的 tool 边界形态回归。
package openaichat

import (
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// 首帧同含 id+name+arguments（GLM/智谱形态）时该帧参数不得丢失。
func TestDecodeStreamToolCallSameFrameArgs(t *testing.T) {
	dec := codec{}.NewStreamDecoder()
	evs, err := dec.Feed("data", `{"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":"}}]}}]}`)
	if err != nil {
		t.Fatal(err)
	}
	evs2, err := dec.Feed("data", `{"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"北京\"}"}}]}}]}`)
	if err != nil {
		t.Fatal(err)
	}
	var args string
	for _, ev := range append(evs, evs2...) {
		if ev.Type == ir.EvToolInput {
			args += ev.Text
		}
	}
	if args != `{"city":"北京"}` {
		t.Fatalf("tool arguments = %q, want %q", args, `{"city":"北京"}`)
	}
}

// 参数先于 id/name 到达（先缓存后冲刷）的既有形态不回归。
func TestDecodeStreamToolCallPendingArgs(t *testing.T) {
	dec := codec{}.NewStreamDecoder()
	evs, err := dec.Feed("data", `{"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":"}}]}}]}`)
	if err != nil {
		t.Fatal(err)
	}
	evs2, err := dec.Feed("data", `{"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"get_weather","arguments":"\"北京\"}"}}]}}]}`)
	if err != nil {
		t.Fatal(err)
	}
	var args string
	for _, ev := range append(evs, evs2...) {
		if ev.Type == ir.EvToolInput {
			args += ev.Text
		}
	}
	if args != `{"city":"北京"}` {
		t.Fatalf("tool arguments = %q, want %q", args, `{"city":"北京"}`)
	}
}
