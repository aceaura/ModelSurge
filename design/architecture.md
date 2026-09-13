# relayd 调度架构设计图

> 2026-09-13 定案。核心原则：**所有协议（客户端侧 + 上游侧）任何一步都必须经 IR 交汇，两两不直转；同协议（如 anthropic→anthropic）也必须先解码为 IR 再重新编码，禁止透传**。UserModelGroup / Upstream Info 是纯控制面，不经手协议字节。

## 一、数据闭环（请求/响应串行路径）

```text
请求：Client → HTTP Server（DecodeRequest）→ IR → Outbound Codec（EncodeRequest）→ Upstream API
响应：Upstream API → Outbound Codec（解码回 IR 事件）→ IR → HTTP Server（客户端协议编码 + 回写）→ Client
```

HTTP Server 是客户端侧唯一出入口；IR 永远不直接面对连接，只面对 codec。

## 二、组件图

```mermaid
flowchart TB
    CL["Client 客户端"]

    subgraph UME["UserModel 用户模型 · 客户端接入实体"]
        direction LR
        F1["UserModelName<br/>用户自定义模型名"]
        F2["HTTP Server 地址"]
        F3["协议类型<br/>三家四大协议"]
        F4["密钥"]
    end

    HS["HTTP Server<br/>兼容三家四大协议<br/>anthropic · openai-chat · openai-responses · gemini"]

    IR["IR<br/>协议唯一交汇点"]

    subgraph UMG["UserModelGroup 调度组<br/>用户选定的模型名 → 一组 UpstreamModel"]
        TC["目标缓存<br/>缓存目标 + 上次请求结果"]
        POL["Policy 策略<br/>preset / dynamic"]
        UMS["UpstreamModel × N"]
        POL -->|按策略排序调度| UMS
        POL -->|调度后写入目标| TC
        TC -.->|命中且上次正常：复用缓存成员| UMS
    end

    subgraph UI["Upstream Info · 每个 UpstreamModel 一份"]
        direction LR
        I1["请求覆盖参数"]
        I2["Upstream 模型名"]
        I3["Upstream 地址 + 密钥"]
        I4["额度查询接口<br/>评估方法接口"]
    end

    OC["Outbound Codec<br/>按 UpstreamModel 的协议"]
    UPA["Upstream LLM API"]

    CL -->|调用| UME
    UME -->|接入| HS
    HS -->|DecodeRequest → IR| IR
    IR -->|按模型名路由| UMG
    IR -.->|上报调用结果 · 更新缓存有效性| TC
    UMS ---|包含| UI
    POL -.->|调度读额度/评估| I4
    IR -->|IR 请求 + 覆盖参数| OC
    OC -->|编码为上游协议| UPA
    UPA -->|原生响应流| OC
    OC -->|解码回 IR 事件| IR
    IR -->|EncodeResponse · 客户端协议编码| HS
    HS -->|响应回写| CL
```

## 三、时序图（含目标缓存）

```mermaid
sequenceDiagram
    autonumber
    participant C as Client
    participant HS as HTTP Server<br/>（三家四大协议）
    participant IR as IR
    participant G as User Model Group<br/>（含目标缓存）
    participant UM as Upstream Model ×N
    participant UI as Upstream Info
    participant A as Upstream API

    Note over C,HS: UserModel = UserModelName + 协议类型 + 密钥（地址即 HTTP Server）
    C->>HS: 调用 User Model
    HS->>HS: 校验密钥 · 识别协议
    HS->>IR: DecodeRequest：解码为 IR（同协议也必须过 IR）
    IR->>G: 按 UserModelName 定位调度组

    G->>G: 读组内目标缓存（上次选定的 Upstream Model + 上次请求结果）
    alt 缓存命中 且 上次请求正常
        Note over G: 沿用缓存目标（跳过策略调度）
    else 缓存未命中 或 上次请求异常
        G->>UM: 策略调度：遍历组内 Upstream Models
        UM->>UI: 调额度查询接口 + 评估方法接口
        UI-->>UM: 额度 / 评估结果
        UM-->>G: 候选排序，选出成员
        G->>G: 新目标写入组内缓存
    end

    G->>UI: 读取目标的 Upstream Info
    UI-->>G: 请求覆盖参数 + Upstream 模型名 + 地址 + 密钥
    G-->>IR: 目标确定，IR 请求继续
    IR->>A: 应用覆盖参数 · 按上游协议编码<br/>用地址 + 密钥调用（上游模型名）
    A-->>IR: 上游原生响应流
    IR->>G: 上报本次结果（成功→缓存保持有效；失败→标记重调度）
    IR->>HS: 解码回 IR 事件 → 按客户端协议编码
    HS-->>C: 回写响应（流式逐块 / 聚合一次性）

    Note over G: 失败时本请求内可重走策略调度换候选重试（限写出首字节前）
```

**缓存语义**：

- 快路径（缓存命中且上次正常）：定位组 → 读缓存 → 直接取 Upstream Info 调上游，跳过整段策略调度（粘性调度，保上游 prompt cache 命中）
- 慢路径（未命中 / 上次异常）：完整走策略调度（额度查询 + 评估 → 排序 → 选成员），新目标写回缓存
- 结果上报是缓存生命周期钩子：成功保持有效，失败失效，下一请求自然落回慢路径

## 四、实体定义

**UserModel**（客户端看到的东西）：

| 字段 | 说明 |
|---|---|
| UserModelName | 用户自定义模型名 |
| HTTP Server 地址 | 客户端接入端点 |
| 协议类型 | 三家四大协议：anthropic / openai-chat / openai-responses / gemini |
| 密钥 | 客户端接入凭据 |

**UpstreamModel → Upstream Info**（调度组内每个成员携带）：

| 要素 | 说明 |
|---|---|
| 请求覆盖参数 + Upstream 模型名 | 转发前覆盖请求参数（叠加序 client < account < model）；native 模型名 |
| Upstream 地址 + 密钥 | 上游端点与凭据 |
| 额度查询接口 | 查询上游账号剩余额度 |
| 评估方法接口 | 评估可用性/成本，作为 Policy 调度依据 |

## 五、概念到代码包的映射

| 设计概念 | 代码落点 |
|---|---|
| UserModel / UserModelGroup / Policy / 目标缓存 | 新 `backend/schedule` + server 鉴权扩展 |
| UpstreamModel / Upstream Info | `account` 目录（store_v2 改表：user_models / schedule_groups / upstream_models） |
| HTTP Server（inbound codec） | `backend/server` |
| Outbound Codec | `backend/proto`（各协议 codec，含 normalize） |
| IR | `backend/ir`（Request/Response/流事件超集/Aggregator/Overrides） |
| 转发/重试/流控 | `backend/relay`（候选循环、pre-write 重试边界不变） |
