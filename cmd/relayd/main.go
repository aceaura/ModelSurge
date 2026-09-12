// relayd：四通八达的 LLM 协议转换网关。
// 客户端可用 Anthropic / OpenAI Chat / OpenAI Responses / Gemini 任一协议接入，
// 上游可以是其中任一协议，转换经由统一 IR 中转。
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"relayd/backend/config"
	"relayd/backend/proto"
	"relayd/backend/server"
)

// shutdownGrace 等待在途请求（含流式响应）完成的上限。
const shutdownGrace = 15 * time.Second

func main() {
	cfgPath := flag.String("config", "relayd.yaml", "配置文件路径")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           server.New(cfg).Handler(),
		ReadHeaderTimeout: 30 * time.Second, // 防 slowloris 慢速连接占资源
	}
	log.Printf("relayd listening on %s, protocols: %v, upstreams: %d", cfg.Listen, proto.Names(), len(cfg.Upstreams))

	// 优雅停机：SIGINT/SIGTERM 后停止收新请求，
	// 等在途请求（含流式响应）完成或超时强制断开。
	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := serve(srv, sigCtx); err != nil {
		log.Fatal(err)
	}
}

// serve 启动 HTTP 服务并阻塞：sigCtx 取消（收到信号）后优雅停机；
// ListenAndServe 自身出错（如端口占用）立即返回。
func serve(srv *http.Server, sigCtx context.Context) error {
	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()
	select {
	case err := <-errCh:
		return err
	case <-sigCtx.Done():
		log.Printf("relayd shutting down, waiting up to %s for in-flight requests", shutdownGrace)
		ctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			log.Printf("relayd: forced shutdown: %v", err)
		}
		<-errCh
		log.Printf("relayd stopped")
		return nil
	}
}
