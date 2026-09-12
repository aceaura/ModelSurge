# relayd

四通八达的 LLM 协议转换网关：客户端可以用 **Anthropic / OpenAI Chat / OpenAI Responses / Gemini** 任一协议接入，上游可以是其中任一协议。所有转换经由统一中间表示（IR）中转，协议两两之间不存在直转代码。

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
backend/ir/         统一中间表示：Request/Response/Block/流式事件/Usage/Error
                    事件词汇以 Anthropic streaming 为超集；Usage 以 Anthropic 口径为规范
backend/normalize/  消息规整流水线：合并同角色、首条 user、强制交替、空内容占位、
                    孤儿 tool_result 降级、tool_use/tool_result 配对、schema 清洗
backend/proto/      Codec 接口与注册表；每协议一个子包，init() 自注册：
                    anthropic / openaichat / openairesponses / gemini / kiro
                    每个 codec 只做 协议<->IR 双向转换（请求、流式、非流式、错误）
backend/account/    账号池：SQLite 持久化（凭据/冷却/禁用/熔断/逐笔 usage）与调度
backend/relay/      转发层：上游永远流式、SSE 读取、非流式客户端缓冲聚合
backend/server/     HTTP 入口：按路径识别客户端协议，鉴权后交给 relay
backend/cmd/relayd/     主程序
backend/cmd/relaymock/  本地上游模拟器（OpenAI/Anthropic 应答 + /mock/control 故障注入）
```

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

见 `backend/relayd.example.yaml`。yaml 只含运行参数；上游账号全部保存在 SQLite（`scheduler.db_path`），启动后经管理面 `/admin` 热建号（api-key / kiro 型）：

```yaml
listen: "127.0.0.1:8080"
api_key: "sk-replace-me"

scheduler:
  db_path: ./data/relayd.db # SQLite 文件（必填）
admin:
  api_key: "sk-admin-change-me"
```

```bash
curl -X POST http://127.0.0.1:8080/admin/accounts \
  -H "X-Admin-Key: sk-admin-change-me" -H "Content-Type: application/json" \
  -d '{"name":"claude","type":"api-key","protocol":"anthropic","base_url":"https://api.anthropic.com","api_key":"sk-ant-xxx","models":{"claude-sonnet-4":"claude-sonnet-4-20250514"}}'
```

账号按声明顺序粘性调度，支持限流冷却（429 重置时间入库）、401 禁用、瞬时错误原地重试与熔断切号；逐笔 usage 记账。`GET /v1/models` 返回全部启用账号的模型并集。

## 运行与测试

```bash
cd backend
go build -o relayd.exe ./cmd/relayd
./relayd.exe -config relayd.yaml   # 本地演示配置（配合 relaymock，脚本 demo/start.sh）

go test ./...                      # 单元 + 4x4 跨协议矩阵
go vet ./...
```

调用示例：

```bash
# Anthropic 客户端 -> 任意协议上游
curl -N -X POST http://127.0.0.1:8080/v1/messages \
  -H "Authorization: Bearer sk-local-change-me" -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"hi"}]}'
```
