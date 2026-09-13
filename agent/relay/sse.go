// Package relay 负责上游转发：强制上游流式、SSE 读取、
// 非流式客户端的缓冲聚合、非 SSE 上游响应的兜底解析。
package relay

import (
	"bufio"
	"bytes"
	"io"
)

// SSEEvent 一条 SSE 事件：event 名（可为空）与 data 载荷（多行 data 已拼接）。
type SSEEvent struct {
	Event string
	Data  string
}

// EventReader 逐条读取 SSE 事件的迭代器。遵循 SSE 规范：data: 行累积，
// 空行分发，event: 行命名，冒号开头的行是注释（跳过）。
type EventReader struct {
	br      *bufio.Reader
	event   string
	data    string
	hasData bool
}

// NewEventReader 构造事件迭代器。
func NewEventReader(r io.Reader) *EventReader {
	return &EventReader{br: bufio.NewReaderSize(r, 64*1024)}
}

// Next 读下一条事件。ok=false 且 err==nil 表示流正常结束（EOF）。
func (er *EventReader) Next() (ev SSEEvent, ok bool, err error) {
	for {
		line, rerr := er.br.ReadBytes('\n')
		if len(line) > 0 {
			line = bytes.TrimRight(line, "\r\n")
			switch {
			case len(line) == 0:
				if er.hasData {
					ev = SSEEvent{Event: er.event, Data: er.data}
					er.event, er.data, er.hasData = "", "", false
					return ev, true, nil
				}
				er.event = ""
			case line[0] == ':':
				// 注释/保活行
			case bytes.HasPrefix(line, []byte("event:")):
				er.event = string(bytes.TrimSpace(line[6:]))
			case bytes.HasPrefix(line, []byte("data:")):
				if er.hasData {
					er.data += "\n"
				}
				er.data += string(bytes.TrimSpace(line[5:]))
				er.hasData = true
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				if er.hasData {
					ev = SSEEvent{Event: er.event, Data: er.data}
					er.event, er.data, er.hasData = "", "", false
					return ev, true, nil
				}
				return SSEEvent{}, false, nil
			}
			return SSEEvent{}, false, rerr
		}
	}
}

// ReadSSE 从 r 中逐条读出 SSE 事件，直到 io.EOF 或出错。
// 回调返回 false 可提前终止。
func ReadSSE(r io.Reader, fn func(SSEEvent) bool) error {
	er := NewEventReader(r)
	for {
		ev, ok, err := er.Next()
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		if !fn(ev) {
			return nil
		}
	}
}
