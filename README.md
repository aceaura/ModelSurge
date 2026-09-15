# ModelSurge

ModelSurge 是一个由三个独立 Go module 组成的 LLM 协议转换网关：

```text
Client → Agent → Replay → Upstream
            └──────────────→ Upstream LLM API
```

- **Agent** 是唯一对外进程，每次请求都向 Replay 获取 `TargetLease`，并由 Agent 实际发出 LLM HTTP 请求。
- **Replay** 管理 UserModel、调度组、Policy 和目标缓存；缓存不可用时向 Upstream 请求评估与解析。
- **Upstream** 管理账号、凭据、UpstreamModel、token、额度、冷却和运行状态。
- **surge** 保持现有 Flutter 管理前端功能不变。

所有协议转换严格经过 IR；即使入站与目标协议相同也禁止透传。首字节写出前 Agent 可携带已尝试目标向 Replay redispatch；首字节写出后目标锁定。

完整设计见 [`design/architecture.md`](design/architecture.md)。

## 仓库结构

```text
agent/       Agent 独立 Go module；对外 HTTP、codec、IR、LLM 调用、agent.db
replay/      Replay 独立 Go module；调度、缓存、Replay admin、replay.db
upstream/    Upstream 独立 Go module；账号/凭据/模型/额度、Upstream admin、upstream.db
surge/       Flutter 管理前端，功能保持不变
design/      架构设计
docs/        协议转换研究资料
docker-compose.yml
.env.example
```

三个数据库由各自进程独占，不跨库直读；两段内部 API 使用不同密钥：

- `MODELSURGE_AGENT_REPLAY_KEY`
- `MODELSURGE_REPLAY_UPSTREAM_KEY`

## 对外协议入口

Agent 默认监听 `127.0.0.1:18099`（Compose 容器内为 `0.0.0.0:18099`）。

| 客户端入口 | 协议 |
|---|---|
| `POST /anthropic/v1/messages` | Anthropic |
| `POST /anthropic/v1/messages/count_tokens` | Anthropic token 计数 |
| `POST /openai/v1/chat/completions` | OpenAI Chat Completions |
| `POST /openai/v1/responses` | OpenAI Responses |
| `POST /gemini/v1beta/models/{model}:generateContent` | Gemini 非流式 |
| `POST /gemini/v1beta/models/{model}:streamGenerateContent` | Gemini 流式 |
| `GET /openai/v1/models` | UserModel 列表 |
| `GET /health` | Agent 健康检查 |

无前缀兼容入口（如 `/v1/messages`、`/v1/chat/completions`、`/v1/responses`、`/v1/models`）仍由 Agent 提供。

### 协议支持矩阵

| 协议 | 客户端入站 | 上游出口 |
|---|---:|---:|
| Anthropic | 支持 | 支持 |
| OpenAI Chat Completions | 支持 | 支持 |
| OpenAI Responses | 支持 | 支持 |
| Gemini | 支持 | **不支持（inbound-only）** |
| Codex | 不作为客户端入口 | 支持 |
| Kiro | 不作为客户端入口 | 支持（独立账号类型） |

Gemini 请求和响应仍由 Agent 的 Gemini codec、流编码器、错误渲染及路由处理，但 `TargetLease` / `UpstreamModel` 不允许使用 Gemini 作为出口协议。出口协议严格限定为 `anthropic`、`openai-chat`、`openai-responses`、`codex`、`kiro`；未知协议不会回落为 OpenAI。

## 核心行为

- **严格 IR**：客户端请求先解码为 IR，再编码为目标协议；响应反向执行。不存在同协议快捷路径。
- **Agent 执行数据面**：Replay 和 Upstream 只传内部 JSON DTO，实际 LLM HTTP/SSE 由 Agent 处理。
- **每请求租约**：Agent 不复用旧租约，每个客户端请求都调用 Replay `/internal/v1/dispatch`。
- **Replay 缓存与 failover**：正常缓存走快速 resolve；缓存缺失、异常、过期或目标已尝试时，Replay 请求 Upstream evaluate，再按 Policy 选择目标。
- **首字节边界**：首字节前可换目标；首字节后禁止 redispatch。
- **独立持久化**：agent/replay/upstream 数据分属三个 PostgreSQL database（compose 三库三 role）。
- **幂等结果上报**：Agent→Replay→Upstream 的结果链按 report ID 支持安全重试。

## 配置

正式配置位于：

- `agent/agent.yaml`
- `replay/replay.yaml`
- `upstream/upstream.yaml`

配置文件通过 `${ENV_VAR}` 注入密钥，不包含默认真实值。Compose 内部 URL 使用服务名：

```yaml
# agent/agent.yaml
replay_url: http://replay:18101
service_key: ${MODELSURGE_AGENT_REPLAY_KEY}
db_dsn: ${MODELSURGE_AGENT_DB_DSN}

# replay/replay.yaml
upstream_url: http://upstream:18100
agent_service_key: ${MODELSURGE_AGENT_REPLAY_KEY}
upstream_service_key: ${MODELSURGE_REPLAY_UPSTREAM_KEY}
db_dsn: ${MODELSURGE_REPLAY_DB_DSN}

# upstream/upstream.yaml
service_key: ${MODELSURGE_REPLAY_UPSTREAM_KEY}
db_dsn: ${MODELSURGE_UPSTREAM_DB_DSN}
```

Replay 和 Upstream 管理面使用独立 admin key，均只在 Compose 私网 listener 上提供；默认不映射宿主端口。

## Compose 部署（推荐）

本仓库以根目录 Compose 为主要部署方式：

```bash
cp .env.example .env
# 将 .env 中所有空密钥填为不同的随机值
docker compose config
docker compose up -d --build
docker compose ps
```

Compose 项目名固定为 `modelsurge`，健康依赖顺序为 `config-check + postgres + redis → upstream → replay → agent`。只有 Agent 映射宿主端口：

```text
${MODELSURGE_AGENT_BIND:-127.0.0.1}:${MODELSURGE_AGENT_PORT:-12345}
```

数据卷：

- `modelsurge_pg-data` → PostgreSQL 三库（agent/replay/upstream）持久化数据

历史 SQLite named volume（`modelsurge_agent-data`/`replay-data`/`upstream-data`）在当前形态下不再挂载，成为孤儿卷；确认无用后可用 `docker volume rm` 清理。

停止服务使用 `docker compose down`。除非确认要删除全部持久化数据，否则不要添加 `-v`。

仓库不再维护旧本地 demo 启动脚本；需要联调时以 Compose 和管理 API 为主。

## 管理面

Upstream 管理账号和上游模型，例如：

```bash
# Upstream 默认不映射宿主端口；请从 Compose 网络内或临时安全转发后调用。
curl -X POST http://upstream:18100/admin/accounts \
  -H "X-Admin-Key: $MODELSURGE_UPSTREAM_ADMIN_KEY" \
  -H "Content-Type: application/json" \
  -d '{"name":"provider","type":"api-key","protocol":"anthropic","base_url":"https://api.anthropic.com","api_key":"replace-locally","models":{"claude-sonnet":"claude-sonnet"}}'
```

Replay 管理 UserModel、Group、成员、Policy 和缓存，接口位于 `http://replay:18101/admin/*`，使用 `MODELSURGE_REPLAY_ADMIN_KEY`。

不要把上游真实 key、admin key 或内部 service key 写入 YAML、命令历史或版本库。

## 本地构建与测试

三个 module 分别运行：

```bash
cd upstream
go test ./...
go vet ./...
go build ./cmd/upstream

cd ../replay
go test ./...
go vet ./...
go build ./cmd/replay

cd ../agent
go test ./...
go vet ./...
go build ./cmd/agent
```

Flutter 前端命令保持不变：

```bash
cd surge
flutter pub get
flutter analyze
flutter build windows --release
```

## 调用示例

完成 Upstream 账号与 Replay UserModel 配置后，通过 Agent 调用：

```bash
curl -N -X POST http://127.0.0.1:18099/v1/chat/completions \
  -H "Authorization: Bearer <UserModel client key>" \
  -H "Content-Type: application/json" \
  -d '{"model":"configured-user-model","stream":true,"messages":[{"role":"user","content":"hi"}]}'
```
