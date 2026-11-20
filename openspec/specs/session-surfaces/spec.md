# session-surfaces Specification

## Purpose

Keel 客户端的基础会话 surface 契约：LLM/Image/Realtime 等 surface 的 composer+timeline 组件边界、五态、只消费 Harness 事件与 Provider descriptor，以及 AgentTeam captain surface 的判定与成员任务只读投影/团队动态隔离。

## Requirements

### Requirement: Provider 类型决定会话面板

客户端 MUST 根据会话解析出的 Provider 类型装载 `surfaceKind`：

| Provider type | surface |
|---|---|
| llm | 文本对话 |
| realtime | 双向语音 |
| image | 生成/编辑画廊 |
| tts / stt | speech 工作台 |
| video / 未识别类型 | 占位面（无 composer/timeline） |

Provider 类型 MUST 声明 `surface` descriptor；客户端 MUST 使用内置 surface 或插件贡献，
MUST NOT 在 `ConversationScreen` 里硬编码「消息列表 + 文本框」。`video` 与未识别类型 MUST
装载占位面（类型名 + 原因 + 设置入口），MUST NOT 渲染 composer 或伪造结果，MUST NOT 回落
llm 聊天壳。

#### Scenario: 选择 realtime 模型
- **WHEN** 标准模式选择 realtime 模型并开始
- **THEN** 进入 realtime surface：composer 是持续双向语音入口，timeline 是转写历史

#### Scenario: 选择 image 模型
- **WHEN** 标准模式选择 image 模型并开始
- **THEN** 进入 image surface：composer 是 prompt/参考图/参数，timeline 是按 turn 分组的画廊

#### Scenario: video 无专用面板
- **WHEN** 会话解析为 `video` surface
- **THEN** 显示占位面：类型名「视频生成」、原因「当前没有已接入的视频 Provider」与
  「打开模型设置」；不出现输入框，不出现任何演示或伪造的视频结果

#### Scenario: 未知类型回退
- **WHEN** 会话的 Provider 类型存在但不在 registry（例如 `hologram`）
- **THEN** 显示 generic 占位面，原因点名该类型；不崩溃、不伪装成文本会话

### Requirement: 会话 surface 不可静默跨类型切换

同一 surface 内 MAY 换模型；跨 surface 换模型 MUST 要求新建会话，并在选择器里明确提示。
MUST NOT 把旧 surface 的历史伪装成新 surface 的历史。

演示模型（`demo-realtime` / `demo-image` / `demo-stt` / `demo-tts`）跨 surface 选择时 MUST 走与
真实模型相同的「新建会话」门禁。演示预览不是会话：preview MAY 接管空面板，MUST NOT 顶替已开
会话的其它 surface。

#### Scenario: LLM → realtime
- **WHEN** 用户在已有 LLM 会话中选择 realtime 模型
- **THEN** 弹“需要新建会话”的确认；取消则保持原模型/会话

#### Scenario: 演示预览跨 surface
- **WHEN** 已打开 LLM 会话，用户选择演示 realtime/image 面板
- **THEN** 与真实模型相同：提示「需要新建会话」，取消则保持原面板；
  确认后创建新会话再打开演示预览；空会话可直接预览

### Requirement: Surface 组件契约

每个 surface MUST 由 composer + timeline 两个可独立替换的组件组成，并 MUST 覆盖
空/加载/错误/权限不足/成功五态。Surface MUST 只消费 Harness 事件与 Provider descriptor，
MUST NOT 直连内核。

#### Scenario: 麦克风权限拒绝
- **WHEN** 用户拒绝麦克风权限
- **THEN** realtime surface 显示权限不足态，并提供同 surface 文本兜底入口

### Requirement: AgentTeam captain surface

AgentTeam 模式下，captain 的默认模型 MUST 决定主会话 `surfaceKind`。成员 MAY 有不同
surface；成员结果 MUST 通过团队消息/任务卡进入 captain 时间线，MUST NOT 混入 captain 的
转写历史或 tool 行。

本 change 交付 LLM captain 的 web 团队界面（header/名单 + captain 时间线的团队动态）与
`GET /v1/sessions/{id}/team-tasks` 只读投影。非 LLM（realtime/image）captain 的团队界面与
成员卡 surface 标记仍为暂缓项 (c)：`resolveCaptainSurface` 判定接缝已实现并由单测 pin，
成员 surface 标记在该团队界面交付后验收。

#### Scenario: realtime captain（判定接缝）
- **WHEN** captain 默认模型是 realtime
- **THEN** `resolveCaptainSurface` 返回 realtime surface，主会话按该 surface 装载；
  非 LLM 团队界面与成员 surface 标记仍为暂缓项 (c)

#### Scenario: 无 captain 回退
- **WHEN** 团队没有 captain，或 captain 的模型无法在 registry 解析
- **THEN** 主会话使用 `llm` 基线 surface，不猜测 realtime/generic

#### Scenario: LLM captain 的团队动态
- **WHEN** LLM captain 的团队会话有成员任务卡
- **THEN** captain 时间线显示独立的「团队动态」区域，按任务卡渲染 title/status/updatedAt；
  卡片 MUST NOT 进入 captain 的 user/assistant 转写或 tool 行，且没有结果正文时不伪造结果文本

### Requirement: 成员任务只读投影

`GET /v1/sessions/{id}/team-tasks?limit=&after=` MUST 是 `task_cards` 的只读投影：
MUST NOT 复制卡片、MUST NOT 写账本或 captain transcript。响应里的卡 MUST 是
`parentTaskId` 指向该会话内任一卡片的子卡；子卡仍属于其子会话账本。

端点 MUST 校验会话属于请求租户：缺少 `X-Tenant-ID` 为 401；外租户或不存在的会话为 404。
`limit` MUST 在 1..200（默认 50），`after` MUST 是不透明 keyset cursor；坏 cursor MUST 400；
没有成员卡 MUST 返回 200 + `tasks: []`。响应 MUST NOT 包含结果正文（无 result/summary 字段）。

#### Scenario: 父卡关联子卡
- **WHEN** 子会话的卡以 `parentTaskId` 指向 captain 会话中的卡
- **THEN** 端点返回该子卡，携带其 `sessionId`/`parentTaskId`，且不返回 captain 自身的卡

#### Scenario: 租户/会话边界
- **WHEN** 请求外租户或不存在的会话
- **THEN** 404；缺少 `X-Tenant-ID` 时 401，且不返回任何卡片

#### Scenario: keyset 分页
- **WHEN** 成员卡数量超过 `limit`，调用方用 `nextCursor` 请求下一页
- **THEN** 页间无重复/遗漏；`limit` 超出 1..200 或 `after` 损坏为 400

#### Scenario: 空态与隔离
- **WHEN** 会话没有成员任务卡
- **THEN** 返回 200 + `tasks: []`，不回退到 captain 转写/tool 行，也不伪造卡片或结果文本
