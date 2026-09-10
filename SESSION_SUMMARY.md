# relayd / ModelSurge 会话总结

日期：2026-09-09

## 项目目标

`relayd` 是一个个人 LLM relay gateway：把多个 OpenAI、Anthropic 等协议的上游订阅聚合成一个本地出口，按 canonical model 统一路由，并在余额耗尽、429、连续失败或探测失败时自动摘除上游；恢复后通过 half-open 重新接入。项目分为三层：

- `strategy/`：无 I/O 的状态机、信号分类、熔断与优先级/SWRR 路由。
- `backend/`：配置与模型 catalog、HTTP ingress、relaykit 协议转换、egress 转发、余额/ping 探测、调度、admin API、事件环。
- `frontend/`：Flutter Windows 管理客户端，采用深色壳、浅色内容区、柠檬绿强调色的 KiroaaS 风格。

## 本会话完成的代码

### 本地上游模拟器

新增 `cmd/relaymock`，一个进程可以启动多个 mock 上游实例：

- OpenAI：`POST /v1/chat/completions`，支持流式和非流式响应。
- Anthropic：`POST /v1/messages`，支持流式和非流式响应。
- OpenAI 余额探测：`GET /api/user/self`，返回 `data.quota`。
- 控制接口：`POST /mock/control`，支持 `quota`、`mode`、`delay_ms`；故障模式包括 `normal`、`ratelimit`、`slow`、`down`、`auth_error`。
- 状态接口：`GET /mock/state`。

演示配置在 `demo/relaymock.yaml`，默认启动 `st-a:9001`、`st-b:9002`、`cl-01:9003`。

### localhost 闭环

`relayd.yaml` 已改为完全本地配置：

- relay：`127.0.0.1:8080`
- admin：`127.0.0.1:8081`
- admin token：`sk-local-change-me`
- 3 个上游：两个 OpenAI mock station、一个 Anthropic mock。

一键启动脚本：

- Windows：`powershell -ExecutionPolicy Bypass -File demo/start.ps1`
- Git Bash：`bash demo/start.sh`

脚本会构建产物之外启动 mock、relayd 和已构建的 Flutter 客户端，并检查 admin API。

### 协议转换修复与实测

新增 `e2e_cross_test.go`，覆盖四条路径：

1. Claude 客户端 → OpenAI 上游，非流式。
2. Claude 客户端 → OpenAI 上游，流式。
3. OpenAI 客户端 → Claude 上游，非流式。
4. OpenAI 客户端 → Claude 上游，流式。

过程中修复了两个实际问题：

- relaykit 的请求 converter 保留源请求中的 `model` 字段；relayd 现在在转换完成后显式改写为上游 native model。
- 非流式响应此前被错误送入 SSE 流转换器；新增 `ConvertResponseBody`，完整响应按 DTO 转换，流式响应继续走 `StreamConvert`。

### 后端健壮性

- `request_timeout` 已通过 `NewForwarderWithTimeout` 接入上游转发；响应 body 关闭时取消上下文，避免影响流式 body 的读取。
- `Snapshot` 的可选时间字段改为指针，JSON 不再泄露 `0001-01-01T00:00:00Z`。
- 429 cooldown 会通过事件回调进入 admin events。
- 每个上游记录请求数、成功数和最近延迟，并由 admin API 返回。
- 成功的 balance/ping 探测可以把 `auto_disabled` 上游直接推进 `half_open`，再按 `success_to_close` 恢复为 enabled；不再无条件等待完整 open window。

### Flutter 客户端

- token 和 admin 地址使用 `shared_preferences` 持久化，重启后自动恢复。
- Dashboard、Events、Models、Upstreams 对 admin API 错误有明确提示，不再无限 spinner 或空白。
- Upstream card 增加余额进度条、请求/成功数、最近延迟。
- `flutter analyze` 通过，Windows Release 构建通过。

## 验证结果

在 Windows 环境完成：

```text
go vet ./...
go test ./...
flutter analyze
flutter build windows
```

均通过。运行时验证包括：

- OpenAI/Anthropic mock 的流式和非流式响应。
- st-a 余额耗尽 → `auto_disabled` → 请求切到 st-b。
- 余额恢复 → `half_open` → `enabled`。
- 429 → `rate limited, cooldown 5s` 事件可在 `/admin/events` 查看。
- admin JSON 不再包含零时间字段。

## 常用命令

构建并启动：

```powershell
go build -o relayd.exe ./cmd/relayd
go build -o relaymock.exe ./cmd/relaymock
cd frontend
flutter pub get
flutter analyze
flutter build windows
cd ..
powershell -ExecutionPolicy Bypass -File demo/start.ps1
```

查看状态：

```bash
curl -H 'Authorization: Bearer sk-local-change-me' \
  http://127.0.0.1:8081/admin/summary
```

调用本地出口：

```bash
curl -N -X POST http://127.0.0.1:8080/v1/chat/completions \
  -H 'Authorization: Bearer sk-local-change-me' \
  -H 'Content-Type: application/json' \
  -d '{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}'
```

故障注入：

```bash
curl -X POST http://127.0.0.1:9001/mock/control -d '{"quota":0}'
curl -X POST http://127.0.0.1:9002/mock/control -d '{"mode":"ratelimit"}'
curl -X POST http://127.0.0.1:9002/mock/control -d '{"mode":"normal"}'
```

## 提交边界与后续工作

提交应包含 Go 后端、mock、demo 配置/脚本、Flutter 源码、测试、`README.md` 和本总结文档；不应包含：

- `relayd.exe`、`relaymock.exe` 等构建产物。
- `*.log` 运行日志。
- `frontend/build/`、`frontend/.dart_tool/`、`frontend/.idea/` 等生成目录。
- 真实 API key 或真实上游配置。

`relayd.example.yaml` 仍是脱敏的真实上游配置模板；`relayd.yaml` 是本地演示配置。