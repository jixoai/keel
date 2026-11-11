# Supervision（护栏、自动恢复与升级阶梯）

本 spec 定义崩溃/Docker 重启后的自动恢复、子代理的续跑、重试预算与升级阶梯。目标是
**默认自愈**：不点按钮、不重开子代理，AgentLoop 自己回到轨道；预算耗尽时交给上一级。

## ADDED Requirements

### Requirement: 自动恢复是默认行为

Harness 进程启动时（含崩溃后重启、容器重启、升级换代）MUST 扫描所有非终态 run 并**自动恢复**，
MUST NOT 需要人工点击或显式命令。恢复 MUST 从账本重建上下文，MUST NOT 依赖进程内残留状态。
恢复过程 MUST 对每个会话幂等：已提交的工具结果不重放，未完成调用按幂等键重试。

#### Scenario: 崩溃后续跑
- **WHEN** 内核进程在 run 中途被 `kill -9`，Harness 重新启动
- **THEN** 该会话自动继续，事件 seq 连续，客户端按 `Last-Event-ID` 续传，无需用户操作

#### Scenario: 容器重启
- **WHEN** 部署在服务器上的 Docker 容器重启
- **THEN** 所有 `status=运行中` 的会话在启动后自动恢复，恢复事实写入账本（含原因与次数）

### Requirement: 恢复策略由 config.toml 决定

恢复行为 MUST 由 `config.toml` 决定并可轮转：`resume.mode = auto | manual | never`（默认 `auto`）、
`resume.maxAttempts`、`resume.backoff`、`resume.onExhausted = escalate | stop`、
`resume.providerFallback`（额度耗尽时允许切换的 provider 顺序）。策略变化 MUST 只影响下一次
恢复尝试，MUST NOT 改写历史事实。

#### Scenario: 关掉自动恢复
- **WHEN** 某会话的 `config.toml` 声明 `resume.mode=never`
- **THEN** 启动后不自动恢复它，而是把它列为「待恢复」并在 UI 提供一键恢复入口

#### Scenario: 预算与退避
- **WHEN** 一个 run 连续失败达到 `resume.maxAttempts`
- **THEN** 按 `resume.onExhausted` 升级或停止，并把耗尽原因写入账本

### Requirement: 显式 resume 接口

Harness MUST 提供 `POST /v1/sessions/{id}/resume`（以及子代理等价的 resume 动作）：
幂等、可带可选的配置轮转（如换 provider）后立即续跑；MUST 返回本次恢复的起点（`eventSeq`）
与所用配置版本。对终态会话调用 MUST 返回明确结果，MUST NOT 假装成功。

#### Scenario: 换 provider 后恢复
- **WHEN** 一个 run 因某 provider 额度耗尽而停止，owner 改 `config.toml` 后调用 resume
- **THEN** 会话以新配置版本从停止点继续，`run.finished` 记录前后配置哈希

#### Scenario: 重复 resume 幂等
- **WHEN** 对同一会话连续两次 resume（同一 requestId）
- **THEN** 只有一次真正启动 attempt，第二次返回同一结果

### Requirement: 子代理必须可续跑，不得要求重开

停止（失败/超时/被取消之外的中断）的子代理 MUST 能继续，MUST NOT 要求父代理新开一个替代者。
子代理的中断与恢复 MUST 是其自身账本的事实（含中断原因、恢复次数、所用配置）。父代理 MUST
能看到子代理的状态与可恢复性，并能以工具动作触发恢复。

#### Scenario: 子代理停住后继续
- **WHEN** 一个子代理因进程崩溃中断
- **THEN** 它被自动恢复并继续原任务；父卡上看到的是同一个子任务卡，不是新卡

#### Scenario: 父代理接手
- **WHEN** 子代理重试预算耗尽
- **THEN** 升级给父代理：父代理可修改子代理的 `config.toml`（例如换 provider）后调用 resume，
  或改写任务范围后继续

### Requirement: 护栏与升级阶梯

Harness MUST 内置护栏并在触发时按阶梯升级，阶梯为：**supervisor（自动重试）→ 父/主代理
（自主处置）→ owner（人工）→ super-agent（若已安装）**。护栏 MUST 至少覆盖：重复工具调用提醒、
单次调用超时、连续失败次数、额度/成本阈值、上下文/内存压力。每次护栏触发与升级 MUST 是账本
事实（谁、何时、为什么、结果）。

#### Scenario: 重复调用被拦
- **WHEN** 同一工具以同一参数连续调用超过阈值
- **THEN** 系统提醒并抑制重复副作用，事件里可见，模型收到可解释反馈

#### Scenario: 额度护栏
- **WHEN** provider 报额度耗尽
- **THEN** 立即停止重试该 provider，按 `providerFallback` 切换或升级，并把原因写入账本

#### Scenario: 升级到 owner
- **WHEN** 自动与代理内处置都无法继续
- **THEN** 会话进入「待人工」态并通过 `keel.notify` 通知 owner，附上原因、已尝试的处置与建议动作

### Requirement: super-agent 是插件，不是内核

层级高于 main-agent 的 super-agent MUST 以插件形式提供（`keel.super-agent`），MUST NOT 需要
内核改动。它 MUST 能：恢复 main-agent 的会话、为同一问题并行运行多个 main-agent、比较其结果
并收敛。它 MUST 通过 `agent` 聚合工具与 resume 接口工作，其自身行为同样受护栏与账本约束。

#### Scenario: 并行多个 main-agent
- **WHEN** super-agent 接受一个问题并派发两个 main-agent 并行求解
- **THEN** 两者的会话、任务卡与成本都可追溯，super-agent 能选择其一或合并结果

#### Scenario: 不装也有底线
- **WHEN** 未安装 `keel.super-agent`
- **THEN** 系统仍能自动恢复与升级到 owner，功能不缺失，只是少了自动接管的一级
