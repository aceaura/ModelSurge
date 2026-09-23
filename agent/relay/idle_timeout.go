// idle_timeout.go 流式响应的 chunk 间读超时看门狗。
// 模型思考/工具调用间隙可能长时间无数据，看门狗在 timeout 内
// 无新 chunk 时强制关闭底层连接（阻塞中的读取随即返回错误），
// 避免僵死流。
package relay

import (
	"io"
	"sync"
	"time"
)

// idleTimeoutBody 包装响应 body：每次成功读取重置计时器，
// 计时器到期则关闭底层 body。
type idleTimeoutBody struct {
	rc      io.ReadCloser
	timeout time.Duration

	mu     sync.Mutex
	timer  *time.Timer
	closed bool
}

// newIdleTimeoutBody 构造看门狗 body；timeout 须为正值。
func newIdleTimeoutBody(rc io.ReadCloser, timeout time.Duration) *idleTimeoutBody {
	b := &idleTimeoutBody{rc: rc, timeout: timeout}
	b.timer = time.AfterFunc(timeout, func() { _ = rc.Close() })
	return b
}

// bump 读取成功后重置看门狗。
func (b *idleTimeoutBody) bump() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.closed {
		b.timer.Reset(b.timeout)
	}
}

func (b *idleTimeoutBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	if err == nil {
		b.bump()
	}
	return n, err
}

// Close 停表并关闭底层 body（幂等语义随底层实现）。
func (b *idleTimeoutBody) Close() error {
	b.mu.Lock()
	b.closed = true
	if b.timer != nil {
		b.timer.Stop()
	}
	b.mu.Unlock()
	return b.rc.Close()
}

var _ io.ReadCloser = (*idleTimeoutBody)(nil)
