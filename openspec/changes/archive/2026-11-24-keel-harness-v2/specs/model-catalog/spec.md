# Model Catalog（逐模型事实、价格与设置/执行投影）

本 spec 定义模型目录的三件事：逐模型事实（能力、思考档、价格）的文档词汇与合并语义；
能力事实在运行时的强制执行；以及「设置视图（`/v1/providers`）」与「可执行注册表
（`/v1/models`）」两个投影的职责边界，和参数编辑器据此必须遵守的最小写入规则。

代码真值：`internal/modelcfg`（解析/校验）与 `web/src/data/model-catalog.ts`（界面派生）。
产品说明见 `docs/MODEL-CATALOG.md`。

## ADDED Requirements

### Requirement: 逐模型事实词汇与合并语义

逐模型配置 MUST 通过 `providerModelRules` 的精确 `(providerId, modelId)` 规则表达，规则
`config` MUST 是 RFC 7386 JSON Merge Patch：缺省继承下层、`null` 清除、数组替换、对象逐键
合并。词汇 MUST 为 `providerType`、`properties.contextWindow`、五个输入模态键
`text/image/video/audio/pdf`、四个输出模态键 `text/image/video/audio`、三个
`properties.supports*`、`options.reasoningLevel`、`options.maxOutputTokens` 与 `pricing`。
未知键 MUST 被拒绝。

#### Scenario: 逐键合并
- **WHEN** 一条精确规则只声明 `properties.inputFormat.image:true`
- **THEN** 其它模态键继承下层规则的声明，只有 `image` 被覆盖

#### Scenario: null 清除一个字段
- **WHEN** 精确规则声明 `properties.contextWindow:null`
- **THEN** 该模型不再继承下层的上下文窗口（视图按未声明/不完整处理），兄弟字段不受影响

#### Scenario: 未知键被拒
- **WHEN** 文档写入 `pricing.inputPrice`
- **THEN** 服务端拒绝并指出未知字段 `inputPrice`

### Requirement: 价格语义与备注上限

`pricing` MUST 只用于展示与估算：MUST NOT 参与可执行性判定，也 MUST NOT 进入请求补丁。
`pricing.notes` MUST 最多 200 个字符（按码点计数）且 MUST NOT 含换行；该上限 MUST 由服务端
校验（客户端 `maxLength` 只是提前反馈）。

#### Scenario: 价格块往返
- **WHEN** 文档声明 `pricing{currency:"USD", input:0.27, output:1.1, cacheRead:0.07, free:false}`
- **THEN** 解析视图逐字段返回同一组值，模型可执行性不变，请求补丁不含任何价格字段

#### Scenario: 超长备注被拒
- **WHEN** 一份文档写入 201 个字符的 `pricing.notes`
- **THEN** 服务端拒绝并给出「notes must be at most 200 characters」，文档不被保存

### Requirement: 能力事实的强制执行

`GET /v1/models` MUST 发布 `enforcement{toolCall, media}`，说明服务端实际执行的声明事实。
工具门 MUST 拒绝向声明 `supportsToolCall=false` 的模型发送工具；媒体门 MUST 按
`properties.inputFormat` 分发附件（未声明任何输入模态的模型按纯文本处理）。客户端 MUST
读取 `enforcement`，MUST NOT 仅凭模型声明的能力就向用户承诺行为。

#### Scenario: 未声明模态只收到文本
- **WHEN** 一个模型没有声明任何 `inputFormat` 键，用户发送一张图片
- **THEN** 附件以文本块进入请求，不发送原生 image part

#### Scenario: 工具门
- **WHEN** 一个模型声明 `supportsToolCall=false`
- **THEN** 该模型的请求不带工具定义

### Requirement: 设置视图与可执行注册表的边界

`GET /v1/providers`（设置视图）MUST 返回每个可见 Provider 及其**全部**成员模型，包括不完整、
停用或凭据无法解析的模型，并 MUST 在对应层携带 issues；隐藏 Provider MUST 不出现。
`GET /v1/models`（可执行注册表）MUST 只返回可见 Provider 中凭据可解析、完整且启用的模型；
选择器再按 `selectable !== false` 收窄。

因此参数编辑器与 Provider 卡片的事实展示 MUST 以设置视图为种子；只有设置视图未携带该模型时
才可退回可执行注册表。设置视图也没有该模型时，界面 MUST 显示明确的「尚不可用/缺少凭据」
状态，并 MUST 说明未修改的字段不会被写回。

#### Scenario: 凭据不可解析的模型仍可读
- **WHEN** Provider 的 env 凭据在本进程未设置
- **THEN** `/v1/providers` 仍返回该 Provider 与其模型（含解析出的上下文/价格）及凭据 issue，
  `/v1/models` 不返回其模型

#### Scenario: 停用的模型仍在设置视图
- **WHEN** 一个模型被规则声明 `enabled:false`
- **THEN** `/v1/providers` 返回它（`enabled=false` 与 issue），`/v1/models` 不返回它

### Requirement: 参数编辑器只写读者改动的字段

参数编辑器 MUST 以打开时展示的解析事实为基线；保存 MUST 只写与基线不同的段与键：未编辑的
模态/能力/思考档/价格 MUST NOT 出现在写入的规则里；被展示过又被清空的字段 MUST 写 `null`；
从未展示的字段 MUST NOT 被写成 clear。未改动任何字段的保存 MUST 是 no-op（不写文档、不新增
规则）。因此一次空白保存 MUST NOT 产生 `inputFormat/outputFormat` 的全 `false`、MUST NOT 写
`reasoningLevel:null`，也 MUST NOT 让模型因能力被写空而变成不可执行。

#### Scenario: 缺凭据模型原样保存
- **WHEN** 用户打开一个凭据不可解析的模型的「参数」对话框，直接保存
- **THEN** 文档不新增该模型的规则，模型的能力声明与思考档保持不变

#### Scenario: 只改一个字段
- **WHEN** 用户在已有解析事实的模型上只修改上下文窗口并保存
- **THEN** 写入的精确规则只含 `properties.contextWindow`，其它能力、思考档与价格不出现在规则里
