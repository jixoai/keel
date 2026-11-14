# provider-registry Specification

## ADDED Requirements

### Requirement: Provider 类型由插件声明

Provider **类型**（capability family）MUST 由插件声明，Harness 与 UI MUST NOT 维护封闭的厂商
枚举。首批类型 MUST 覆盖 `llm` / `image` / `video` / `decision` / `tts` / `stt`；插件 MAY
声明新的类型。TTS/STT 是独立类型，不得被塞进 LLM 或 image 类型。

每个类型 MUST 声明：

- `type_id`、`display_name`、`icon`、`capabilities`
- `config_schema`（Provider 实例配置）
- `auth_schema`（凭据字段；secret 字段必须标记）
- `model_schema`（模型目录条目）
- `parameter_schema`（模型/绑定可调参数；可以为空）
- `feature_ports`（该类型可绑定的功能端口）

#### Scenario: 第三方声明新类型
- **WHEN** 一个插件声明 `type_id: "embedding"` 并带完整 schemas
- **THEN** `/v1/provider-types` 返回它，设置页出现“Embedding”分组，无需改 UI 代码

#### Scenario: 类型声明缺失 schema
- **WHEN** 插件声明的类型缺少 `config_schema` 或 `auth_schema`
- **THEN** 插件装配失败并给出字段级原因；该类型不可被 Provider 实例使用

### Requirement: Provider 实例与租户隔离

Provider 实例 MUST 由租户创建并只对创建租户可见。实例 MUST 记录类型、插件来源、名称、
凭据引用、endpoint、配置、模型目录、启用状态与健康状态。

- 启用状态与健康 MUST 是两个独立轴：`status` = 用户启用状态（`active` / `disabled`），
  `health` = 健康检查结果（`ready` / `needs_auth` / `unreachable` / `invalid_config` /
  `degraded`）。同一字段名 MUST NOT 同时表达两者；写读 MUST 可往返（写入 `disabled`
  后读回仍是 `disabled`，且其健康值不被覆盖）。
- 凭据 MUST 只以 `credentialRef` 形式出现；实例、API 响应、事件、日志和 UI 状态 MUST NOT
  包含明文密钥。
- Provider 实例 MUST NOT 被硬删除当它仍被任何预设引用；删除 MUST 返回引用方，只允许禁用。
- 模型目录 MAY 来自静态声明或运行时发现；缓存 MUST 绑定 Provider 配置 hash，配置变更即失效。
- Provider 健康检查 MUST 区分 `needs_auth` / `unreachable` / `invalid_config` / `degraded`
  与 `ready`，且失败 MUST NOT 删除用户配置。

#### Scenario: 被引用的 Provider 删除
- **WHEN** 用户删除一个被两个预设引用的 Provider
- **THEN** 删除被拒绝，响应列出预设 id/名称，并给出“禁用”动作

#### Scenario: 凭据缺失
- **WHEN** 一个 Provider 的 `credentialRef` 不存在
- **THEN** 状态为 `needs_auth`，模型目录标记为不可用，调用返回 typed `missing_credentials`，
  不尝试不带凭据的请求

### Requirement: 模型目录与参数 schema

每个模型条目 MUST 声明模型 id；SHOULD 声明显示名、上下文窗口、最大输出、能力 flags 和
参数 schema 引用。参数 schema 的合并顺序 MUST 为：类型默认 → Provider 覆盖 → 模型覆盖。

- UI MUST 只展示 schema 声明的参数。
- 对 Provider 不支持的通用参数（如 reasoning/context），UI MUST 禁用并说明原因，MUST NOT
  静默丢弃。
- 模型目录 MUST NOT 包含价格/套餐字段；usage 可以计量，价格逻辑留在合同侧。

#### Scenario: 模型不支持思考强度
- **WHEN** 一个模型条目没有 reasoning 能力
- **THEN** 预设参数编辑器禁用思考强度并显示“该模型不支持”，保存的 tuning 不含该字段

### Requirement: Provider 插件隔离与崩溃边界

Provider 插件 MUST 声明 `layer: provider`，并 MUST 在独立进程或等价隔离边界运行。
Provider 崩溃 MUST NOT 拖垮 Harness、其它 Provider 或其它会话；Harness MUST 标记该实例
`degraded` 并按预设 fallback 继续或返回 typed 错误。

Provider 调用 MUST 支持 timeout、cancel 与 bounded output；图片/视频字节 MUST 进入 layerfs，
Provider 回执只带资产引用。

#### Scenario: 图片 Provider 崩溃
- **WHEN** 图片 Provider 进程在生成中断开
- **THEN** 会话不崩溃，实例标记 degraded，返回 `provider_unavailable` 与可重试提示；
  预设的其它绑定不受影响

### Requirement: 清单与扩展面声明

Provider 插件清单 MUST 能声明 `provides.provider_types`、`provides.provider` 与
`provides.feature_ports`；`KnownFields(true)` MUST 接受这些字段。类型声明的字段名 MUST 是
snake_case（`type_id` / `display_name` / `config_schema` / `auth_schema` / `model_schema` /
`parameter_schema` / `feature_ports`），`config_schema` 等槽位 MUST 接受 `{ref, inline}`
两种形式以及直接写 JSON Schema 文档的简写。

#### Scenario: 插件清单声明类型
- **WHEN** plugin.yaml 写入 `provides.provider: {layer: provider}` 与
  `provides.provider_types`（含完整 schemas）与 `provides.feature_ports`
- **THEN** 清单解析（KnownFields）与校验通过，类型可被 Provider 实例使用

#### Scenario: 类型声明缺少 schema
- **WHEN** 插件声明的类型缺少 `config_schema`、`auth_schema` 或 `model_schema`
- **THEN** 插件装配失败，字段错误路径指向 `provides.provider_types[i].config_schema` 等
  snake_case 键

每个 Provider 插件 MUST 随包提供 `extensibility.json` 与 `EXTENSIBILITY.md`，声明其类型、
配置/参数 schema、功能端口、示例与限制；缺失时创造模式目录 MUST 标记为 `missing`。

#### Scenario: 创造模式聚合 Provider 扩展点
- **WHEN** 创造模式请求本机扩展目录
- **THEN** 已安装 Provider 插件的类型、schema 与 feature ports 出现在目录中；
  未声明扩展面的插件标记 `missing` 而非消失
### Requirement: 模型 Provider 的 CLI 管理面

model-plugin MUST 提供可发现的 CLI 管理能力，至少包括
`keel model provider list|show|add|update|remove|test` 与 `keel model catalog`。
每个子命令 MUST 在插件能力树中声明 summary/description/args/output schema/permissions/
danger/idempotent/callable_by；CLI 与 UI MUST 使用同一 schema 与租户边界。

- CLI MUST NOT 维护封闭的 Provider 类型枚举：`--type` 接受插件声明的任意类型
  （如 `embedding`），新增类型 MUST NOT 需要改 CLI 代码。
- `credentialRef` MAY 为空：此时实例状态为 `needs_auth`，MUST NOT 拒绝创建。
- CLI 的 `status` 词表 MUST 与 API/领域一致（`active` / `disabled`），健康检查结果只用
  `health` 表达。

#### Scenario: CLI 创建 Provider
- **WHEN** 用户运行 `keel model provider add` 并给出类型与凭据引用
- **THEN** Provider 出现在设置页，健康检查可运行；输出不含明文凭据
