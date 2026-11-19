# demo-provider Specification

## ADDED Requirements

### Requirement: 客户端不承载演示

客户端 MUST NOT 内置演示模型、演示数据、演示模式标记或演示注入入口。选型器与各 surface 的
渲染 MUST 只由 `/v1/models`、`/v1/providers`、media capabilities 与 realtime status 决定。

#### Scenario: 生产部署打开选型器
- **WHEN** 部署未启用 demo provider
- **THEN** 选型器只列真实 provider 的模型，没有演示分组、演示徽标或「退出演示」入口
- **AND** 任一 surface 在无 provider 时显示配置态而不是演示内容

#### Scenario: 代码层复核
- **WHEN** 在 `web/src` 搜索「演示」或「demo」
- **THEN** 命中只允许出现在测试的禁用断言与解释性注释中，不出现于渲染逻辑或数据表

### Requirement: 演示是服务端 provider

演示能力 MUST 由一个服务端 provider 提供，默认关闭，经 `KEEL_DEMO_PROVIDER=1` 启用。启用后
该 provider MUST 以普通 provider/model 身份出现在 `/v1/models`，并走与真实 provider 相同的
协议路径（LLM 输入、media 生成、realtime WS、audio 合成/转写）。

#### Scenario: 启用 demo provider
- **WHEN** 服务端以 `KEEL_DEMO_PROVIDER=1` 启动
- **THEN** `/v1/models` 返回 providerId=`demo` 的 llm/image/decision/realtime/tts/stt 模型
- **AND** 客户端不需要任何特殊分支即可选中并跑完一轮

#### Scenario: 未启用 demo provider
- **WHEN** 服务端未设置该变量
- **THEN** `/v1/models` 不含任何 demo 模型
- **AND** `/v1/media/capabilities` 不把 tts/stt/realtime 报告为可用

### Requirement: 演示内容确定性且可识别来源

demo provider 的响应 MUST 确定性可复现，MUST NOT 读取或回显真实用户数据；其资产与转写
MUST 能追溯到 demo provider（模型名 `demo-*`），但客户端 MUST NOT 因此添加展示分支。

#### Scenario: demo 图像生成
- **WHEN** 客户端对 `demo-image` 发起生成
- **THEN** 服务端返回服务端托管的演示资产 URL 与 `model: demo-image`
- **AND** 客户端按普通图像画廊渲染

### Requirement: 目录里列出的 demo 模型必须可服务

`/v1/models` 列出 `demo` 模型时，任何部署 MUST 能服务它们——包括同时持有 vendor 凭据的部署。
路由规则（**已裁决**：S2b 选 A，按显式模型 id 路由）：

- 显式指定 `demo-*` 且开关开启 → demo 服务；
- 不带 model（部署默认）→ vendor 优先，行为不变；
- 开关关闭 → demo 模型一律拒绝（有 vendor 时 400，无 vendor 时 503）。

目录内容 MUST NOT 随「是否有 vendor 凭据」漂移，只由开关决定。理由与实现落点见 `design.md` §2.1。

#### Scenario: 同时存在 vendor 凭据时选 demo 模型
- **WHEN** 部署既有 `KEEL_BASE_URL`/`KEEL_API_KEY` 又设了 `KEEL_DEMO_PROVIDER=1`，客户端对
  `model:"demo-image"` 发起生成
- **THEN** 返回 200，资产 URL 指向 `/v1/demo/assets/*`，`model` 为 `demo-image`
- **AND** vendor 不被调用
- **AND** 同一端点不带 `model` 的请求仍走 vendor

#### Scenario: 开关关闭
- **WHEN** `KEEL_DEMO_PROVIDER` 未设置，客户端对 `model:"demo-image"` 发起生成
- **THEN** 有 vendor 时返回 400（vendor 允许列表拒绝），无 vendor 时返回 503
- **AND** demo 资产端点返回 404

#### Scenario: 目录不随 vendor 凭据漂移
- **WHEN** 两台机器都设 `KEEL_DEMO_PROVIDER=1`，一台另有 vendor 凭据、一台没有
- **THEN** 两台机器的 `/v1/models` 携带同一组 `demo` 模型
- **AND** 差异只出现在 vendor 自己的模型上

### Requirement: 面板必须把选中的模型传给网关

图像与语音面板 MUST 把会话选中的模型 id 放进请求体（`model` 字段）。网关按 model 路由；
省略该字段等于「用部署默认 provider」，在混合部署里会把用户选中的 demo 面板送去真实付费
vendor。选中项不属于该面板家族时 MUST 省略（回落部署默认），不得猜一个模型名。

#### Scenario: 选中 demo-image 生成图像
- **WHEN** 会话选择 `demo / demo-image`，用户在图像面板发起生成
- **THEN** 请求体含 `"model":"demo-image"`
- **AND** 网关返回 `/v1/demo/assets/*` 资产，vendor 不被调用

#### Scenario: 选中不是图像模型
- **WHEN** 会话选择的是 llm 模型（例如回退到默认），图像面板仍能打开
- **THEN** 请求体不含 `model`，由网关用部署默认，行为与引入该规则前一致
