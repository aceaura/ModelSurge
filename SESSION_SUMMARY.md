# ModelSurge 会话总结

日期：2026-09-14

## 当前定案

仓库已收口为三个独立 Go module 和一个保持不变的 Flutter 工程：

```text
agent/       对外数据面与严格 IR
replay/      UserModel 调度、目标缓存与 Replay admin
upstream/    上游账号、凭据、模型、额度与 Upstream admin
surge/       Flutter 管理前端，功能不变
```

生产请求链路为：

```text
Client → Agent → Replay → Upstream（控制面）
            └────────────→ Upstream LLM API（实际推理 HTTP）
```

Agent 每次请求都从 Replay 获取 `TargetLease`。Replay 的缓存不能使用时请求 Upstream 评估并解析候选。实际 LLM HTTP 和流式响应始终由 Agent 发出和处理。

## 架构不变式

- 请求与响应都严格经过 IR；同协议也不透传。
- Agent 是唯一对外服务，也是唯一 LLM HTTP 发起者。
- 首字节前 Agent 可以携带 `tried_ids` 向 Replay redispatch；首字节后目标锁定。
- `agent.db`、`replay.db`、`upstream.db` 分别由所属进程独占。
- Agent→Replay 与 Replay→Upstream 使用不同 service key。
- `TargetLease` / `ResolvedTarget` 中的凭据不得持久化到 Agent 或 Replay，也不得进入日志。
- surge 本轮不改功能。

详细架构、组件图、时序图、内部 API、数据归属、缓存语义和 Compose 边界见 `design/architecture.md`。

## 当前目录与入口

| 模块 | 入口 | 配置 | 数据库 |
|---|---|---|---|
| Agent | `agent/cmd/agent` | `agent/agent.yaml` | `/data/agent.db` |
| Replay | `replay/cmd/replay` | `replay/replay.yaml` | `/data/replay.db` |
| Upstream | `upstream/cmd/upstream` | `upstream/upstream.yaml` | `/data/upstream.db` |
| surge | Flutter 工程 | Flutter 自有配置 | 无 |

根 `docker-compose.yml` 是推荐部署入口，项目名为 `modelsurge`，健康依赖顺序为：

```text
upstream healthy → replay healthy → agent
```

只有 Agent 默认映射 `127.0.0.1:18099`；Replay `18101` 和 Upstream `18100` 只在 Compose 私网可访问。三个服务分别使用 `agent-data`、`replay-data`、`upstream-data` named volume。

## 配置变量

本地部署前复制 `.env.example` 为 `.env`，并为以下变量填写不同的随机值：

```text
MODELSURGE_AGENT_REPLAY_KEY
MODELSURGE_REPLAY_UPSTREAM_KEY
MODELSURGE_REPLAY_ADMIN_KEY
MODELSURGE_UPSTREAM_ADMIN_KEY
```

可选端口变量：

```text
MODELSURGE_AGENT_BIND=127.0.0.1
MODELSURGE_AGENT_PORT=12345
```

仓库中的正式 YAML 仅引用环境变量，不提供真实密钥。`.env`、数据库、日志和构建产物均不应提交。

## 常用命令

### Compose（推荐）

```bash
cp .env.example .env
# 编辑 .env，填入独立随机密钥
docker compose config
docker compose up -d --build
docker compose ps
docker compose logs -f agent replay upstream
```

停止但保留数据：

```bash
docker compose down
```

除非确认需要删除三库数据，否则不要使用 `docker compose down -v`。

### Go 模块验证

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

### surge 验证

```bash
cd surge
flutter pub get
flutter analyze
flutter build windows --release
```

## 健康检查

Agent 对外健康检查：

```bash
curl http://127.0.0.1:18099/health
```

内部健康检查需要在 Compose 网络内携带对应 Bearer key：

```bash
wget --header="Authorization: Bearer $MODELSURGE_REPLAY_UPSTREAM_KEY" \
  -qO- http://upstream:18100/internal/v1/health

wget --header="Authorization: Bearer $MODELSURGE_AGENT_REPLAY_KEY" \
  -qO- http://replay:18101/internal/v1/health
```

## 调用示例

在 Upstream 创建账号/模型并在 Replay 创建 UserModel/Group 后，通过 Agent 调用：

```bash
curl -N -X POST http://127.0.0.1:18099/v1/chat/completions \
  -H 'Authorization: Bearer <UserModel client key>' \
  -H 'Content-Type: application/json' \
  -d '{"model":"configured-user-model","stream":true,"messages":[{"role":"user","content":"hi"}]}'
```

## 验证边界

本次仓库收口不运行 Docker，也不创建本地 `.env`、SQLite、日志或构建产物。后续需要在具备 Docker 的环境中验证：

1. `docker compose config` 能正确展开。
2. 三个 Dockerfile 能构建，镜像内有 `ca-certificates`、`tzdata`、`wget`。
3. Upstream 健康后 Replay 启动，Replay 健康后 Agent 启动。
4. 只有 Agent 暴露宿主端口。
5. 两段内部密钥不可互换，Replay/Upstream admin key 独立。
6. 三个 named volume 各自产生对应数据库。
7. 每请求 dispatch、缓存失效后的 Upstream evaluate、首字节前 redispatch 和首字节后锁定符合设计。
8. surge 行为没有变化。
