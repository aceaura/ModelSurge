package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// TestServeShutdown 优雅停机：sigCtx 取消后 serve 等在途请求完成才返回，
// 而不是立即断连；请求放行后 serve 正常退出。
func TestServeShutdown(t *testing.T) {
	// 找一个空闲端口给 ListenAndServe 用
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	release := make(chan struct{})
	var hits atomic.Int32
	srv := &http.Server{
		Addr: addr,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			hits.Add(1)
			w.WriteHeader(200)
			<-release // 模拟未完成的流式响应
		}),
	}
	sigCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- serve(srv, sigCtx) }()

	// 等服务就绪
	deadline := time.Now().Add(2 * time.Second)
	for {
		conn, derr := net.DialTimeout("tcp", addr, 50*time.Millisecond)
		if derr == nil {
			conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("server did not come up")
		}
		time.Sleep(20 * time.Millisecond)
	}

	clientDone := make(chan struct{})
	go func() {
		defer close(clientDone)
		resp, cerr := http.Get("http://" + addr)
		if cerr != nil {
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()
	for hits.Load() == 0 {
		time.Sleep(10 * time.Millisecond)
	}

	cancel() // 模拟 SIGINT/SIGTERM

	// 在途请求未放行时 serve 不得返回
	select {
	case <-done:
		t.Fatal("serve returned before in-flight request finished")
	case <-time.After(200 * time.Millisecond):
	}

	close(release)
	<-clientDone
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return after shutdown")
	}
}

// TestServeListenError 端口被占时 serve 立即返回错误而不是阻塞等信号。
func TestServeListenError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	srv := &http.Server{Addr: ln.Addr().String(), Handler: http.NotFoundHandler()}
	sigCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- serve(srv, sigCtx) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("serve should fail on busy port")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("serve did not report listen error")
	}
}
