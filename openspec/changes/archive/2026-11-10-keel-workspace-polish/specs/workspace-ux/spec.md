# Workspace UX（工作区体验）

基准：`references/zai-org__zcode` 的对话面规则（下称「基准」）。每条要求给出可验收的行为，不写主观形容词。

## ADDED Requirements

### Requirement: 会话标题可得

- `GET /v1/sessions` 的行 MUST 带 `title`（派生自首条用户消息）与 `createdAt`；无消息的会话 `title` 为空。
- 客户端 MUST NOT 把会话 id 当标题渲染；`title` 为空时显示「新会话」＋相对时间。

#### Scenario: 首条消息后
- **WHEN** 在会话里发出第一条消息并返回列表
- **THEN** 该行显示消息派生的标题（不是 id），仍按创建时间排序

### Requirement: 等待人类 ≠ 正在思考

- 存在未答问句（decision）时，对话 MUST NOT 显示加载动画；状态词 MUST 使用「等待确认」。
- 列表行 MUST 显示「等待确认」徽标，且该徽标 MUST NOT 点亮未读标记。

#### Scenario: 会话被阻塞
- **GIVEN** 某会话有待答问句
- **WHEN** 打开列表与对话
- **THEN** 列表该行有「等待确认」，对话不显示加载动画

### Requirement: 消息操作

- 每个回合的最后一段助手文本 MUST 提供：复制（合并该回合助手文本）、时间（相对或绝对，悬停/移动端常显）。
- 失败或不满意的回合 MUST 可重试：`POST /v1/sessions/{id}/retry {turnId}` 追加一次新 run（账本只追加，不改写历史）。
- 用户消息 MUST 显示可复制的内容（编辑/重写不在本轮）。

#### Scenario: 复制与重试
- **WHEN** 点击复制
- **THEN** 剪贴板是该回合合并后的助手文本；点击重试后新 run 开始，旧回合仍在账本里

### Requirement: 滚动意图与记忆（逐会话）

- 滚动 MUST 是意图状态机（FOLLOWING/DETACHED）：用户上滚立即脱离，内容增长与终态转换 MUST NOT 把视口拽回底部。
- 意图与阅读位置 MUST 按会话保存并在重开时恢复；「回到底部」MUST 仅在脱离且内容溢出时出现。

#### Scenario: 切会话不串
- **GIVEN** 在会话 A 上滚到历史
- **WHEN** 切到会话 B
- **THEN** B 处于 FOLLOWING（除非 B 自己被脱离过），A 的位置在切回时恢复

### Requirement: 列表顺序稳定

- 列表 MUST 按 `createdAt` 排序并以就地更新的方式反映状态；仅状态变化的刷新 MUST NOT 改变行序。

#### Scenario: 后台完成
- **WHEN** 一个较早的会话完成一次运行
- **THEN** 该行状态更新，但仍在原位置

### Requirement: 错误可见且可恢复

- 回合级失败 MUST 显示在输入框上方（可关闭），MUST 携带权威原因；可恢复时 MUST 提供重试。
- 关闭 MUST 只影响当前面板；重开会话时仍存在的错误 MUST 再次出现。

#### Scenario: 模型失败
- **WHEN** 一次运行以 provider 错误结束
- **THEN** 输入框上方出现错误横幅（含原因与重试）；关闭后重开会话仍看到该错误

### Requirement: 单一流式来源与静默加载

- 已提交文本与流式预览 MUST 走同一渲染路径；终端文本 MUST 与快照逐字一致（不得二次排版/计时）。
- 加载状态 MUST 只有动效，MUST NOT 出现「正在思考」等文案；MUST NOT 出现面向开发者的措辞（如「尚未提交」）。

#### Scenario: 提交瞬间
- **WHEN** 流式文本转为已提交
- **THEN** 无重排/闪烁，文本与提交前一致

### Requirement: 空态建议只预填

- 空态建议 MUST 只把文本填入输入框，MUST NOT 自动发送。

#### Scenario: 点建议
- **WHEN** 点击一个建议 chip
- **THEN** 输入框被预填，光标就位；消息未被发送

### Requirement: 工具与思考不挡答案（P1）

- 工具调用 MUST 按家族分组（Explore/Terminal/Changes，最少两个子项），父状态取可见阶段尾部；完成摘要 MUST 含数量与失败数（`N 个命令，M 个失败`）。
- 详情 MUST 结果先行，MUST NOT 以原始 JSON 作为主视图。
- 思考 MUST 在流式中默认折叠并显示一行摘要，完成后显示耗时；最新答案 MUST 渲染在折叠之外。
- 分段折叠与计时 MUST 使用服务端分段起止时间；缺失时可用本地时钟，但 MUST NOT 写回账本/会话。

#### Scenario: 工具密集回合
- **WHEN** 一个回合里有 5 次 shell 调用、1 次失败
- **THEN** 读到一行摘要（5 个命令，1 个失败），展开后看到每条与失败详情；答案文本在折叠之外

### Requirement: 通知卫生（P1）

- 问句通知 MUST 按 decisionId 去重；重连回放/水合 MUST NOT 重复弹通知。

#### Scenario: 断线重连后
- **GIVEN** 一条已通知过的未决问句
- **WHEN** 连接断开并以相同 decisionId 回放该问句
- **THEN** 不再弹第二次通知（客户端按 decisionId 记账）
