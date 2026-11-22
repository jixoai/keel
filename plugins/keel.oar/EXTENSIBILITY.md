# keel.oar 可扩展面

> 机器可读版本：[`extensibility.json`](extensibility.json)。契约正文：
> [`docs/OAR.md`](../../docs/OAR.md)。本文件只描述 OAR 暴露给宿主/UI/创造模式的扩展点。

## 我提供什么

| 扩展点 | 形态 | 说明 |
|---|---|---|
| OAR record stream | `agent_backend` 协议上的 `type=oar.record` 事件 | `seq` + `agentPath` + call/result/status/message/usage/error；durable NDJSON journal |
| 渲染投影 | JSON（`keel.oar.render-projection/1`） | 只提供数据形状与 fixture；W12 UI 拥有 `keel.ui.tool-renderer` 槽位，OAR 不建平行 web renderer |
| 能力协商 | `initialize.capabilities.experimental.oar` | 闭集能力矩阵；启动前 typed rejection |
| 运行时驱动 | `runtime=codex` / `runtime=fake` | Codex 是第一条真实链路；fake 永远 `simulated=true`，不冒充真实模型 |

本包**不声明槽位**（`slots` 为空），`contributes` 也为空：渲染槽位属于 `keel.ui`。
W12 接线时由 UI 侧消费 projection fixture，而不是由 OAR 注入 UI 代码。

## 我接受什么配置

- `runtime`（必填，`codex|fake`）
- `journalDir`（默认 `<user cache>/keel/oar`）
- `workdir` / `model` / `sandbox`（Codex 运行参数；sandbox 默认 `read-only`）
- `requireLiveModel`（拒绝 fake）
- `codexPath`（默认 PATH）

环境变量白名单（sidecar 只拿网关 minimal env + 显式声明）：`KEEL_OAR_JOURNAL_DIR`、
`CODEX_HOME`、`OPENAI_API_KEY`。宿主环境不会整体下传。

## 我能挂什么 hook / 替换什么 provider

v1 不挂宿主 hook，也不替换 provider。OAR 是外部工具适配器：
- 不写账本（事件由既有 `externalgw`/宿主投影）；
- 不建任务卡、不改 prompt；
- 不做模型 provider（Codex 自己管模型与鉴权）。

## 示例

```bash
# 确定性 fake：验证 record stream、seq、agentPath、投影
keel-oar sidecar -runtime=fake -usage -step-delay=50ms

# 真实 Codex：JSONL → OAR records；取消/退出杀进程组
keel-oar sidecar -runtime=codex -workdir /tmp/project -sandbox read-only
```

宿主侧：

```go
service, err := oar.NewService(oar.Config{
    Runtime: oar.RuntimeCodex, JournalDir: dir, RequireLiveModel: true,
})
err = service.Start(ctx)
session, err := service.Open(ctx, oar.OpenRequest{HostSessionID: sid, TaskID: tid})
```

## 限制

- 单条记录 ≤1 MiB；`seq` 每后端会话严格递增，崩溃允许缺口。
- 不给外部运行时注入「请记录任务卡」；归属由 `session.open` 的 `attribution` 带外提供。
- v1 不做 token 级 streaming、approval 桥、Claude、跨 sidecar 生命周期的 resume。
- 当前插件 loader 只接受 tool_provider sidecar；OAR 是 agent_backend，marketplace 安装
  与槽位打包留到 W12（见 README）。
