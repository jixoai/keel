# 扩展地图

Keel 只有两种扩展哲学，适用哪一种取决于**你要改的是什么类型的东西**：

1. **协议/原语级变更**（新的执行原语、新传输、新事件 kind、新 loop 实现）属于仓内、规格先行的
   工作：触及内核或协议包，必须走仓库的 OpenSpec 流程。
2. **装配/策略级变更**（用哪个 provider、哪些工具、哪些服务、哪个外部 backend）属于配置与服务图
   工作，不改账本。

选接缝之前，先记住可见性规则：**运行时包全部在 `internal/` 下**，因此只能被本模块内的代码 import。
「嵌入 runtime」因此意味着在本仓检出（或 fork）里工作——见[嵌入 runtime](/zh/core/extend/embed-runtime)。

## 决策表

| 我想…… | 接缝 | 页面 |
|---|---|---|
| 在自己的进程/二进制里跑核心 | `internal/servicehost.Boot` + 内核窄接缝，或 fork 里新增 `cmd/` | [嵌入 runtime](/zh/core/extend/embed-runtime) |
| 增加或替换一个服务（provider、投影、压缩、遥测……） | `servicegraph.ServiceDescriptor` + `loadout.Config.Provide` | [写一个服务描述符](/zh/core/extend/service-descriptor) |
| 替换 agent loop | `agentloop.Loop` 契约 + 钉住的 PI conformance manifest（fail-closed） | [写一个服务描述符](/zh/core/extend/service-descriptor) |
| 对接外部 agent runtime（自己拥有会话状态的 sidecar） | 外部网关上的 `AgentBackend`（`external.backends`） | [接外部 backend](/zh/core/extend/external-backend) |
| 暴露外部工具服务 | `ToolProvider`（`external.tools`） | [接外部 backend](/zh/core/extend/external-backend) |
| 加模型 provider 或换协议 | `provider.StreamFn` + `provider.ResolveProvider` | [写 provider 适配器](/zh/core/extend/provider-adapter) |
| 给模型加一个工具 | `agenttool.ToolRegistry`（内核侧）或宿主 registry（受 `tools.allow` 过滤） | [嵌入 runtime](/zh/core/extend/embed-runtime) |
| 加产品能力（插件、hook、触发器、子代理、审批） | Harness 扩展面（当前实现 `internal/agentext`；插件标准在 Harness 主线） | [Harness 插件](/zh/harness/plugins/) |
| 加新事件 kind | 仓内变更：`kindRegistry` + `internal/store` 常量 + 转录/SSE/R1 规则同步移动 | [事件与协议](/zh/core/events) |
| 改存储或加表 | 仓内变更：`internal/store/schema.sql` 或带编号的内核迁移 | [存储与运行](/zh/core/storage) |
| 做 UI | Harness API + SSE（版本化 `/v1`，能力探测） | [AgentAPI 参考](/zh/core/agentapi) |

## 两条硬规则

**服务可以换，但服务不能改账本。** 服务拿到的是窄接缝（`kernel.Committer`、`kernel.Reader`、
`kernel.LeaseManager`、`kernel.ExternalSessions`，`internal/kernel/seams.go`）并提交提案；内核决定什么
成为事实。如果你的变更需要写 `events`、分配 `seq`、调整提交顺序或绕过租户/租约校验，那你改的是内核——
那意味着规格、迁移编号与 R1 差分门，而不是插件。

**Conformance 门是 fail-closed 的。** 任何提供 `ctx.loop` 的服务都必须给出 PI conformance manifest，
其规范哈希与其 pin 相等；缺声明即拒绝，而不是「默认通过」。替换 loop 就要跑通测试套件并显式声明
（`internal/loadout/conformance.go:55`、`internal/loadout/conformance.go:18`）。

## 改动落在哪里

| 变更 | 你动的文件 | 必须跑的门 |
|---|---|---|
| 新服务描述符 | `internal/loadout/*`、你的工厂、测试 | `go test -race ./internal/loadout ./internal/servicegraph ./internal/servicehost`；默认装配变了还要跑 `scripts/acceptance-all.sh` |
| 新 loop | `internal/agentloop` 实现 + 声明 | PI conformance 套件 + R1 差分 + loop conformance 门 |
| 外部 backend | 租户文档（`external.backends`）+ 一个 provider 进程 | `go test -race ./internal/gateway ./internal/externalgw` + `scripts/external-gateway-smoke.sh` |
| provider 适配器 | `internal/provider` + 注册表条目 | provider 流测试；有凭据则跑真模型 E2E |
| 新事件 kind | `kindRegistry`、store 常量、转录/SSE/R1 规则 | R1 差分（白名单之外零差异）+ 完整 `acceptance-all.sh` |

这些门本身见[测试与验收](/zh/core/extend/testing)。
