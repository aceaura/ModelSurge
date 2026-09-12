// idle_timeout_test.go 流式看门狗与候选级首事件超时测试（任务组 13）。
package relay

import (
	"io"
	"testing"
	"time"

	"relayd/backend/account"
)

// TestIdleTimeoutBody_Fires 超时触发：底层 body 被关闭，阻塞中的读取返回错误。
func TestIdleTimeoutBody_Fires(t *testing.T) {
	pr, pw := io.Pipe()
	b := newIdleTimeoutBody(pr, 40*time.Millisecond)
	defer b.Close()

	errCh := make(chan error, 1)
	go func() {
		_, err := b.Read(make([]byte, 8))
		errCh <- err
	}()
	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("read should fail after watchdog fires")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("watchdog did not fire")
	}
	_ = pw.Close()
}

// TestIdleTimeoutBody_Bump 持续到达的 chunk 不断重置看门狗：不误杀。
func TestIdleTimeoutBody_Bump(t *testing.T) {
	pr, pw := io.Pipe()
	b := newIdleTimeoutBody(pr, 80*time.Millisecond)
	defer b.Close()

	go func() {
		defer pw.Close()
		for i := 0; i < 4; i++ {
			if _, err := pw.Write([]byte{byte('a' + i)}); err != nil {
				return
			}
			time.Sleep(30 * time.Millisecond) // chunk 间隔小于超时
		}
	}()
	buf := make([]byte, 4)
	if _, err := io.ReadFull(b, buf); err != nil {
		t.Fatalf("read should succeed with periodic chunks: %v", err)
	}
	if string(buf) != "abcd" {
		t.Fatalf("read = %q", buf)
	}
}

// TestIdleTimeoutBody_Close 停表：Close 后读取按正常 EOF/错误结束，
// 看门狗不再产生副作用。
func TestIdleTimeoutBody_Close(t *testing.T) {
	pr, pw := io.Pipe()
	b := newIdleTimeoutBody(pr, 20*time.Millisecond)
	pw.Close() // 底层先 EOF
	if _, err := b.Read(make([]byte, 8)); err != io.EOF {
		t.Fatalf("read = %v, want EOF", err)
	}
	// EOF 后看门狗未停（Read 错误时不 bump）；Close 停表
	if err := b.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// 停表后 bump 是安全的 no-op
	b.bump()
}

// TestCandidateFirstTokenTimeout kiro 候选覆盖全局；未配置/非 kiro 沿用全局。
func TestCandidateFirstTokenTimeout(t *testing.T) {
	f := &Forwarder{
		firstTokenTimeout:     30 * time.Second,
		kiroFirstTokenTimeout: 15 * time.Second,
	}
	kiroCand := candidate{acc: &account.Account{Type: account.TypeKiro}}
	apiCand := candidate{acc: &account.Account{Type: account.TypeAPIKey}}
	staticCand := candidate{}
	if got := f.candidateFirstTokenTimeout(kiroCand); got != 15*time.Second {
		t.Errorf("kiro cand = %v, want 15s", got)
	}
	if got := f.candidateFirstTokenTimeout(apiCand); got != 30*time.Second {
		t.Errorf("api cand = %v, want 30s", got)
	}
	if got := f.candidateFirstTokenTimeout(staticCand); got != 30*time.Second {
		t.Errorf("static cand = %v, want 30s", got)
	}
	f.kiroFirstTokenTimeout = 0 // 未配置：kiro 也沿用全局
	if got := f.candidateFirstTokenTimeout(kiroCand); got != 30*time.Second {
		t.Errorf("unconfigured kiro cand = %v, want 30s", got)
	}
}
