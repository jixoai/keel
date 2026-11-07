# ecosystem-adapters

## ADDED Requirements

### Requirement: PI loop 是默认服务，不是唯一实现

`pi-loop` SHALL 是默认 agent loop 服务。其他 loop MAY 通过 loadout 替换，
但 MUST 先通过 PI conformance；未通过的 loop MUST NOT 进入生产。

#### Scenario: 默认 loadout
- **WHEN** 未显式选择 loop
- **THEN** 使用 `pi-loop`，行为与当前 Keel 一致

#### Scenario: 自定义 loop
- **WHEN** 租户选择自定义 loop 且 conformance 失败
- **THEN** loadout 拒绝；不会静默回退到不同语义

### Requirement: DSH 整机 sidecar

DSH SHALL 以整个 runtime 作为 ExternalAgentBackend 接入，而不是把 Cordis
`ctx` 逐对象 RPC 化。DSH 插件在 DSH 内部保持零改动。

DSH backend MUST 声明：版本、能力、会话状态所有权、恢复方式。
Keel MUST 将该会话标记为外部 backend 兼容模式。

#### Scenario: DSH 插件不改代码
- **WHEN** 一个现有 DSH 插件在 sidecar 中加载
- **THEN** 它继续使用 Cordis 的进程内语义；Keel 只通过 AgentBackend 协议对话

#### Scenario: 状态所有权
- **WHEN** DSH backend 声明 `state_owner=external`
- **THEN** Keel 不承诺该会话的 kill-9 原生重建；UI/API 显示兼容模式

### Requirement: DSH 工具子集

纯工具类 DSH 插件 SHALL 通过 ToolProvider 暴露为外部工具；工具参数/结果 MUST
是 JSON 可序列化的。工具调用结果 MUST 由 Keel 内核提交。

#### Scenario: 工具插件桥接
- **WHEN** 一个 `defineTool` 插件注册到 DSH sidecar
- **THEN** Keel 可通过 ToolProvider 调用它，结果进入 Keel 事件账本

### Requirement: DSH sidecar 版本与生命周期

DSH sidecar MUST pin 精确 npm 包版本与 Node 运行时版本；Keel MUST 定义：
谁拉起/停止、env 与凭据注入/清洗、健康检查、崩溃退避与 reaper、资源预算。
版本升级 MUST 走兼容矩阵与回归测试，不得自动跟随 latest。

#### Scenario: sidecar 崩溃重启
- **WHEN** DSH sidecar 异常退出
- **THEN** Keel 按退避重启；外部会话标记 backend-unavailable/恢复中；不污染原生账本

### Requirement: DSH 工具桥需要 DSH 侧适配器

纯工具桥 SHALL 通过 DSH 侧导出插件实现；MUST NOT 声称 DSH 工具插件零代码即可被
Keel ToolProvider 调用。适配器必须处理：工具白名单、JSON 可序列化校验、
错误映射、取消、输出裁剪；非 JSON 参数/结果 MUST 拒绝并给出可读错误。

#### Scenario: 非 JSON 参数
- **WHEN** DSH 工具声明了不可 JSON 序列化的参数/结果
- **THEN** 适配器拒绝暴露该工具；诊断说明原因

### Requirement: 禁止逐插件 Cordis RPC

实现 MUST NOT 试图把 `ctx.get/provide/effect/on/emit` 或任意 JS 服务对象
透明代理到 Keel。服务身份、同步语义、effect 所有权无法跨进程无损保留。

#### Scenario: 评审桥接方案
- **WHEN** 方案试图把 Cordis context 作为 RPC 对象暴露
- **THEN** 评审拒绝；应改为整机 backend 或工具子集

### Requirement: 兼容矩阵

文档 SHALL 明确列出支持/不支持：

- 支持：纯工具、skills/资源、部分 LLM adapter、整机 DSH backend；
- 有条件：hooks/扩展（需事件映射与 conformance）；
- 不支持：Web/Desktop UI、Electron/native、依赖本机路径/进程的插件、
  DSH plugin manager/HMR 在 Keel 内原生编排。

#### Scenario: 用户查询兼容性
- **WHEN** 用户想知道某 DSH 插件能否接入
- **THEN** 文档给出类别、所需适配器、已知限制，不承诺“全部无缝”

### Requirement: 双账本只允许兼容模式

若 DSH backend 需要保留自己的会话日志，Keel MUST 明确标记为兼容模式；
原生会话 MUST 继续以 Keel 事件日志为唯一真值。不得把双账本伪装成原生恢复。

#### Scenario: 兼容会话崩溃
- **WHEN** 双账本模式下 DSH backend 崩溃
- **THEN** Keel 报告 backend 不可用；不声称已从 Keel 账本恢复
