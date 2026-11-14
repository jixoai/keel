# session-surfaces Specification

## ADDED Requirements

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
surface；团队界面交付后，成员结果 MUST 通过团队消息/任务卡进入 captain 时间线，
MUST NOT 混入 captain 的转写历史。

本 change 只交付判定接缝 `resolveCaptainSurface`（captain 模型 → surface；无 captain 或无法解析
→ `llm`）；web 团队界面不在本 change 范围（见 tasks 暂缓项 (c)），下列成员规则是团队界面
交付时的验收口径。

#### Scenario: realtime captain（判定接缝）
- **WHEN** captain 默认模型是 realtime
- **THEN** `resolveCaptainSurface` 返回 realtime surface，主会话按该 surface 装载；
  团队抽屉的成员 surface 标记在团队界面交付后验收

#### Scenario: 无 captain 回退
- **WHEN** 团队没有 captain，或 captain 的模型无法在 registry 解析
- **THEN** 主会话使用 `llm` 基线 surface，不猜测 realtime/generic
