# provider-config

## ADDED Requirements

### Requirement: 身份是 providerId/modelId

Provider Config MUST 描述访问方式（api-key 静态配置或环境变量引用）、API 协议、Base URL 与静态
header；Model Config MUST 描述能力与选项。模型身份 MUST 是 `providerId + modelId`，
MUST NOT 仅凭 modelId 路由；同一 modelId 在不同 Provider 下 MAY 解析出不同的协议、端点与能力。

#### Scenario: 同名模型、不同网关
- **GIVEN** `s2api/deepseek-v4.1-flash` 与 `a6api/deepseek-v4.1-flash`
- **WHEN** 分别解析
- **THEN** 两条 Effective 配置各自带自己的 Base URL/协议，能力与选项独立覆盖，互不污染

#### Scenario: 只给 modelId
- **GIVEN** 一个未指定 providerId 的选择
- **WHEN** 解析
- **THEN** 只有在内置 catalog 中存在唯一去重身份时才落到该身份，否则报配置错误（不猜）

### Requirement: 三层稀疏 overlay 与完整 Effective

解析顺序 MUST 是 内置 catalog → 租户 overlay → 选择级精确覆盖。overlay MUST 是稀疏的：
缺省字段继承上一层，显式 `null` 清除，具体值覆盖。Effective 配置 MUST 完整（enabled、properties
全叶、reasoningLevel 的 values 与非空 map、maxOutputTokens 的 max 与 map）；不完整或被禁用的
条目 MUST 保留在设置视图中并带 issues，但 MUST NOT 进入可执行 Registry。

#### Scenario: 租户只改端点
- **GIVEN** 内置模型声明了能力与选项，租户只覆盖 `baseUrl`
- **WHEN** 解析
- **THEN** Effective 保留内置能力与选项，仅端点变化

#### Scenario: 未知模型
- **GIVEN** catalog 里没有的 modelId
- **WHEN** 以租户 overlay 提供完整端点与能力
- **THEN** 可执行；若缺必需字段则只出现在设置视图并标记 issue，不执行

### Requirement: 有序规则匹配

Model 规则集合 MUST 按固定顺序匹配：`modelRules`（模型族通配）→ `modelApiRules`（模型+协议
类型）→ `providerSiteRules`（精确站点 URL）→ `providerModelRules`（providerId+modelId）。
同一集合内 MUST 保持书写顺序，MUST NOT 引入动态优先级；模型名匹配 MUST 忽略大小写并整串匹配；
URL 匹配 MUST 使用规范化值。

#### Scenario: 具体变体覆盖族基线
- **GIVEN** 族规则给所有 `.*deepseek.*` 设 contextWindow=64k，具体规则给
  `deepseek-v4.1-flash` 设 128k
- **WHEN** 解析后者
- **THEN** 取 128k；数组顺序不变时结果确定

### Requirement: 能力与选项是唯一事实源

Model properties MUST 覆盖 `contextWindow`、`inputFormat{text,image,video,audio,pdf}`、
`outputFormat{text}`、`supportsToolCall`、`supportsJsonSchemaOutput`、`supportsNativeWebSearch`。
选项规格 MUST 覆盖 `reasoningLevel`（有序 values + 值→请求补丁 map）与 `maxOutputTokens`
（max + 补丁 map）。执行分支（普通请求、压缩、子代理、闲时）MUST 读取同一份 properties/options，
MUST NOT 按模型名硬编码思考档位或能力。v1 的补丁模板是闭集 JSON（未知值 fail-closed）。

#### Scenario: 思考档位映射
- **WHEN** 选择 `reasoningLevel=high` 且协议是 openai-chat-completions
- **THEN** 请求体得到 `reasoning_effort=high`；Anthropic 协议映射为 thinking budget，
  且不超过该模型 maxOutputTokens

#### Scenario: 能力门
- **GIVEN** 模型 `inputFormat.supportsImage=false`
- **WHEN** 提交带图片附件的输入
- **THEN** 产品层拒绝或降级（按附件策略），MUST NOT 把图片塞进请求

### Requirement: 不可变 ActiveModel

`ModelFactory` MUST 从当次解析结果创建不可变 ActiveModel（provider/model/协议/端点/凭据引用/
properties/options 固定）。run MUST 在 claim 时取得并冻结 ActiveModel（随 loadout pin）；
配置更新 MUST 只影响之后创建的 run，MUST NOT 改变在途请求。

#### Scenario: 运行中改配置
- **GIVEN** 一个 run 正在执行
- **WHEN** 租户把模型切换到另一个 provider
- **THEN** 该 run 继续用原 ActiveModel；下一个 run 才用新配置

### Requirement: 选择与四层状态

Selection MUST 只保存 `{providerId, modelId, options.reasoningLevel?}`，MUST NOT 复制端点/能力/凭据。
选择 MUST 在提交时对配置校验。草稿、提交、会话当前、Active 四层 MUST 分开；v1 以「租户默认
selection + 每条 user.message 携带的 selection」表达，运行取最新已提交 selection。

#### Scenario: 队列中改模型
- **GIVEN** 一条输入带着旧 selection 在队列里
- **WHEN** 用户改选并提交新输入
- **THEN** 旧输入按提交时的选择执行，不回填；新 run 用新选择

### Requirement: 设置视图与 Registry 同源

设置视图与可执行 Registry MUST 来自同一次解析。设置视图 MUST 含 Effective 配置、issues、
executable/selectable；Registry MUST 只含完整可执行项。`GET /v1/providers` 返回设置视图，
`GET /v1/models` 返回可选择的扁平列表。

#### Scenario: 不完整配置
- **GIVEN** 租户写了一个缺 API 协议的 provider
- **WHEN** 调用 `/v1/providers`
- **THEN** 该 provider 出现且带 issue、`executable=false`；`/v1/models` 不含它

### Requirement: 兼容与门禁

租户文档既有的 `agent.model/baseURL/protocol/provider` MUST 被归一为一条 selection 参与解析
（旧部署零改动）。内核包 MUST NOT import `internal/modelcfg`；既有 16 门验收与 R1 MUST 不退化；
真实模型 E2E MUST 用 selection 跑通（providerId+modelId 到达线路，不出现 provider 名冒充 model id）。

#### Scenario: 旧配置照常工作
- **GIVEN** 只配置了 `agent.model=deepseek-v4.1-flash, baseURL=…, protocol=openai`
- **WHEN** 启动
- **THEN** 解析为一条匿名 provider + 该模型，行为与升级前一致

#### Scenario: 线路上的身份
- **WHEN** 真实模型回合发出请求
- **THEN** 请求体 `model` 等于 selection 的 modelId；日志记录 providerId/modelId 两个身份
