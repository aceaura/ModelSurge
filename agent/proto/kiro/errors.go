// errors.go kiro codec 的错误渲染。
// kiro 只作为上游协议接入（客户端入口永远是 anthropic/openai/gemini），
// 以下方法仅在把 kiro 当客户端协议直连时才会触达，渲染为通用 JSON 外形。
package kiro

import (
	"encoding/json"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// marshalJSON 序列化 JSON，失败时给最简兜底（与其他 codec 包的 marshal 惯例一致）。
func marshalJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte(`{"error":{"type":"api_error","message":"render error failed"}}`)
	}
	return b
}

type kiroErrorBody struct {
	Type    string `json:"type"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

type kiroErrorResponse struct {
	Error kiroErrorBody `json:"error"`
}

// RenderError 按通用 JSON 外形渲染错误。
func (Codec) RenderError(e *ir.Error) (int, []byte) {
	body := kiroErrorResponse{Error: kiroErrorBody{Type: e.Type, Code: e.Code, Message: e.Message}}
	return e.HTTPStatus(), marshalJSON(body)
}

// RenderStreamError 渲染流内错误事件（SSE 字节）。
func (Codec) RenderStreamError(e *ir.Error) []byte {
	body := kiroErrorResponse{Error: kiroErrorBody{Type: e.Type, Code: e.Code, Message: e.Message}}
	return []byte("data: " + string(marshalJSON(body)) + "\n\ndata: [DONE]\n\n")
}
