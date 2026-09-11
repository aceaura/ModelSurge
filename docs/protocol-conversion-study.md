# LLM 协议转换实现调研：new-api / sub2api / kiro-gateway

日期：2026-09-10
调研对象（均为 2026-09-09 拉取的快照）：

| 项目 | 本地路径 | 语言 | 定位 |
|---|---|---|---|
| QuantumNous/new-api | `W:\github.com\QuantumNous\new-api` | Go | one-api 衍生版，40+ 上游聚合分发，多渠道网关 |
| Wei-Shaw/sub2api | `W:\github.com\Wei-Shaw\sub2api` | Go (Gin + ent) | 把 Claude/OpenAI/Gemini/Grok 等**订阅账号**中转成标准 API |
| jwadow/kiro-gateway | `W:\github.com\jwadow\kiro-gateway` | Python (FastAPI) | 把 Kiro IDE / CodeWhisperer 订阅代理成 OpenAI/Anthropic API |

本文回答一个核心问题：**这三个项目各自如何把各种上游协议转换成 OpenAI 和 Anthropic 协议**（以及反向），并提炼对 ModelSurge/relayd 的可借鉴设计。所有代码引用格式为 `文件路径:行号`。

---

## 第一部分：三种架构范式对比（速览）

| 维度 | new-api | sub2api | kiro-gateway |
|---|---|---|---|
| 转换层位置 | 独立 Go module `relaykit/`（DTO + 转换引擎） | `backend/internal/pkg/apicompat/`（纯转换）+ `service/`（编排） | `kiro/converters_*.py`（适配器）+ `converters_core.py`（核心） |
| 中间表示（IR） | **无消息级 IR**；A→B 直转 + 注册表多跳 | **无统一 IR**；DTO 直转，Responses 做枢纽 | **有真 IR**：`UnifiedMessage`/`UnifiedTool`/`ThinkingConfig` |
| 枢纽格式 | OpenAI Chat Completions | OpenAI Responses | 无（唯一上游协议，IR 即枢纽） |
| 多跳链 | 声明式 `StepConverters`（如 Claude→OAI→Gemini） | 手写两跳/三跳链式调用 | 不需要 |
| 直转桥例外 | — | Anthropic↔ChatCompletions 直转桥（避免双状态机损耗） | — |
| 流式策略 | 每方向一个有状态流转换器 + Finalize 兜底 | 同左（State + Events + Finalize 三件套） | 统一 `KiroEvent` 事件流 → 两个出口格式器 |
| 上游流形态 | 按上游原生（流式/非流式都支持） | **上游永远流式**，非流式 = 网关缓冲聚合 | **上游只有流式**（AWS 二进制事件流），非流式 = 聚合 |
| 工具系统 | **独立工具 IR**（`toolconv` 包，Extract/Attach） | 各转换器内处理 + Codex 工具降级 | 统一 IR 内处理 + 无 tools 时文本降级 |
| thinking/reasoning | 独立 `reasoning` 包（Intent/Effort 抽象） | 三种签名形态显式互认（见 §3.8） | **伪 thinking**：注入标签诱导 + 出口剥离 |
| 错误转换 | 万能解析 → 内部统一错误 → 按客户端协议渲染 | 每种客户端协议各写一个错误函数 | Kiro reason 分类 → FATAL/RECOVERABLE |

**范式结论**：
- new-api 代表**注册表 + 格式路由**的工程化极致，适合上游种类爆炸的场景。
- sub2api 代表 **hub + 直转桥混合**，在高频有损路径上保留直转，兼顾 O(n) 转换器数量与转换质量。
- kiro-gateway 代表**单上游 + 真 IR**，当上游只有一个协议时，统一中间表示最干净。

---

## 第二部分：new-api（Go，注册表 + 枢纽多跳）

### 2.1 分层与调度链路

四层结构：

| 层 | 位置 | 职责 |
|---|---|---|
| 入口/调度 | `controller/relay.go`、`router/relay-router.go` | 按 URL 确定客户端协议格式（RelayFormat），鉴权、计费预扣、重试 |
| 中转主流程 | `relay/*.go`（compatible_handler / claude_handler / gemini_handler / responses_handler） | 每种客户端协议一个 Helper：模型映射 → adaptor 转换请求 → 转发 → 转换响应 |
| 渠道适配器 | `relay/channel/<provider>/`（40+ 个目录） | 每个上游一个 Adaptor：URL、鉴权头、请求转换、响应/流处理 |
| 转换引擎 | `relaykit/`（**独立 go.mod**）：`relaykit/dto`、`relaykit/relayconvert`、`relaykit/types` | 全部协议 DTO + 注册表式 A→B 转换器 |

一次 `/v1/chat/completions` 的完整链路：

1. 路由按路径确定 `RelayFormat`（`router/relay-router.go:79-192`）：`/v1/messages`→Claude、`/v1/chat/completions`→OpenAI、`/v1/responses`→OpenAIResponses、`generateContent`→Gemini。
2. `controller.Relay`（`controller/relay.go:73`）：按格式解析 body 成 DTO → `GenRelayInfo` 构造 RelayInfo（记录**客户端协议**）→ 计费预扣 → **重试循环**：`getChannel()` 选渠道 → 按格式分派 Helper → 失败 `shouldRetry()` 换渠道。
3. 渠道 channel type 经 `ChannelType2APIType`（`common/api_type.go:5`）映射为 `ApiType` —— **ApiType 决定目标上游协议**。
4. Helper 主流程（三种格式同构，以 `relay/compatible_handler.go:25` 为例）：
   - `common.DeepCopy` 复制请求隔离重试污染（:33）
   - `ModelMappedHelper` 模型名映射（:42）
   - `GetAdaptor(info.ApiType)` 取 adaptor（:70）
   - `adaptor.ConvertOpenAIRequest`（:112）→ marshal → `RemoveDisabledFields`（按渠道删字段）→ `ApplyParamOverride`（渠道参数覆盖）
   - `adaptor.DoRequest`（:153）发 HTTP
   - 非 200 → `service.RelayErrorHandler`；`adaptor.DoResponse` 处理响应返回 usage → 结算
5. 透传模式：`PassThroughRequestEnabled` 时跳过转换直接重放原始 body（compatible_handler.go:100-110）。

Adaptor 接口（`relay/channel/adapter.go:17-34`）要求每个上游实现四种客户端格式的请求转换入口（`ConvertOpenAIRequest`/`ConvertOpenAIResponsesRequest`/`ConvertClaudeRequest`/`ConvertGeminiRequest`）+ `DoRequest`/`DoResponse`。

### 2.2 注册表转换引擎（relaykit/relayconvert）——最值得借鉴的部分

`RelayFormat` 定义：`relaykit/types/relay_format.go:3-20`。

**请求侧**（`request_registry.go`）：
- `RequestConverterSpec{ID, From, To, Quality, Convert, StepConverters}`（:48-55）。`StepConverters` 声明多跳链。
- 注册时严格校验：route 唯一、step 链首尾衔接，冲突直接 panic（:114-135）。
- 入口 `ConvertRequest`（:155）：按 Go 类型推断源格式 → 查路由 → `executeRequestSteps`（:242-305）执行多跳。**转换前先用 `toolconv.ExtractRequest` 把工具定义剥离成工具 IR，逐跳转换 messages，最后 `toolconv.AttachRequest` 按目标格式重编码**——工具系统因此只需"格式↔IR"两套编解码，不是 N²。
- 每次转换产出 `RequestResult{Value, Diagnostics}`，Diagnostics 记录有损告警。

**响应侧**（`response_registry.go`）：
- `ResponseConverterSpec`（:58-69）多三个流式钩子：`ConvertStream`（无状态）、`NewStreamState`+`ConvertStreamChunk`（有状态逐 chunk）、`FinalizeStream`（流尾冲刷）。
- `ResponseStreamState`（:95-109）持有逐步 step 状态、累计 usage、去重 diagnostics；多跳流式时一个 chunk 依次流过各 step（:607-631）。

**内置路由表**（`text_converter_registry.go:49-252`）11 条路由：

| From → To | 质量 | 方式 |
|---|---|---|
| Claude ↔ OpenAI chat | fair | 直转 |
| Gemini ↔ OpenAI chat | fair | 直转 |
| OpenAI chat ↔ OpenAI responses | good | 直转 |
| Claude ↔ Gemini | discouraged | 多跳经 OpenAI |
| Claude ↔ OpenAI responses | fair | 部分直转部分多跳 |
| Gemini ↔ OpenAI responses | fair | 多跳 |

**OpenAI Chat Completions 是事实上的枢纽**。写新上游只需实现"你的格式 ↔ OpenAI chat"两个方向即可接入全部格式对；代价是 Claude↔Gemini 被标注 `discouraged`（两跳有损）。

### 2.3 请求转换细节（核心转换对）

**Anthropic → OpenAI Chat**（`relaykit/relayconvert/internal/claude_messages/to_oai_chat_req.go:26-231`）：
- 标量直拷；`stop_sequences` 长度 1 写字符串、>1 写数组（OpenAI 两种形态都接受）。
- thinking → 统一 `reasoning.Intent`，按目标方言编码（OpenRouter 写 `reasoning{enabled,effort,...}` JSON，否则 `reasoning_effort`）。
- tools：`{name, description, input_schema}` → `{type:"function", function:{... parameters: input_schema}}`。
- system：字符串 → system 消息；数组默认拼成单字符串；OpenRouter + `anthropic/claude` 前缀时保留数组以携带 `cache_control`。
- messages：`text`→text content；`image`→`image_url`（data URI）；`tool_use`→`tool_calls`（input marshal 成 arguments 字符串）；`tool_result`→独立 `role:"tool"` 消息（名称缺失时从上下文反查）；空消息丢弃。

**OpenAI → Anthropic**（`oai_chat/to_claude_messages_req.go:17`）：
- `web_search_options` 会**合成 Anthropic 服务端工具 `web_search_20250305`**（:32-65）。
- MaxTokens 缺失兜底返回 `ErrMissingMaxTokens`（Claude 必填）。
- **消息规整**：`developer`→`system`；相邻同 role 合并（:158-163）；空 content 填 `"..."`；所有 system 聚合成顶层数组；**首条必须 user，否则插占位**（:206-211）；`tool` 消息并入前一条 user 的 `tool_result` block；URL 图片下载转 base64。
- assistant `tool_calls` → `tool_use` block（arguments 反序列化成 input 对象）。

**Gemini → OpenAI**（`gemini_chat/to_oai_chat_req.go:14-195`）：
- role：`model→assistant`、`function→function`；`thought:true` 的 part 聚合成 `reasoning_content`。
- **function call ID 合成**：Gemini 无 tool call ID，`geminiFunctionCallHistory`（:206-277）预扫已有 ID 后生成 `call_N`，按"名称匹配 + 同名称按顺序"把 functionResponse 关联回去。
- `stopSequences` **截断到 4 条**（OpenAI 限制）。

### 2.4 响应/流式转换细节

**非流式**：
- Claude→OpenAI：`ResponseClaude2OpenAI`（`to_oai_chat_resp.go:218`）——`tool_use`→`tool_calls`、`thinking`→`reasoning_content`、text 拼接 + citations→annotations（含 UTF-8 rune offset 计算）；`stop_reason` 经 `relaykit/reasonmap/` 映射表转换（end_turn→stop、max_tokens→length、tool_use→tool_calls、refusal→content_filter）。
- OpenAI→Claude：`ResponseOpenAI2Claude`（`to_claude_messages_resp.go:449`）——反向同理。

**流式核心难点与处理**（SSE 互转的四座大山）：

1. **block index 重映射**（Claude 流 → OpenAI 流，`to_oai_chat_resp.go:126-207` `ClaudeToChatStreamState`）：Anthropic 的 content block index 全局混编（text/thinking/tool 共享序号），OpenAI tool_calls 的 index 是工具内稠密的，状态里维护 `toolIndexByContentBlock` 映射；`server_tool_use` 等 hosted 工具块静默丢弃。
2. **tool_call 首帧时序**（OpenAI 流 → Claude 流，`to_claude_messages_resp.go:264-324`）：OpenAI 首帧只带 id/name、arguments 后续才到；**id 和 name 齐了才发 `content_block_start`，之前攒的 arguments 存 `PendingArguments` 随后以 `input_json_delta` 补发**；id 缺失合成 `toolu_<uuid>`。
3. **block 开合不变式**：Anthropic 要求每个 block 严格 start→delta*→stop；内容类型切换时先 `stopOpenBlocksAndAdvance`（:137-154）补 stop 再开新 block。
4. **finish_reason 与 usage 时序解耦**（:393-414）：finish_reason 到了但 usage 没到时**不发终止帧**，等 usage-only chunk 到了合并发 `message_delta` + `message_stop`；`FinalizeStreamResponseOpenAI2Claude`（:420-447）兜底：异常断流强制关闭所有打开的 block 并补终止帧（stop_reason 缺省 `end_turn`）。

**通用读流框架**：`relay/helper/stream_scanner.go:77` 三 goroutine（scanner 读行 / data handler / ping 保活），区分 `[DONE]`/EOF/超时/客户端断开并记入 `info.StreamStatus.EndReason`；**客户端断开立即关上游 body 停止消耗 token**（:298-301）。OpenAI 流侧保留倒数第二帧（`secondLastStreamData`）应对部分网关把完整 usage 放倒数第二事件。

**Gemini→Claude 流是两段串联**：Gemini→OpenAI chunk → OpenAI→Claude 管线（`relay-gemini.go:146-156`）——多跳在流侧同样成立。

### 2.5 usage 统计

- 统一 `dto.Usage`（`relaykit/dto/openai_response.go:228`），含 cache 5m/1h 拆分、`UsageSemantic`（"anthropic"/"openai" 标记口径）、`BillingUsage`。
- **口径换算**：Claude→OpenAI 时把 prompt 重算为 input+cache_read+cache_write 的**总输入口径**（`UsageFromClaudeAPIUsage`，to_oai_chat_resp.go:291-353）；Gemini 的 ThoughtsTokenCount 计入 completion。
- 上游没给 usage 时本地 tokenizer 估算补全，估算值标记 `Estimated:true`。
- 合并语义 `MergeUsageNonZero`：后到的非零字段覆盖、零不擦除。

### 2.6 错误处理（三段式）

1. **万能解析**：`dto.GeneralErrorResponse`（`relaykit/dto/error.go:23-39`）同时覆盖 `error`(object/string)、`message`、`msg`、`err`、`error_msg`、`header.message` 等各家错误外形。
2. **内部统一**：`types.NewAPIError`（`relaykit/types/error.go:90-99`）持有原始错误对象、errorType、StatusCode、skipRetry。
3. **出口渲染**：`controller/relay.go:94-112` 的 defer 按客户端协议输出——Claude 客户端收 `{"type":"error","error":{...}}`（`ToClaudeError`），其余收 `{"error":{...}}`（`ToOpenAIError`），均做 API key 脱敏。
4. 渠道可配 `status_code_mapping` 改写状态码（`service/error.go:140-163`）。

### 2.7 模型名映射

渠道级 `model_mapping` JSON（`relay/helper/model_mapped.go:14-69`）：**支持链式重定向（A→B→C 用链尾）带环检测**；未命中回退去 thinking 后缀的 base 名再查。`OriginModelName` 与 `UpstreamModelName` 全程分离。换渠道重试时重新走一遍映射。

---

## 第三部分：sub2api（Go，Responses 枢纽 + 直转桥 + 身份伪装）

### 3.1 代码分布

**（A）纯协议 DTO 互转层**（无状态/状态机，可复用）—— `backend/internal/pkg/apicompat/`：

| 文件 | 内容 |
|---|---|
| `types.go` | 三大协议全部 DTO（约 850 行） |
| `anthropic_to_responses.go` | Anthropic 请求 → Responses 请求 |
| `responses_to_anthropic_request.go` | Responses 请求 → Anthropic 请求 |
| `responses_to_anthropic.go` | Responses 响应/流 → Anthropic 响应/流（含状态机） |
| `anthropic_to_responses_response.go` | 反向（含状态机） |
| `chatcompletions_to_responses.go` / `responses_to_chatcompletions.go` | CC ↔ Responses（含 `BufferedResponseAccumulator`） |
| `chatcompletions_anthropic_bridge.go` | **Anthropic ↔ CC 直转桥**（跳过 Responses 中间层） |

**（B）网关编排层**（选号、模型映射、伪装、重试、usage）—— `backend/internal/service/`：每个"入口协议 × 上游平台"组合一个 `Forward*` 方法。

### 3.2 完整链路骨架（所有 Forward 路径同构）

以 `gateway_forward_as_responses.go:31` 为典型：

1. 入口 body 归一化 → 解析入口 DTO
2. **协议转换**（如 `ResponsesToAnthropicRequest` :63）
3. **强制上游 `stream=true`**（:69）——上游永远流式，客户端要 JSON 就在网关侧缓冲聚合
4. 模型映射（三级，见 §3.9）
5. Claude Code OAuth 伪装（见 §3.5）
6. 取凭证 + 代理 → 构造上游请求（UA、beta 头、身份头）→ `DoWithTLS`（TLS 指纹伪装）
7. 错误处理与 failover
8. 响应转换：客户端 stream → 逐事件流式转换；客户端非流式 → 缓冲上游 SSE 组装完整 JSON，**显式把 Content-Type 改回 `application/json`**（:457，注释说明了被 SSE Content-Type 污染的坑）

### 3.3 入口 × 上游转换矩阵

分派取决于三维：API Key 分组平台 → 账号平台/类型 → **上游能力探测**。

| 入口 | 上游 | 转换链 |
|---|---|---|
| /v1/messages | Anthropic 原生 | **零转换透传**（仅模型映射 + body 清洗 + OAuth 伪装） |
| /v1/messages | OpenAI Responses | Anthropic → Responses |
| /v1/messages | OpenAI CC | **直转** Anthropic → CC |
| /v1/messages | Gemini | Claude → generateContent（手写 map，约 3800 行文件） |
| /v1/responses | Anthropic | Responses → Anthropic |
| /v1/responses | OpenAI CC | Responses → CC |
| /v1/chat/completions | Anthropic | CC → Responses → Anthropic（**两跳链式**） |
| /v1/chat/completions | Gemini | CC → Responses → Anthropic → Gemini（**三跳链式**） |

**上游能力探测**（`pkg/openai_compat/upstream_capability.go`）：账号 `extra.openai_responses_supported` 探测落标；`ShouldUseResponsesAPI`（:113）缺失默认 true；运行时 `/v1/responses` 返回 404 类错误当场降级 CC 直转。

### 3.4 请求转换要点

**Anthropic → Responses**（`anthropic_to_responses.go:13`）：
- system → `role=developer` 的 message item；Claude Code 计费归因块（`x-anthropic-billing-header` 前缀）识别并过滤（:152-175）。
- `tool_result` 内嵌图片被提取成单独 user message（Responses 的 `function_call_output` 不能挂图片）（:216-244）。
- `thinking` 块（带 signature）→ `reasoning` item 的 `encrypted_content`；`gAAAA` 前缀（Google 系签名）跳过（:285-299）。
- `max_tokens`→`max_output_tokens`（下限 128）；`thinking.effort=max`→`reasoning.effort=xhigh`；gpt-5 系剔除 `temperature`/`top_p`。
- 要求 `include:["reasoning.encrypted_content"]` 以便回程还原签名。

**Responses → Anthropic**（`responses_to_anthropic_request.go:13`）：
- **`reasoning` item 直接丢弃**（:167-173）——无法伪造 Anthropic 加密 signature，单向有损。
- 结构修复：`normalizeAnthropicToolPairing`（:311）修复 tool_use/tool_result 邻接不变式；`mergeConsecutiveMessages`（:576）保证角色交替；空内容消息丢弃防 400。
- effort≠low 时自动启用 thinking + 默认 budget（:55-65）。

**CC → Responses**（`chatcompletions_to_responses.go:18`）：assistant 的 `reasoning_content` 包成 `<thinking>...</thinking>` 文本放回 content（:168-170）——CC 明文推理无法变成 Responses reasoning item；强制 `stream=true`、`store=false`、`include=[reasoning.encrypted_content]`（:29-47）。

**Anthropic ↔ CC 直转桥**（`chatcompletions_anthropic_bridge.go`）：thinking 块 → `reasoning_content`，**但仅在该 assistant 消息带 tool_calls 时才保留**（:292,313，DeepSeek 约束）；`web_search` 工具丢弃。

### 3.5 Claude Code 客户端兼容（mimicry）——本仓库最有特色的部分

**识别真实 Claude Code**（`service/claude_code_validator.go`）：UA 正则 `claude-cli/x.x.x` + 官方 system prompt 模板用 **Dice 系数（bigram 相似度）阈值 0.5** 匹配（:308）+ 计费归因块识别 + X-App/anthropic-beta 头 + `metadata.user_id` 格式。

**伪装成 Claude Code**（OAuth 账号 + 非 Claude Code 入口时必须，否则 Anthropic 判第三方扣 extra usage）：
- 触发条件：`account.IsOAuth() && !isClaudeCode`（`gateway_forward.go:196`）
- `applyClaudeCodeOAuthMimicryToBody`：system prompt 重写注入官方 CLI system 块 + 工具名混淆 + metadata.user_id 注入
- **thinking 签名 400 两阶段降级重试**（`gateway_forward.go:400-490`）：上游拒绝签名时剥离重试

**设计要点：身份伪装与协议转换正交**——mimicry / Codex instructions 模板 / 身份头恢复都封装成独立 body 变换函数，插在"协议转换完成、发送之前"。

### 3.6 Gemini 方向（hub 外的手写转换）

`convertClaudeMessagesToGeminiGenerateContent`（`gemini_messages_compat_service.go:3260`）：system→`systemInstruction`；`tool_use`→`functionCall`+`thoughtSignature`；`tool_result`→`functionResponse`；**缺失签名的 functionCall 注入 dummy 签名 `"skip_thought_signature_validator"`**（:43-44，Gemini 3 强制要求）；`cleanToolSchema`（:3610）清理 Gemini 不支持的 schema 字段。三种上游形态：AI Studio APIKey(:626) / Code Assist v1internal wrapped request(:679) / Vertex(:734)。

### 3.7 响应转换

**非流式统一模式：上游强制流式 → 网关缓冲聚合 → 转目标协议 JSON**。缓冲时逐事件扫描：`input_json_delta` 用 `appendRawJSON` 拼 JSON 片段（特判 content_block_start 自带的 `{}` 占位）；Responses terminal 事件 `output` 为空时用 `BufferedResponseAccumulator.SupplementResponseOutput` 从累积 delta 重建。

**流式：每方向一个 State + Events + Finalize 三件套**。两张核心事件映射表：

Responses 流 → Anthropic 流（`responses_to_anthropic.go:175-254`）：

| Responses 事件 | Anthropic 事件 |
|---|---|
| `response.created` | `message_start` |
| `output_item.added`(function_call/reasoning/message) | `content_block_start`（tool_use/thinking/text） |
| `output_text.delta` | `content_block_delta`(text_delta) |
| `function_call_arguments.delta` | `content_block_delta`(input_json_delta) |
| `reasoning_summary_text.delta` | `content_block_delta`(thinking_delta) |
| 关闭 thinking 块前 | 先发 `signature_delta`（携带 encrypted_content）（:688） |
| `response.completed` | `message_delta` + `message_stop` |

Anthropic 流 → Responses 流（`anthropic_to_responses_response.go:140-197`）：基本镜像；`signature_delta` 事件**丢弃**（签名已在 output_item.done 的 reasoning item 里）；`message_stop`→`response.completed`（携带累积完整 Outputs 供 SDK `get_final_response`）。

**双状态机串联**案例：CC 入口 → Anthropic 上游的流式（`gateway_forward_as_chat_completions.go:354`）：`AnthropicEventToResponsesEvents` → `ResponsesEventToChatChunks`，一个 Anthropic 事件经两个状态机变成 CC chunk。

**CC chunk → Anthropic 流直转桥**（`chatcompletions_anthropic_bridge.go:557-678`）：tool_calls 按 index **延迟宣告**——name 未到的 chunk 先缓存参数（:793-848），空参数补 `"{}"`（:921）；Finalize 发 message_delta + message_stop。

### 3.8 账号池与协议转换的交互（签名字段的账号绑定）

这是 sub2api 相比 new-api 独有的复杂度——**thinking.signature / encrypted_content / thoughtSignature 是账号绑定的加密字段**：

- **粘性会话**：sessionHash 让同会话落同账号，否则换账号后上游无法解密历史中的加密 reasoning。
- **换账号解密失败处理**：Grok 命中 `isGrokInvalidEncryptedContentResponse` 时，剥离请求历史中的 thinking 签名（`stripAnthropicThinkingSignatures`，`openai_gateway_grok.go:436`）整体重试一次；出站方向同样有 strip+retry 循环——出站 body 仍带 `reasoning.encrypted_content` 时任何 400 先剥离重试再视为硬失败。
- **明文 reasoning 缓存自愈**：Responses→CC 桥接时客户端可能只回传 encrypted-only reasoning item，网关按 item id 把明文存缓存（TTL 7 天），回程回注 `reasoning_content`；失败 fail-open；请求历史里带明文的 item 顺手回写缓存自愈 Redis 漂移。
- **流式中途失败不可换号**：一旦已向客户端写出语义内容，failover 会把两个模型的流拼接在一起，所以显式判断 `!clientOutputStarted && shouldFailover` 才允许换号，否则转成流内 error 事件发给客户端（`openai_gateway_messages.go:1046-1083`）。
- **会话续传**：`prompt_cache_key` + `session_id` 头维持上游 prompt cache；session_id 由 `apiKeyID + 账号身份 + promptCacheKey` 派生 UUID，隔离不同 API key 的上游会话；`previous_response_id` 失效自动降级重试。

### 3.9 错误格式转换与模型映射

- 每种客户端协议各写一个错误函数：`writeAnthropicError`（`{"type":"error","error":{...}}`）/ `writeChatCompletionsError`（`{"error":{...}}`）/ `writeResponsesError`（`{"error":{"code":...}}`）+ 流内错误 SSE。
- **错误透传规则服务**（`error_passthrough_service.go`）：按关键词/平台/错误码规则决定是否透传上游错误原文（8KB body 匹配窗口）。
- **cyber_policy 特例**：OpenAI 风控拦截（流内 `response.failed`）不可重试不 failover，按当前客户端协议写错误 + 记 tokens=0 免费用量行。
- 模型名三级映射：账号级显式映射（通配符、最长优先，`account.go:865`）→ 协议级归一化（OAuth 短名↔带日期长 ID，`claude/constants.go:231`）→ 上游级适配；**响应回程把模型名改回客户端请求的原名**，并记录上游真实返回模型名检测冲突。

---

## 第四部分：kiro-gateway（Python，真 IR + 单上游二进制协议）

### 4.1 架构

FastAPI + httpx + Pydantic v2（DTO 全部 `extra:"allow"` 宽容透传未知字段）。分层：

- 路由：`routes_openai.py`（`/v1/chat/completions`）、`routes_anthropic.py`（`/v1/messages`）
- 适配器：`converters_openai.py` / `converters_anthropic.py`（各自 API → **统一 IR**）
- 核心：`converters_core.py`（IR → Kiro payload）、`streaming_core.py` + `parsers.py`（AWS 事件流 → 统一 `KiroEvent`）
- 出口：`streaming_openai.py` / `streaming_anthropic.py`（`KiroEvent` → OpenAI SSE / Anthropic SSE）

统一 IR（`converters_core.py`）：`UnifiedMessage{role, content, tool_calls, tool_results, images}`（:83）、`UnifiedTool`（:106）、`ThinkingConfig{enabled, budget_tokens}`（:54）。**注意：上游 CodeWhisperer 协议没有 DTO，payload 全部手工拼 dict**——省一层映射但牺牲类型安全。

主链路：校验 API key → 截断恢复注入 → failover 循环（选账号 → `build_kiro_payload` → POST）→ 流式逐事件转换 / 非流式聚合。**无论客户端是否要求 stream，对上游永远流式请求**。

### 4.2 上游协议本质（CodeWhisperer/Kiro）

- 端点：`POST https://runtime.{region}.kiro.dev/generateAssistantResponse`
- 认证：**Bearer token 即可，无需 SigV4**，但必须伪装 Kiro IDE 的 aws-sdk-js User-Agent + 机器指纹（`utils.py:61-89`）。头含 `x-amz-target: AmazonCodeWhispererStreamingService.GenerateAssistantResponse`、`Content-Type: application/x-amz-json-1.0`。
- 请求体核心（`converters_core.py:1568-1585`）：

```json
{
  "conversationState": {
    "chatTriggerType": "MANUAL",
    "conversationId": "<uuid>",
    "currentMessage": {"userInputMessage": {"content": "...", "modelId": "...", "origin": "AI_EDITOR",
      "images": [{"format": "png", "source": {"bytes": "<base64>"}}],
      "userInputMessageContext": {
        "tools": [{"toolSpecification": {"name": "...", "description": "...", "inputSchema": {"json": {}}}}],
        "toolResults": [{"content": [{"text": "..."}], "status": "success", "toolUseId": "..."}]
      }}},
    "history": [
      {"userInputMessage": {}},
      {"assistantResponseMessage": {"content": "...", "toolUses": [{"name": "...", "input": {}, "toolUseId": "..."}]}}
    ]
  },
  "profileArn": "arn:aws:codewhisperer:..."
}
```

要点：**最后一条消息放 currentMessage，其余进 history**；工具定义挂当前消息的 context（不是顶层）；**没有独立 system 字段，system 只能内联进首条 user 消息文本**；payload 超 600KB 自动裁剪 history（成对弹最老消息、对齐 user 开头、修复孤儿 toolResults，`payload_guards.py:121-163`）。

- 响应：**AWS event-stream 二进制帧流**（prelude 12 字节 + headers + payload + CRC32）。事件载荷类型：`{"content"}` 文本增量 / `{"name","toolUseId","input"}` tool 开始 / `{"input"}` 分片续传 / `{"stop":true}` tool 结束 / `{"usage"}` credits / `{"contextUsagePercentage"}` / `{"followupPrompt"}`。

**该项目并未真正实现帧解析器**，而是利用"JSON payload 是 UTF-8 文本嵌在二进制帧里"做文本扫描（`parsers.py:211-569`）：`decode('utf-8', errors='ignore')` 丢弃帧头 → 前缀模式表找 JSON 起点 → 括号配平找边界 → json.loads 分发。**能用但脆弱**（content 字符串以 `{"name":` 开头会误判）；Go 实现建议按帧格式正经解析。

### 4.3 请求转换：消息规整流水线（兼容性关键）

`converters_core.py:1435-1495` 按顺序执行 12 步规整，这是把"任意客户端历史"塞进"挑剔上游"的工程核心：

1. 长 description 工具降级（>10000 字符挪进 system prompt 的 `# Tool Documentation` 段，:493-557）
2. 工具名 >64 字符 → 400（:560-599）
3. system prompt 追加"thinking 标签合法化说明"
4. **无 tools 时剥离所有 tool 痕迹**——tool_calls/tool_results 渲染成 `[Tool: name (id)]` 文本（:911-992；Kiro 拒绝"有 toolResults 但没 tools"）
5. 孤儿 tool_results 修复（前面没有带 tool_calls 的 assistant → 降级文本，:995-1068）
6. 相邻同 role 合并（:1071-1152）
7. 首条必须 user，否则前置占位（:1155-1201）
8. 未知 role 归一为 user（:1204-1256）
9. 强制 user/assistant 交替（连续 user 间插占位 assistant，:1259-1313）
10. system prompt 拼到第一条 user 消息 content 前
11. 空 content 补 `"(empty placeholder)"`
12. JSON Schema 清洗：递归删空 `required:[]` 和所有 `additionalProperties`（否则 Kiro 400，:439-486）

对比：new-api 和 sub2api 只做其中 3-4 步（合并同 role、首条 user、空内容占位、tool 邻接修复），kiro-gateway 因为上游最挑剔所以最全。**这份清单可以直接作为 relayd 消息规整的检查表。**

### 4.4 响应转换

**统一事件流**：`parse_kiro_stream`（`streaming_core.py:118-227`）两种输出格式共用——`asyncio.wait_for` 等首字节（15s 超时，超时整请求重来最多 3 次，对用户透明）；chunk 经 `AwsEventStreamParser.feed` → 统一 `KiroEvent{type ∈ content/thinking/tool_use/usage/context_usage/error}`；content 先过 `ThinkingParser` 剥出伪 thinking 段；**tool use 在流末尾一次性产出**（解析器要攒齐 input 分片）。

**→ OpenAI SSE**（`streaming_openai.py:72-447`）：
- content → `delta.content`（首 chunk 带 `role:"assistant"`）；thinking 按配置放 `delta.reasoning_content` 或 content。
- 流尾对累积全文跑 `parse_bracket_tool_calls`（识别 `[Called fn with args: {...}]` 文本型工具调用，`parsers.py:92-148`），与流内 tool calls 合并去重（按 id/名称+参数两级）。
- finish_reason 判定：截断 → `length`、有 tool calls → `tool_calls`、否则 `stop`（:271-302）。
- usage 最后一个 chunk 发出；结尾 `data: [DONE]`。
- 非流式 `collect_stream_response`（:576-688）：**复用流式生成器，解析自己产出的 SSE 文本**聚合出标准 `chat.completion`。

**→ Anthropic SSE**（`streaming_anthropic.py:129-718`），严格按 Messages streaming 规范：
1. `message_start`：`usage.input_tokens` 用**请求侧 tiktoken 估算**（上游准确值流尾才来，自承精度 ~85-90%）。
2. thinking block：`content_block_start{type:"thinking", signature: <伪造 sig_xxx>}` → `thinking_delta` → 遇正文/tool 时 `content_block_stop`。
3. text block：start → `text_delta`。
4. tool_use block（流尾一次性）：完整 `content_block_start{type:"tool_use", id, name, input:{}}` → 单个 `input_json_delta`（partial_json 是完整 JSON）→ `content_block_stop`；id 缺省生成 `toolu_<hex24>`。
5. `message_delta`：stop_reason（截断→max_tokens、有 tool→tool_use、否则 end_turn）+ output_tokens（tiktoken 数）；input_tokens 用上游 context_usage 修正。
6. `message_stop`；中途异常发 `event: error`。
7. `web_search` 拦截：完整模拟 `server_tool_use` + `web_search_tool_result` + 文本摘要的 block 序列（:355-468）。
- Anthropic block 状态机（:186-192）：`current_block_index` + 各类 block 的 started/index 标志，异类内容先 stop 旧 block 再 start 新 block，流尾统一关未闭 block。

### 4.5 usage 填写策略

| 字段 | OpenAI | Anthropic | 来源 |
|---|---|---|---|
| prompt/input tokens | 最后 chunk 的 `usage.prompt_tokens` | `message_start`（估算）+ 流尾修正 | 优先 `contextUsagePercentage × maxInputTokens / 100 − output`（`streaming_core.py:337-362`），回退 tiktoken |
| completion/output tokens | tiktoken × 1.15 数全文 | 同左 | `kiro/tokenizer.py:45,77-106` |
| credits | `usage.credits_used` 扩展字段 | 不透出 | 上游 `{"usage": N}` 事件 |

**要点：上游只给 credits 和上下文百分比，token 数全是估算的。**

### 4.6 伪 thinking（注入 + 剥离的闭环）

上游协议没有 thinking 概念，网关自己造了一个：

1. **注入侧**：最后一条 user 消息前注入 `<thinking_mode>enabled</thinking_mode><max_thinking_length>N</max_thinking_length><thinking_instruction>…` 标签（`converters_core.py:361-432`），并在 system prompt 里声明这些标签"不是注入攻击"（:304-331）。
2. **剥离侧**：`ThinkingParser`（`thinking_parser.py:79-380`）状态机识别 `<thinking>/<think>/<reasoning>/<thought>` 开标签，`_could_be_tag_prefix` + 初始 20 字节缓冲处理标签跨 chunk 切断；输出模式四选一：`as_reasoning_content` / `remove` / `pass` / `strip_tags`。
3. `reasoning_effort` → 预算映射：minimal 10% / low 20% / medium 50% / high 80% / xhigh 95% 的 max_tokens（`converters_openai.py:300-386`）。

### 4.7 错误处理与多账号

- HTTP 层（`http_client.py:170-342`）：403 → 强制刷新 token 重试；429/5xx → 指数退避（1s/2s/4s）；重试耗尽后把最后响应原样上交分类。流式请求加 `Connection: close` 防 CLOSE_WAIT 泄漏。
- Kiro 错误语义（`kiro_errors.py:63-138`）：解析 `{"message","reason"}`，已知 reason 映射友好文案（上下文超长 / 月度配额 / 模型不可用 / payload 畸形）。
- failover 分类（`account_errors.py:49-134`）：RECOVERABLE（402/403/429/400+INVALID_MODEL_ID → 换账号）vs FATAL（其余 400/422/5xx → 直接回客户端）。
- 多账号：全局 sticky 索引环形遍历；**熔断** `failures>0` 冷却 `60s × 2^(failures-1)`（上限 1 天），冷却期内 10% 概率试探；冷却过期进 Half-Open；`report_success` 清零；状态 `state.json` 每 10s 脏检查 + tmp+rename 原子写。
- 模型名：5 组正则归一化客户端写法为 Kiro 点号格式（`model_resolver.py:87-189`）；解析管线：别名 → 归一化 → 动态缓存 → 隐藏表 → **pass-through**（"gateway, not gatekeeper"，未知模型原样发给上游让它自己拒绝）。

---

## 第五部分：横向对比与对 relayd 的借鉴清单

### 5.1 核心设计决策对比

| 决策点 | new-api | sub2api | kiro-gateway | relayd 现状 |
|---|---|---|---|---|
| 枢纽格式 | OpenAI Chat | OpenAI Responses | 真 IR | OpenAI ↔ Anthropic 两协议直转（relaykit） |
| 非流式实现 | 上游原生非流式 | 网关聚合上游流 | 网关聚合（甚至复用自家 SSE 再解析） | 已修复：非流式走 `ConvertResponseBody` |
| 工具 IR | 有（toolconv Extract/Attach） | 无 | 并入消息 IR | 无 |
| 消息规整 | 4 步 | 3-4 步 | 12 步流水线 | 少量 |
| 流式状态机 | 有，Finalize 兜底 | 有，Finalize 兜底 | block 状态机 + 流尾关块 | 基础版 |
| 签名字段处理 | 透传为主 | 粘性会话 + 剥离重试 + dummy 注入 + 缓存自愈 | 伪造签名占位 | 未处理 |
| 错误转换 | 三段式 + 脱敏 + 状态码映射 | 每协议一个函数 + 透传规则 | reason 分类 + failover 分级 | 基础版 |

### 5.2 可直接落地的借鉴点（按优先级）

1. **流式状态机四难点的参考实现**（new-api §2.4）：block index 重映射、tool_call 首帧 PendingArguments 缓存、stop 未开 block、finish_reason 与 usage 时序解耦 + Finalize 兜底。relayd 的 StreamConvert 应补齐 Finalize 路径，异常断流时也产出合法的 `message_stop`/`[DONE]`。
2. **"上游永远流式"**（sub2api/kiro-gateway 共识）：统一只有一套流式读取代码，非流式 = 缓冲聚合 + 显式改回 `application/json` Content-Type。relayd 目前流式/非流式两条路径，可考虑收敛。
3. **消息规整检查表**（kiro-gateway §4.3 的 12 步）：相邻同 role 合并、首条必须 user、强制交替、空内容占位、孤儿 tool_result 降级、无 tools 时剥离 tool 痕迹、schema 去 `additionalProperties`、工具名长度上限。relayd 转换后应过一遍规整管线。
4. **usage 口径显式换算**：Anthropic input_tokens 不含 cache、OpenAI 含 cached_tokens、Gemini 含 cachedContentTokenCount——每个转换器入口就地处理；合并用"非零覆盖"语义；估算值打 `Estimated` 标记。
5. **错误三段式**（new-api §2.6）：万能解析 → 内部统一错误（带 skipRetry）→ 按客户端协议渲染 + 脱敏。relayd 的 429/失败事件已有雏形，可补齐出口渲染层。
6. **流式中途失败不可换上游**：一旦已向客户端写出语义内容，failover 只能发流内 error 事件，不能拼流（sub2api §3.8）——这与 relayd 的熔断摘除逻辑直接相关。
7. **模型名双向映射**：请求时映射到上游名、响应时改回客户端原名；`Origin` 与 `Upstream` 两个字段全程分离。
8. **宽容解析**：DTO 用 `json.RawMessage` + 自定义 UnmarshalJSON 容忍第三方字段漂移；只改少数字段时直接字段级改写原始 body（gjson/sjson 式），避免全量重建丢字段。
9. **签名字段按账号绑定对待**（若 relayd 未来支持订阅账号）：粘性会话、换账号剥离签名重试、Gemini dummy 签名注入。
10. **若接入 Kiro/CodeWhisperer 类上游**：Bearer 即可无需 SigV4；event-stream 要按帧格式正经解析（prelude+headers+payload+CRC32），不要学 kiro-gateway 的文本扫描法；伪 thinking 的"注入标签 + 出口剥离"闭环可直接照搬。

### 5.3 不建议照搬的

- kiro-gateway 的"文本扫描解析二进制帧"（脆弱，content 以 `{"name":` 开头会误判）。
- kiro-gateway 上游侧不建 DTO 全拼 dict（Go 里应建类型）。
- sub2api 的三跳链式转换（CC→Responses→Anthropic→Gemini）损耗大，只在没有直转实现时兜底。
- new-api 的 Claude↔Gemini 多跳（官方自己标注 `discouraged`）。

---

附：三份逐文件调研底稿见同目录 `.research-new-api.md`、`.research-sub2api.md`、`.research-kiro-gateway.md`（如需更细的行号级细节可查）。
