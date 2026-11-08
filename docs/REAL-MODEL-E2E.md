# 真实模型 E2E（Real-model end-to-end）

> 目的：证明 keel 的 loop 能用**真实模型**跑完一次完整回合——
> `input → 模型 → 工具往返（echo）→ 终答`——并给出可复跑脚本、事件级断言与失败路径证据。
>
> 本文只描述既有装配（`cmd/runtime` → `servicehost` → `provider`），不引入第二套配置词汇。
> 所有结论以代码为准，引用处标注 `path:line`。

## 1. 配置路径（代码为准）

真实 provider 的选择与凭据全部在 **runtime 进程启动时**由环境变量决定：

| 环境变量 | 进入的字段 | 消费者 |
|---|---|---|
| `KEEL_MODEL` | `host.AgentConfig.Model` | `provider.ResolveProvider`；空 = fake provider |
| `KEEL_BASE_URL` | `host.AgentConfig.BaseURL` | OpenAI 兼容 / Anthropic 的端点前缀 |
| `KEEL_PROTOCOL` | `host.AgentConfig.Protocol` | `openai`（含 `openai/chat`）、`openai/resp_api`、`anthropic` |
| `KEEL_PROVIDER` | `host.AgentConfig.Provider` | 内置 provider 注册表名（可选，显式 provider 优先于 protocol） |
| `KEEL_API_KEY` | `servicehost.LLMOptions.APIKey` | 每次模型请求的 `StreamConfig.APIKey` |

调用链：

1. `cmd/runtime/main.go:338-355` 把上面五个变量装进默认租户文档
   （`host.TenantConfig.Agent`），`cmd/runtime/main.go:384` 把 `KEEL_API_KEY`
   与 `os.LookupEnv` 交给 `servicehost.Boot`。
2. `internal/servicehost/services.go:112-122`：`Model` 为空 → `agent.FakeStream()`；
   非空 → `provider.ResolveProvider(model, baseURL, protocol, provider, lookup)`。
3. `internal/provider/resolve.go:50-83` 的解析顺序：
   显式 `provider` → 显式 `protocol` → 预设目录 → 模型名推断 → OpenRouter 兜底。
   - `protocol=openai` **必须**有 `baseURL`，构造 OpenAI Chat Completions 驱动；
   - `protocol=anthropic` 可省略 `baseURL`（默认 `https://api.anthropic.com/v1`）。
4. 请求线路：`internal/provider/providers.go:99` 把
   `${KEEL_BASE_URL}/chat/completions` 作为 URL，`:104` 设置
   `Authorization: Bearer <KEEL_API_KEY>`；未配置 key 时在发请求前拒绝
   （`providers.go:85-87`，错误只提变量名，不提值）。
5. `internal/agent/runner.go:228` 把 `APIKey` 放进每次 loop 的 `LoopConfig`，
   随 `StreamConfig.APIKey` 到达 provider；key 不进入事件、日志或租户文档。

### 租户文档形状

`internal/host/config.go:54-69` 是唯一配置文档（`PUT /v1/tenants/{id}/config` 写入）：

```json
{
  "agent": {
    "provider": "openai",
    "model": "deepseek-v4.1-flash",
    "baseUrl": "https://api.a6api.com/v1",
    "protocol": "openai",
    "apiKeyRef": "env:A6API_KEY"
  }
}
```

- `apiKeyRef` 只接受引用形态 `${NAME}` / `env:NAME` / `secret:NAME`
  （`config.go:509-513`、`:640-648`），明文密钥（含 `Bearer …`）在写入边界被拒绝。
- **当前分支的实际行为**：租户文档里的 `agent.*` 会被解析/校验/持久化，但运行中的
  llm 服务来自**启动时的默认文档**；`servicehost.Runtime.Reload` 在本分支没有 HTTP
  调用方（`grep -rn "Reload(" internal/httpapi cmd/runtime` 为空）。因此换模型 =
  重启 runtime 进程；改动已有租户的模型字段不会影响正在跑的进程。这是既有装配的
  边界，不是 E2E 脚本的假设。

## 2. 最小配置示例（不含密钥）

A6API（OpenAI 兼容网关，项目测试期默认模型来源）：

```bash
export KEEL_MODEL=deepseek-v4.1-flash
export KEEL_PROTOCOL=openai
export KEEL_BASE_URL=https://api.a6api.com/v1     # 注意：要带 /v1，驱动会拼 /chat/completions
export KEEL_API_KEY=...                           # 只放环境，禁止入库
```

Anthropic（第二路 provider，满足 loop-complete 规格的“至少两路”）：

```bash
export KEEL_MODEL=<anthropic-model-id>
export KEEL_PROTOCOL=anthropic
# KEEL_BASE_URL 可省略 → https://api.anthropic.com/v1
export KEEL_API_KEY=...
```

如果已有项目内约定变量，脚本按下表映射（以环境里已有的值优先）：

| 项目既有变量 | keel 变量 |
|---|---|
| `AI_MODEL` | `KEEL_MODEL` |
| `A6API_BASE_URL` / `S2API_BASE_URL` | `KEEL_BASE_URL`（S2API 缺 `/v1` 时脚本补上） |
| `A6API_KEY` / `S2API_KEY` | `KEEL_API_KEY` |

`KEEL_E2E_GATEWAY=a6api|s2api|auto` 选择变量家族；显式 `KEEL_*` 仍然最高优先。
2026-11-06 实测本机到 A6API 的 TLS 握手被重置（§6），可用
`KEEL_E2E_GATEWAY=s2api` 跑同一个 `deepseek-v4.1-flash`。

> 注意：当前分支有一处装配缺陷会让配置的模型名到不了线路（§5）。直连真实网关
> 前先确认该缺陷已修复；未修复时用 `KEEL_E2E_MODEL_REWRITE=1` 打开 harness 绕行。

## 3. 脚本用法

```bash
# 成功回合（默认）
scripts/real-model-e2e.sh

# 只检查配置解析，不连数据库、不发请求（凭据缺失时退出 2）
scripts/real-model-e2e.sh --check

# 失败路径：故意用错误 key，断言 401 可见且密钥不泄露
scripts/real-model-e2e.sh --failure-auth

# 成功回合 + 失败路径一起跑
scripts/real-model-e2e.sh --with-failure
```

脚本行为：

1. 从环境读取凭据；若目标变量为空，才从**候选 env 文件**里按键名提取
   （`KEEL_E2E_ENV_FILE` > `<worktree>/.env.personal` > `<主仓>/.env.personal` >
   `<主仓>/fde2e-omni-v1/.env`）。只逐行匹配已知变量名，**不执行**文件内容，
   也从不回显值；报告只写变量名。网关家族由 `KEEL_E2E_GATEWAY` 选择
   （`auto|a6api|s2api`）：`auto` 优先 A6API 变量、否则 S2API 变量。
2. 起隔离库：默认用 dockerized Postgres（`$KEEL_PG_CTR`，默认 `keel-pg`）里的
   专用库（库名含 `e2e`，`DROP/CREATE ... WITH (FORCE)`，并断言 0 张表）。
   `KEEL_E2E_DSN` 可指向一个已存在的专用空库（库名必须含 e2e/test）。
3. 选一个**空闲端口**（避开 13610–13799 与占用端口）启动 runtime，
   以 `/v1/health` 的 `pid` 核对端口归属，防止连到别人的进程。
4. 建租户/会话，提交一条**显式要求调用 echo** 的输入；轮询
   `GET /v1/sessions/{id}/events` 直到终答，断言事件序列：
   `user.message → assistant.message(stopReason=tool_use, toolCall=echo)
   → tool.result(toolCallId 匹配) → assistant.message(stopReason=end_turn)`；
   同时断言 event seq 从 1 连续、提交回执的 `seq` 就是 user 事件的 seq、
   终答非空、run 状态 `done`。
5. 证据落盘到 `KEEL_E2E_EVIDENCE_DIR`（默认 `/tmp/keel-real-model-<时间戳>/`，
   **不入库**）：成功路径 `runtime.log` / `events.json` / `summary.json` /
   `success-*.json`；失败路径 `failure-runtime.log` / `failure-events.json` /
   `failure-summary.json` / `failure-*.json`；走临时绕行时另有
   `rewrite-proxy.log`。stdout 只打摘要，不打印消息正文与密钥。
6. 退出码：`0` 通过；`1` 断言/运行时失败；`2` blocked（缺凭据或依赖）。

可调环境变量：

| 变量 | 默认 | 作用 |
|---|---|---|
| `KEEL_E2E_PORT` | 自动挑选 | runtime 端口 |
| `KEEL_E2E_DB` | `keel_real_model_e2e_<pid>` | 专用库名（必须含 e2e/test） |
| `KEEL_E2E_DSN` | 空 | 外部专用空库；设置后脚本不建库，只做新鲜度断言 |
| `KEEL_E2E_PG_CTR` | `keel-pg` | dockerized Postgres 容器名 |
| `KEEL_E2E_PG_PORT` | `13741` | 该容器的宿主端口（只连既有基础设施） |
| `KEEL_E2E_GATEWAY` | `auto` | `a6api` / `s2api` / `auto`（显式 `KEEL_*` 仍优先） |
| `KEEL_E2E_MODEL_REWRITE` | 空 | `1` = 启用临时模型名重写代理（见 §5） |
| `KEEL_E2E_BIN` | `/tmp/keel-real-model-runtime` | runtime 二进制；不存在则构建 |
| `KEEL_E2E_TIMEOUT_SEC` | `180` | 等待终答的上限 |
| `KEEL_E2E_PROMPT` | 内置触发 echo 的提示词 | 自定义输入文本 |
| `KEEL_E2E_STRICT_MARKER` | `0` | `1` = 要求工具结果逐字等于标记串 |
| `KEEL_E2E_EVIDENCE_DIR` | `/tmp/keel-real-model.<ts>` | 证据目录 |
| `KEEL_E2E_SKIP_BUILD` | `0` | `1` = 复用 `KEEL_E2E_BIN`，不构建 |

## 4. 断言与证据口径

成功路径的断言全部基于**已提交事件**（不是进程日志）：

- `user.message` 恰好 1 条，seq 等于 input 接口返回的 `seq`；
- 至少 1 条 `assistant.message` 带 `stopReason=tool_use`，且 content 里
  `type=toolCall`、`name=echo`；
- 每个 toolCall **恰好 1 条** `tool.result`，`toolCallId` 相同、`isError=false`、
  内容非空，seq 在调用之后；
- 最后一条 `assistant.message` 为 `stopReason=end_turn`，文本非空，seq 在
  tool.result 之后；
- 事件 seq 从 1 到 N 连续无缺口；
- `GET /v1/sessions/{id}/run` 的 `status=done`；
- 真实密钥不出现在 `events.json` / `runtime.log`（字面量扫描，仅断言不打印）。

失败路径（`--failure-auth`）的断言：

- 输入仍被接受（201），失败在模型回合内可见；
- 事件里出现 `assistant.message` 且 `stopReason=error`，`errorMessage` 含
  upstream 状态码（401/403）或“missing API key”；或 run 状态 `failed` 且
  `lastError` 非空；
- 错误文本里不含密钥字面量；
- **观察**：本分支模型失败走的是“终止 assistant 消息”，
  `LoopEventStream.Result` 不返回 Go error（`internal/loop/loop.go:353-354`），
  因此 run 行可能仍落 `done`。失败的可诊断性靠事件里的
  `stopReason=error` + `errorMessage`；`run.failed/run.finished` 的收口是
  `keel-kernel-loop` K3 的待办，不在本脚本的修改范围。

## 5. 阻断项：配置的 model id 没有到达线路（2026-11-06 实测）

**现象**：`KEEL_MODEL=deepseek-v4.1-flash` 时，runtime 启动日志是
`agent worker started provider=openai model=openai`，出站请求体是
`"model":"openai"`。真实网关对不存在的模型直接失败（A6API 断连，S2API 4xx），
所以修复前「真实模型回合」无法成立。

**证据**：

- 本地 mock 网关记录的请求体：`"model":"openai"`（tools=`clock,echo`、
  `stream=true`、`reasoning_effort=low`、`stream_options.include_usage=true`）；
- 直连真实网关（不打开绕行）时，网关在 3.4s 内明确拒绝：
  `transport: upstream 404: {"error":{"message":"Model \"openai\" is not supported
  by any configured account in this group","type":"model_not_found"}}`；
- harness 模型名重写代理日志（`/tmp/keel-real-model.*/rewrite-proxy.log`）：
  `POST /v1/chat/completions model 'openai' -> 'deepseek-v4.1-flash'`；
- `internal/servicehost/services.go:115-121`：
  `resolved, modelID, err := provider.ResolveProvider(...)`，随后
  `svc.model = modelID`；但 `provider.ResolveProvider` 的第二个返回值在所有分支
  都是 **provider 名**（`internal/provider/resolve.go:70,78,80,99-109,116,121,132,142`；
  `ResolveNamedProvider` 返回 `spec.Name`）。

**最小修复（装配层一行，不在本任务的改动范围）**：

    svc.model = agentCfg.Model   // 或 resolved.Models()[0].ID

**临时绕行**：`KEEL_E2E_MODEL_REWRITE=1` 时，脚本在 runtime 与真实网关之间起一个
本地模型名重写代理（只改 `model` 字段，转发真实 `Authorization` 与流式响应），
证据里保留 `model 'openai' -> '<配置模型>'` 与 `workaround` 字段。绕行只影响
harness，不掩盖缺陷：不打开它时，真实回合在修复前必然失败。

## 6. 实测结果（2026-11-06）

环境：model `deepseek-v4.1-flash`；OpenAI 兼容网关 `https://s2api.top/v1`
（本机到 A6API `api.a6api.com` 的 TLS 握手被重置——直连与本机代理均
`SSL_ERROR_SYSCALL`；S2API 上同一模型可用）；harness 打开
`KEEL_E2E_MODEL_REWRITE=1`；隔离库 + 随机空闲端口。

成功回合（`scripts/real-model-e2e.sh --with-failure`）：

| 项 | 值 |
|---|---|
| 事件序列 | `user.message(seq1) → assistant.message(tool_use, seq2) → tool.result(seq3) → assistant.message(end_turn, seq4)` |
| seq | 1..4 连续 |
| 工具往返 | `echo`，toolCallId 匹配，`isError=false`，结果文本 25 字节，标记串原样回显 |
| 模型调用 | 2 次（工具轮 + 终答轮），`rewrite-proxy.log` 两条 `status=200` |
| 耗时 | 9.2s / 11.9s / 12.2s / 13.1s / 24.3s（五次独立运行；最终一次工具里程碑 7.5s、终答 12.2s） |
| run 状态 | `done` |
| 密钥 | `events.json` / `runtime.log` 无密钥字面量（脚本字面量断言） |
| 严格标记 | `KEEL_E2E_STRICT_MARKER=1` 时工具结果逐字等于标记串（已验证） |

最终证据目录（原始日志，**不入库**）：`/tmp/keel-real-model-final/`
（另有每次运行的 `/tmp/keel-real-model.*`；报告只引用上面这些摘要值）。

失败回合（错误 key 打真实网关）：输入 201 接受；0.96–6.6s 后事件
`assistant.message(stopReason=error)`，`errorMessage` 为
`transport: upstream 401: {"code":"INVALID_API_KEY",...}`；错误信息与证据无密钥字面量。
**观察**：run 状态仍为 `done`（见下条）。

## 7. 已知边界

- **模型失败不改变 run 状态**：本分支 `LoopEventStream.Result` 不返回 Go error
  （`internal/loop/loop.go:353-354`），worker 的 Finish 收到 `Err=nil`，run 落
  `done`；失败只在终止 assistant 消息（`stopReason=error` + `errorMessage`）里可见。
  `run.failed`/`run.finished` 收口是 `keel-kernel-loop` K3 的待办。
- 租户文档 `agent.*` 已持久化，但运行中进程的 llm 服务来自启动文档，
  本分支没有 HTTP Reload 入口（§1）；换模型需重启进程。
- 一次脚本调用只跑一路 provider；切 Anthropic 只需改
  `KEEL_PROTOCOL`/`KEEL_MODEL`（见 §2），断言逻辑不变。
- 模型是否调用工具由模型决定；脚本用确定性的提示词 + 唯一标记串提高命中率，
  并把“标记串是否原样回显”作为摘要字段（`markerEchoed`），默认不作硬门。
- 证据里的消息正文只留在 `/tmp`（不入库、不贴报告）；报告只写长度/seq/耗时。
