# relayd

个人 LLM 中转网关：把多个订阅/API 上游聚合成**一个出口**，同时暴露 OpenAI (`/v1/chat/completions`) 与 Anthropic (`/v1/messages`) 协议，支持任意协议互转；按模型路由、余额探测自动禁用/恢复、429 冷却、熔断与半开恢复。Flutter Windows 客户端可视化监控与手动控制。

## 一键本地演示

```powershell
# 构建（首次）
go build -o relayd.exe ./cmd/relayd
go build -o relaymock.exe ./cmd/relaymock
cd frontend && flutter build windows && cd ..

# 启动全部（3 个 localhost mock 上游 + relayd + 客户端）
powershell demo/start.ps1        # 或 git bash: bash demo/start.sh
```

启动后：

- 业务出口 `http://127.0.0.1:8080`（OpenAI + Anthropic 双协议）
- 管理 API `http://127.0.0.1:8081`，客户端 Settings 页填 token `sk-local-change-me`
- 发请求：`curl -N -X POST http://127.0.0.1:8080/v1/chat/completions -H "Authorization: Bearer sk-local-change-me" -H "Content-Type: application/json" -d '{"model":"claude-sonnet-4","stream":true,"messages":[{"role":"user","content":"hi"}]}'`

故障注入演示（客户端 Events 页可见状态流转）：

```bash
# 耗尽 st-a 余额 → 余额探测自动禁用 st-a → 流量切到 st-b
curl -X POST http://127.0.0.1:9001/mock/control -d '{"quota": 0}'
# 让 st-b 限流 429 → 进入冷却
curl -X POST http://127.0.0.1:9002/mock/control -d '{"mode":"ratelimit"}'
# 恢复
curl -X POST http://127.0.0.1:9002/mock/control -d '{"mode":"normal"}'
curl -X POST http://127.0.0.1:9001/mock/control -d '{"quota": 50000000}'
```

## 结构

```
strategy/   零 I/O 决策核心：信号 → 状态机（熔断/冷却/半开）→ 路由（优先级 + SWRR）
backend/    catalog(配置/模型归并) ingress(入口/重试) egress(转发/relaykit 转换)
            probe(余额/ping 探测与调度) admin(管理 API) obs(事件环)
frontend/   Flutter Windows 客户端（KiroaaS 视觉风格）
cmd/relayd      主程序
cmd/relaymock   本地上游模拟器（双协议应答 + /mock/control 故障注入）
```

## 配置

见 `relayd.yaml`（本地演示）与 `relayd.example.yaml`（真实上游模板，含 `credential_files` 批量接入说明）。

## 测试

```bash
go test ./...
cd frontend && flutter analyze
```
