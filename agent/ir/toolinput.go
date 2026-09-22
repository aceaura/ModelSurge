package ir

import (
	"encoding/json"
	"fmt"
)

// RawArgsKey 非法工具参数的原文落位键。工具调用的参数槽必须是 JSON 对象，
// 但模型上一轮可能吐出截断/非对象 JSON（max_tokens 截断是最常见来源）。
// 客户端把历史原样带回来时，静默替换成 {} 等于让工具不带参数执行——
// 比 400 更糟，因为那是一次真实副作用。规整把原文挪进显式键位：
// 工具按 schema 校验会自然报缺参，客户端也能从键名看出发生了什么。
const RawArgsKey = "_modelsurge_raw_args"

// NormalizeToolInput 把工具参数规整为 JSON 对象。
// ok=false 表示原文不是合法 JSON 对象，已被挪进 RawArgsKey 键位；
// 空输入视为「无参调用」，返回 {} 且 ok=true。
// 合法对象原样透传（不做压缩，保持字节级保真）。
func NormalizeToolInput(raw json.RawMessage) (json.RawMessage, bool) {
	if len(raw) == 0 {
		return json.RawMessage(`{}`), true
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		// 非法 JSON（多为截断）：原文按字符串保下来。
		out, _ := json.Marshal(map[string]any{RawArgsKey: string(raw)})
		return out, false
	}
	if _, isObj := v.(map[string]any); !isObj {
		// 合法但不是对象（数组/字符串/数字/null）：协议槽位要求是对象，
		// 原值按 JSON 嵌入，类型信息不丢。
		out, _ := json.Marshal(map[string]json.RawMessage{RawArgsKey: raw})
		return out, false
	}
	return raw, true
}

// RewrapNote 参数挪键损耗的诊断措辞。挪键发生在三处（聚合器冲刷、gemini
// 流式编码、对象槽位响应扫描），共用同一句，客户端无论从哪条路收到都一样。
func RewrapNote(n int) string {
	return fmt.Sprintf(
		"rewrapped %d malformed tool call argument(s) into %s: the client will not receive them as parameters",
		n, RawArgsKey)
}
