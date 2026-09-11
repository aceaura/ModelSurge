// relayd：四通八达的 LLM 协议转换网关。
// 客户端可用 Anthropic / OpenAI Chat / OpenAI Responses / Gemini 任一协议接入，
// 上游可以是其中任一协议，转换经由统一 IR 中转。
package main

import (
	"flag"
	"log"
	"net/http"

	"relayd/backend/config"
	"relayd/backend/proto"
	"relayd/backend/server"
)

func main() {
	cfgPath := flag.String("config", "relayd.yaml", "配置文件路径")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	srv := server.New(cfg)
	log.Printf("relayd listening on %s, protocols: %v, upstreams: %d", cfg.Listen, proto.Names(), len(cfg.Upstreams))
	if err := http.ListenAndServe(cfg.Listen, srv.Handler()); err != nil {
		log.Fatal(err)
	}
}
