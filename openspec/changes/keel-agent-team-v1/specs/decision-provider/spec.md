# decision-provider Specification

## ADDED Requirements

### Requirement: 决策模型使用 System One 协议

决策 Provider MUST 实现 **System One API 0.2.0**（OpenAPI 3.1）的
`state` + `questions` → `answers` 形状。决策 Provider MUST NOT 被建模成
「带特殊提示词的 LLM」；请求 MUST NOT 使用 `messages`，响应 MUST NOT 期望自由文本。

- `questions` MUST 是 host 命名的 map，至少一个；每个问题 MUST 声明
  `type: noul | choice | score`，TypeSafe 后端 MAY 额外支持 `box`。
- `answers` MUST 以相同 question id 返回，并 MUST 按 `type` 判别。
- `choice`/`score` MUST 返回 `probabilities`，并在支持时返回 `confidence`；`noul` MUST
  只返回概率，MUST NOT 伪造 confidence。
- 原生决策模型 MUST NOT 被要求返回 rationale/explanation；需要解释时由宿主另行调用 LLM。
- 决策 Provider MUST NOT 支持 streaming、tools、temperature、max_tokens 或
  response_format；这些字段 MUST 被拒绝，不得静默忽略。

#### Scenario: Noul 问题
- **WHEN** 宿主提交 `{"urgent":{"type":"noul","instructions":"..."}}`
- **THEN** `answers.urgent` 只含 `type` 与 `noul` 概率；适配器不添加 confidence/rationale

### Requirement: 适配器、模型族与协议分层

Provider 类型 descriptor MUST 区分：

- `protocol: "systemone"` 与 `protocol_version`
- `adapter`: `typesafe` / `cloudflare` / `openrouter` / `openjev`
- `model_family`: `jev` / `clef`
- `model`: 具体模型 id/版本

Cloudflare Clef MUST 被实现为 System One 兼容适配器，而非新协议；适配器 MUST 解包
Cloudflare `{result,success,errors,messages}` envelope。AI Gateway 只作为控制面
（日志/缓存/限流/fallback），MUST NOT 改变请求/响应 schema。

#### Scenario: Cloudflare Clef
- **WHEN** 预设绑定 `@cf/cloudflare/clef`
- **THEN** 请求仍是 `state/questions`，媒体走 Clef 扩展字段；适配器接受 data URL/base64，
  拒绝远程 URL；响应解包后与 TypeSafe 形状一致

### Requirement: 能力预检与限制

决策 Provider 的 descriptor MUST 声明并校验：

- `question_types`（含/不含 box）
- `input_modalities`（text/json/image，可选 audio/video）
- `confidence` 语义（none / choice_score / +box）与 `probabilities` 来源
  （native/derived/normalized）
- `max_questions`、`max_choice_options`、`max_score_levels`、context/body/media limits
- `streaming:false`、`batched_questions:true`、`cross_question_visibility:false`、
  `rationale:false`、`advisory_only:true`
- `native_decision` 或 `llm_adapter`；LLM adapter MUST 标记 `calibration:none`

注册/保存绑定时 MUST 预检：Cloudflare 拒绝 `box`、超限 questions/choices/levels、文本模型
携带媒体、远程图片 URL；不符合 MUST 返回字段级错误，不得等到运行时才失败。

#### Scenario: Box 绑定 Cloudflare
- **WHEN** 预设把一个含 `box` 问题的 rubric 绑定到 Clef
- **THEN** 保存被拒绝，错误指出 `question_types.box` 不受该 Provider 支持

### Requirement: 失败策略与副作用边界

决策调用 MUST 是 all-or-nothing；协议层没有 fail-open。默认策略 MUST 为 **fail-closed**，
尤其当决策会 gate 审批、破坏性操作、财务或权限动作。

- fail-open 只允许显式配置的 shadow/观察路径，且 MUST 记录失败原因。
- 失败 MUST NOT 被合成为 `noul:0`、`choice:"unknown"`、`confidence:0` 或任何默认判断。
- 决策 Provider MUST NOT 直接执行副作用；执行 MUST 由 approve/tool 层在宿主策略下完成。
- 决策结果写入账本时 MUST 关联 `guardrail`/`approval`，并只保留必要摘要、model、request id
  与 usage；敏感 `state` MUST NOT 明文写入日志。

#### Scenario: Provider 超时
- **WHEN** 一个 gate 动作的决策调用超时且策略为 fail-closed
- **THEN** 返回 `decision_unavailable` 并阻止动作；不得返回 allow

#### Scenario: Shadow 模式
- **WHEN** 预设把决策 Provider 设为 shadow
- **THEN** 调用失败只记录 shadow 结果与原因，不影响主流程；UI 显示 shadow 标识

### Requirement: 决策绑定参数

预设中的 decision 绑定 MUST 只暴露决策语义参数：

- `mode`: `shadow` / `enforce`
- `threshold` / confidence band
- `failPolicy`: `fail_closed` / `fail_open`（后者仅 shadow 允许）
- 模型选择与 Provider 级配置（gateway id、long-context 等）

MUST NOT 暴露 temperature、tools、stream、max_tokens 等 LLM 参数；UI MUST 在决策类型表单中
隐藏/禁用这些字段并说明原因。

#### Scenario: 决策参数编辑器
- **WHEN** 用户编辑 decision 绑定
- **THEN** 只看到 mode/threshold/failPolicy/模型/网关配置，不出现 temperature 或 tools
