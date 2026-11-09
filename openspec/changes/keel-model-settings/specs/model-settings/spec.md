# Model Settings（模型配置的产品面）

## Requirement: 模型配置可读可写

租户级模型配置 MUST 可读、可写、可重置，且写入前 MUST 用与运行期同一套解析器校验。

- **GET `/v1/settings/models`** MUST 返回 `{revision, source, document, issues}`：
  `source` ∈ {`stored`, `deployment`}（有覆盖=stored，否则=启动文档）；`document` 是**当前生效**的模型文档
  （覆盖或部署默认）；`issues` 是解析级诊断（无则空数组）。
- **PUT `/v1/settings/models`** MUST 接受一份完整模型文档（`schemaVersion` / `providers` / 规则数组 /
  `selection`），返回 `{revision, source:"stored", selection, providersExecutable, modelsSelectable}` 摘要。
  - 校验 MUST 覆盖：文档形状、规则引用、默认 selection 可执行（含凭据可解析、驱动可构造、思考档在词汇表内）。
  - 校验失败 MUST 400，body 带字段路径（`{"error":"...","issues":[{"field":"selection.modelId","message":"..."}]}`）。
  - 并发写 MUST 用 `expectedRevision`（可选）守卫：不匹配 409，并返回当前 revision。
- **DELETE `/v1/settings/models`** MUST 清除覆盖并回到部署默认，返回 `{revision, source:"deployment"}`。
- 写入 MUST 落审计：revision 单调递增、`updated_at`、来源与操作者（会话/租户）。

#### Scenario: 保存即生效
- **GIVEN** 服务以部署默认（deepseek）运行
- **WHEN** PUT 一份默认 selection 为 claude-sonnet-5 的文档
- **THEN** 200，revision+1；**不重启**，下一条输入的 run 解析到 claude-sonnet-5（线路与 `run.finished` 都证明）

#### Scenario: 在飞 run 不受影响
- **GIVEN** 一个 run 正在执行
- **WHEN** 此时 PUT 新配置
- **THEN** 该 run 继续用旧代数；之后的新 run 用新配置

#### Scenario: 坏配置被拒且不影响服务
- **GIVEN** 服务正常运行
- **WHEN** PUT 一份默认 selection 指向不存在模型的文档
- **THEN** 400 带字段路径；旧配置继续服务，revision 不变

#### Scenario: 重置回部署默认
- **WHEN** DELETE `/v1/settings/models`
- **THEN** 200 且 source=deployment；后续 run 回到启动文件配置

## Requirement: 热更按代数生效，边缘缝按请求读取

配置变更 MUST 通过装配新服务代数生效，MUST NOT 要求重启；HTTP 边缘的两处读取
（selection 校验、设置/注册表投影）MUST 按请求解析到当前代数，MUST NOT 在 boot 时捕获后不再更新。

- reload 失败（新文档无法装配）MUST 拒绝写入并保持旧代数服务。
- 写入成功后 MUST 在秒级内可观察（`GET` 立即反映新 revision 与文档）。

#### Scenario: 写入后立即读到
- **WHEN** PUT 成功
- **THEN** 紧随其后的 GET 返回同一 revision 与新文档（无缓存窗口）

## Requirement: 设置是独立的面

设置 MUST 是独立于工作区的页面/路由（`/settings`），MUST NOT 以面板形式塞进会话界面。

- 工作区（会话列表＋对话）MUST 保持原样；设置从工作区入口进入、可返回，并保留当前会话。
- 设置页 MUST 有分区导航：模型 / 密钥 / 插件；深链 `/settings` MUST 可直接打开。
- 设置页与工作区共享同一套视觉语汇（NSH 客户端既有 primitives/theme），MUST NOT 引入第二套组件体系。

#### Scenario: 深链直达设置
- **WHEN** 已登录用户打开 `/settings`
- **THEN** 直接进入设置页（不是重定向到工作区），返回后仍在原会话

## Requirement: 客户端设置页——模型

设置页的模型分区 MUST 覆盖：当前生效（来源/revision/默认 selection）、provider 列表与不可执行原因、
模型列表与能力事实/issues、默认模型与思考档编辑、原始文档编辑、保存/重置、字段级错误呈现。

- 设置页 MUST 只管理解析结果中的**可见** provider；隐藏 provider 不出现在设置页（服务端已剔除）。
- 保存失败 MUST 原样呈现服务端 issues（字段路径 + 消息），MUST NOT 用“保存失败”吞掉原因。
- 保存成功 MUST 显示新 revision 与生效状态（source）。

## Requirement: 密钥管理

设置页的密钥分区 MUST 支持按名字写入、覆盖、删除，供模型配置以 `secret:NAME` 引用。

- 列表 MUST 只返回名字与时间（创建/更新时间），MUST NOT 返回值、长度或任何可用于猜测值的信息。
- 写入 MUST 走加密落盘（复用既有 `store.SecretBox`，密钥来自 `KEEL_SECRET_KEY`）；
  未配置密钥时 MUST 明确不可用（503/禁用态 + 原因），MUST NOT 降级为明文存储。
- 值 MUST NOT 出现在日志、事件、错误消息或任何 GET 响应中；写入审计只记名字/时间/操作者。
- 模型配置的 `access.ref` MUST 能把 `secret:NAME` 解析到该存储（`env:NAME`/`${NAME}` 仍查环境）。

#### Scenario: 写入后模型可用它
- **GIVEN** 未设置任何环境变量凭据
- **WHEN** 在设置页写入 `secret:s2api`，并把模型配置的 `access.ref` 设为 `secret:s2api` 保存
- **THEN** 保存成功；下一条输入的 run 正常调用网关；`GET /v1/settings/secrets` 只显示名字与时间

#### Scenario: 未配置密钥盒
- **GIVEN** 进程没有 `KEEL_SECRET_KEY`
- **WHEN** 打开密钥分区或尝试写入
- **THEN** 界面明确说明「密钥存储未启用」并给原因；写入被拒；不回退明文

## Requirement: 插件清单与触发

设置页的插件分区 MUST 呈现装载清单与状态，并允许手动触发已注册的触发器。

- 清单 MUST 含：generation、每个插件的 id/版本/状态/provides/requires/tools/hooks/triggers/diagnostics。
- 触发器 MUST 可列出并手动触发（既有 `POST /v1/triggers/{id}/fire`），触发结果 MUST 可见。
- 界面 MUST 明确说明「插件在启动时装载；这里看状态、控制触发，不做代码级配置」。

#### Scenario: 插件状态可见
- **WHEN** 打开插件分区
- **THEN** 看到插件 generation、状态、其工具/hook/触发器与诊断；能触发一个触发器并看到结果

## Requirement: 会话内模型选择

聊天界面 MUST 提供「本会话模型」选择器（provider/model/reasoning），选中后该会话后续每条输入
MUST 携带该 selection；会话选择 MUST 与租户默认选择分开呈现（会话当前 vs 默认）。

- 选择器选项 MUST 来自 `/v1/models`（只含凭据可解析、可执行的模型）。
- 会话选择 MUST NOT 修改租户文档（它是输入事实，不是配置写入）。
- 当会话没有选择时，输入不带 selection，服务端用租户默认。

#### Scenario: 会话切换模型
- **GIVEN** 会话当前默认（deepseek）
- **WHEN** 在头部选择 claude-sonnet-5 后发一条消息
- **THEN** 该条输入的 payload 带该 selection；`run.finished` 记录 s2api/claude-sonnet-5；下一条不带选择时回到默认

#### Scenario: 选择器只给能跑的
- **GIVEN** 进程只解析得到 s2api 的凭据
- **WHEN** 打开选择器
- **THEN** 只出现 s2api 的可执行模型（无凭据的 provider 不出现在选择器里）

## Requirement: 一致性与门禁

- 内核包 MUST NOT import `internal/modelcfg`（已有边界测试保持）。
- 既有 18 门验收与 R1 MUST 不退化；真实模型 E2E MUST 覆盖：写入→热更→新 run 用新模型→DELETE 回默认。
- 设置写入 MUST 只接受租户自身作用域（cookie 会话注入的租户；不得跨租户写入）。
