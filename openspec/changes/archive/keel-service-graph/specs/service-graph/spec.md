# service-graph

## ADDED Requirements

### Requirement: 服务描述符

每个可替换服务 SHALL 声明：`name`、`version`、`provides`、`requires`（必需）、
`optional`（可选）、`factory`、`effects`/清理范围。描述符 MUST 可静态枚举，
不得依赖运行时反射猜测。

#### Scenario: 描述符缺失
- **WHEN** loadout 中某个服务没有声明 `provides`
- **THEN** loadout 校验失败并拒绝启动，不得静默跳过

### Requirement: 不可变 loadout

loadout SHALL 是不可变快照，包含服务集合、版本和配置摘要；运行时 MUST 记录
`loadout_id` / `revision`。同一 run MUST 固定引用一个 revision，不得在 token 级
重新解析。

#### Scenario: 运行中改配置
- **WHEN** 管理员修改租户 loadout
- **THEN** 已开始的 run 继续使用旧 revision；新 run 使用新 revision

### Requirement: 依赖门控

必需依赖未满足时，服务 MUST 保持 `PENDING`，而不是启动失败或空指针运行。
可选依赖缺失时 MAY 正常激活，但 MUST 在诊断中登记降级。

#### Scenario: 缺少工具执行器
- **WHEN** `pi-loop` 必需 `ctx.tools`，但 loadout 未提供
- **THEN** `pi-loop` 处于 `PENDING`；其他不依赖它的服务仍可运行

#### Scenario: 可选 sandbox 未启用
- **WHEN** 某服务声明 sandbox 为 optional 且未提供
- **THEN** 服务激活，诊断记录 sandbox disabled

### Requirement: 循环与重复 provider 拒绝

loadout 解析 SHALL 拒绝依赖环和同一 service name 的多个必需 provider。
拒绝发生在启动前，不能进入半启动状态。

#### Scenario: 循环依赖
- **WHEN** A requires B，B requires A
- **THEN** loadout 校验失败，返回可读的环路径

#### Scenario: 两个 provider 同时提供 ctx.llm
- **WHEN** loadout 同时启用两个 `provides: llm` 的必需服务
- **THEN** 启动前拒绝，除非显式声明一个为 primary、另一个为 fallback

### Requirement: 生命周期与 effects

服务生命周期 SHALL 为 `PENDING → LOADING → ACTIVE → RETIRING → UNLOADING`；失败进入
`FAILED`。`RETIRING` 表示不再接新 run，但仍有在途引用。启动时产生的 effects MUST 被登记；卸载时 MUST 按反序清理。

#### Scenario: 启动到一半失败
- **WHEN** 服务 B 启动失败，A 已 ACTIVE
- **THEN** B 回滚，A 不受影响；loadout 报告失败原因，不留下半注册的工具/监听器

### Requirement: 反序卸载

替换/停用 provider 时，**被替换实例**先进入 `RETIRING`；依赖它的在途 run 继续使用旧实例。
未变化服务 MAY 跨代共享，不强制重载。
引用归零或 drain 截止后，依赖方 MUST 先 `UNLOADING`，provider 后卸载；新 provider 激活后，
依赖方按依赖顺序重新加载。

#### Scenario: 替换 LLM provider
- **WHEN** loadout 将 `llm` 从 A 换到 B
- **THEN** 依赖 `llm` 的 `pi-loop` 先卸载，A 释放，B 激活，`pi-loop` 再加载

### Requirement: generation

每次 loadout 重新装配 SHALL 生成新的 generation；诊断与日志 MUST 能区分 generation。
指标 MUST NOT 使用 revision/generation 作为标签（避免高基数）；指标只带低基数状态/loadout 名。旧 generation 的 effect 不得泄漏到新 generation。

#### Scenario: 重载两次
- **WHEN** 同一服务连续重载两次
- **THEN** generation 递增，旧监听器/定时器不再触发

### Requirement: 默认 loadout 行为等价

默认 loadout MUST 对 W0 合并后的 R1 基线做差分：规范化 HTTP 状态/头、事件日志 dump、
工具序列；白名单只允许时间/随机 ID/日志行序，白名单外 0 差异。

#### Scenario: 默认 loadout 回归
- **WHEN** 以默认 loadout 启动
- **THEN** 现有 E2E、PI conformance、2GB/1000 基线全部保持

### Requirement: 旧 generation 的引用收尾

旧 generation MUST 记录在途 run 引用；`RETIRING` 期间 MUST NOT 接新 run。
引用归零或 drain 截止后，按反序卸载；超时在途 run MAY 按失败处理，但必须记录原因。
禁止“替换 provider 立即卸载旧实例”。宿主 MUST 周期调用过期收尾（ticker/后台任务），
不能只在 Apply/Acquire 时顺带触发，否则空闲进程的 RETIRING 永不收尾。

#### Scenario: 运行中替换 provider
- **GIVEN** run R 正在使用 generation 1 的 llm
- **WHEN** 管理员把 loadout 切到 generation 2
- **THEN** generation 1 进入 RETIRING；R 继续用旧 llm 完成；新 run 用 generation 2；R 结束后旧实例按反序卸载

### Requirement: loadout 与 TenantConfig 的关系

loadout SHALL 是 TenantConfig 的解析结果：TenantConfig（agent/skills/mcp/hooks/
compaction/telemetry/tools/secretRef）→ 服务描述符解析 → 不可变 loadout + revision + 摘要。
租户配置修改 MUST 只在下一 run 生效；在途 run 固定旧 revision。

#### Scenario: 修改租户配置
- **WHEN** 管理员更新 TenantConfig
- **THEN** 新 run 解析新 loadout revision；在途 run 的 revision 不变

### Requirement: R1 差分基线

W0 合并完成后 SHALL 冻结 R1 基线；默认 loadout 的验收器必须能重放同输入并输出差异。
"逐字节等价"不是验收标准；R1 白名单外 0 差异才是。

#### Scenario: 默认 loadout 差分
- **WHEN** 用 R1 输入脚本分别驱动旧装配与默认 loadout
- **THEN** 状态码/头/事件 dump/工具序列在规范化后一致；差异仅有白名单字段

### Requirement: 替换 loop 的 conformance 门

agent loop 是服务，但替换 `pi-loop` MUST 通过 PI conformance 套件；未通过的
loop MUST NOT 进入生产 loadout。

#### Scenario: 候选 loop 失败
- **WHEN** 一个 loop 在某项 PI 事件/工具语义测试失败
- **THEN** loadout 拒绝选择它，默认仍为 `pi-loop`
