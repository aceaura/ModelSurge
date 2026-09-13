// stream.go NewStreamEncoder 的占位实现。
// 解码方向（事件流/非流式）已由 decode.go 与 decode_response.go 实现；
// kiro 仅作上游协议（客户端不会以 kiro 协议接入网关），流式编码按接口
// 要求保留为未实现错误——真实链路中不会被调用。
package kiro

import (
	"fmt"

	"github.com/aceaura/ModelSurge/agent/ir"
	"github.com/aceaura/ModelSurge/agent/proto"
)

var errStreamNotImplemented = fmt.Errorf("kiro codec: stream encoding not implemented (kiro is upstream-only)")

func (Codec) NewStreamEncoder() proto.StreamEncoder { return notImplEncoder{} }

type notImplEncoder struct{}

func (notImplEncoder) Encode(_ ir.Event) ([][]byte, error) { return nil, errStreamNotImplemented }
func (notImplEncoder) Finish() [][]byte                    { return nil }
