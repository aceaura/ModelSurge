# ModelSurge

ModelSurge 是由 `surge`、`relay`、`upstream` 三个进程组成的 LLM 协议转换网关。客户端可以用 **Anthropic / OpenAI Chat / OpenAI Responses / Gemini** 任一协议接入，上游可以是任一已注册协议。所有转换都由 `relay` 经统一中间表示（IR）中转，同协议也禁止透传。

## 入口矩阵

单一监听地址（如 `http://1.1.1.1:8080`），按路径前缀区分接入协议：

| 客户端入口 | 协议 |
|---|---|
| `POST /anthropic/v1/messages` | anthropic |
| `POST /anthropic/v1/messages/count_tokens` | anthropic 计数（无 anthropic 上游时本地粗估） |
| `POST /openai/v1/chat/completions` | openai-chat |
| `POST /openai/v1/responses` | openai-responses |
| `POST /gemini/v1beta/models/{model}:generateContent` | gemini（非流式） |
| `POST /gemini/v1beta/models/{model}:streamGenerateContent` | gemini（流式） |
| `GET /openai/v1/models` | 模型列表 |

无前缀的原生路径（`/v1/messages`、`/v1/chat/completions` 等）同样保留可用。

4 客户端协议 × 4 上游协议 × 流式/非流式 = 32 条路径全部支持（`backend/server/e2e_cross_test.go` 矩阵覆盖）。

## 架构

```
surge/                      Flutter 管理前端，本阶段保留现有功能
backend/cmd/relay/           客户端 HTTP、UserModelGroup 调度、IR、codec、转发与响应回写
backend/cmd/upstream/        UpstreamModel、账号凭据、Kiro token、额度与评估控制面
backend/relaystore/          relay.db：UserModel、调度组、Policy、组内目标缓存
backend/upstreamstore/       upstream.db：账号、UpstreamModel、状态、额度与 usage
backend/contract/upstreamv1/ relay 与 upstream 的版本化 HTTP JSON DTO
backend/ir/                  协议无关 Request/Response/Block/流事件/Usage/Error
backend/proto/               anthropic / openaichat / openairesponses / gemini / kiro codec
backend/relay/               上游调用、流解码、pre-write 重试与响应聚合
backend/cmd/relaymock/       本地上游模拟器
```

`relay` 与 `upstream` 仅通过带 service key 的内部 HTTP JSON API 通信；协议原始字节只由 `relay` 处理。两进程分别独占 `relay.db` 和 `upstream.db`，不跨库直读。

核心设计（调研 new-api / sub2api / kiro-gateway 后的提炼，详见 `docs/protocol-conversion-study.md`）：

- **IR 枢纽**：跨协议转换 = 解码为 IR + 从 IR 编码，无 N² 直转。
- **上游永远流式**：强制 `stream=true`；客户端要非流式时网关聚合 SSE 后一次性返回 JSON。
- **block 开合不变式**：编码器保证 start→delta*→stop，断流由 Finish 兜底补齐终止事件。
- **签名链保真与防 400**：签名按到达时的协议形态标记（`SignatureFrom`）。同协议往返原样透传；跨协议回放时保守降级（Anthropic 降为 text 块 / Responses 不构造 reasoning item / Gemini 置空 thoughtSignature）——Anthropic 对历史 thinking 块强制签名校验、Gemini 3 校验 functionCall 签名，透传外族签名必 400，宁可断签名链保住请求。降级均记入诊断。
- **能力声明 + 诊断**：codec 声明能力（`Caps()`：thinking 签名/图片/托管工具），转发前对比请求特征，必然有损项记日志并写入 `X-Relayd-Notes` 响应头，不再静默丢失。
- **字节未出前可重试**：连接失败 / 429 / 5xx / 首事件超时（`first_token_timeout`，默认 30s）且尚未向客户端写字节时，换下一个候选上游重发；写出第一字节后锁死，错误只在流内渲染。
- **托管工具声明映射**：Anthropic `web_search`/`code_execution` ↔ Responses `web_search`/`code_interpreter` ↔ Gemini `google_search`/`code_execution` 三向互转（仍由上游服务器执行，网关不做仿真）；Chat Completions 无此能力，丢弃并记诊断。
- **usage 估算兜底（opt-in）**：`estimate_usage: true` 后上游不上报 usage 时按文本粗估并在日志标注，默认关闭（估算值不代表真实计费）。
- **访问日志**：`access_log`（默认开）记录每请求的 method/path/status/耗时（含鉴权失败的 401）。
- **优雅停机**：SIGINT/SIGTERM 后停止收新请求，等在途请求（含流式响应）最多 15s 完成再退出。

## 配置

运行配置拆为 `backend/relay.yaml` 与 `backend/upstream.yaml`：

```yaml
# relay.yaml
listen: 0.0.0.0:18099
db_path: /data/relay.db
upstream_url: http://upstream:18100
service_key: local-service-key

# upstream.yaml
listen: 0.0.0.0:18100
db_path: /data/upstream.db
service_key: local-service-key
```

账号与 UpstreamModel 归 `upstream` 管理；UserModel、调度组、Policy 和目标缓存归 `relay` 管理：

```bash
curl -X POST http://127.0.0.1:18100/admin/accounts \
  -H "X-Admin-Key: change-upstream-admin-key" -H "Content-Type: application/json" \
  -d '{"name":"claude","type":"api-key","protocol":"anthropic","base_url":"https://api.anthropic.com","api_key":"sk-ant-xxx","models":{"claude-sonnet-4":"claude-sonnet-4-20250514"}}'
```

账号按声明顺序粘性调度，支持限流冷却（429 重置时间入库）、401 禁用、瞬时错误原地重试与熔断切号；逐笔 usage 记账。`GET /v1/models` 返回全部启用账号的模型并集。

api-key 账号的 `base_url` 支持自适应探测（GET 模型列表，零 token 消耗）：裸域名自动补 scheme；带 `/v1` 或完整端点的 SDK 风格写法自动剥版本段/端点尾段；`/api/v1` 等网关路径自动试出正确根地址。建号/更新时探测并把解析出的根地址写回 `base_url`（响应带 `probe` 报告：逐候选证据 + 上游模型列表）；`POST /admin/accounts/{name}/test` 可随时重探测修正。

kiro 账号支持三种认证方法（`refresh_token` / `creds_file` / `cli_db`），每种均可用路径或内联字符串接入（`creds_text` 文本 XOR `creds_b64` base64，与路径字段三选一）：文本区直接粘贴凭据即可建号，无需往服务器放文件。`refresh_token` 内联支持裸串或 `{"refreshToken":...}` JSON；`creds_file` 内联为 credentials.json 原文；`cli_db` 内联为提取的凭据 JSON 或 base64(SQLite 库文件)。token 轮转统一由 `token_state` 列持久化，内联字段不回写。

## 运行与测试

```bash
cd backend
go build -o upstream.exe ./cmd/upstream
go build -o relay.exe ./cmd/relay
go build -o relaymock.exe ./cmd/relaymock
bash demo/start.sh

go test ./...
go vet ./...

cd ../surge
flutter pub get
flutter analyze
flutter build windows --release
```

也可以在 `backend/` 运行 `docker compose up --build`，默认只向宿主机暴露 `relay` 的 `127.0.0.1:18099`。

调用示例：

```bash
# Anthropic 客户端 -> 任意协议上游
curl -N -X POST http://127.0.0.1:18099/v1/messages \
  -H "Authorization: Bearer change-client-key" -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"hi"}]}'
```
