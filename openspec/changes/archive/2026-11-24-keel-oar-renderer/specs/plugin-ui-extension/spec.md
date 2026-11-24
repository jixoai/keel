## ADDED Requirements

### Requirement: 外部运行记录经 tool-renderer 呈现

客户端 MUST 把外部运行（`external.event`，`type="oar.record"`）的记录经
`keel.ui.tool-renderer` 槽位呈现为一张外部运行卡，而不是只显示一行「外部事件」。

- 卡片 MUST 显示 runtime 与 `simulated` 徽标；`simulated=true` MUST NOT 被呈现为 live；
- 行 MUST 来自服务端投影（`keel.oar.render-projection/1`），客户端 MUST NOT 自行
  折叠记录；
- 归属 `taskId` MUST 只关联已有任务卡，MUST NOT 创建新卡；
- 槽位贡献装载失败、投影接口 4xx/5xx 或目标为 TUI 时 MUST 回退为原文本行，
  MUST NOT 让整轮对话失败；
- 渲染 MUST 使用 kit 的官方组件与主题令牌。

#### Scenario: 外部运行卡
- **WHEN** 一轮对话含 `external.event`（`oar.record`）且投影接口返回 200
- **THEN** 对话中出现外部运行卡：runtime/simulated、call/result/status 行、usage/错误；
  `taskId` 打开已有任务卡

#### Scenario: 投影不可用
- **WHEN** 投影接口返回 501（部署未接 OAR）或网络失败
- **THEN** 该轮只显示既有的「外部事件 · …」文本行，对话其余部分不受影响

#### Scenario: TUI 目标
- **WHEN** 同一段对话在 TUI 宿主渲染
- **THEN** 外部运行以文本行降级呈现，不渲染卡片
