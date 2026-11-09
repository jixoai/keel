# Proposal: keel-model-settings（模型配置的产品面）

## Why

模型配置的能力已经在内核与运行时齐了（身份、稀疏 overlay、有序规则、请求补丁、selection 作为输入事实），
但**产品面只有半条**：读得到（`/v1/providers`、`/v1/models`），写只能靠启动时的一份文件
（`KEEL_MODELS_CONFIG`／`--models-config`），改一次要重启；客户端没有设置页，会话里也不能选模型。
这等于把「模型可配」做成了运维脚本能力，而不是产品能力。

## What Changes

**一句话：把模型配置变成产品里可读、可写、可即时生效、可审计的一等能力，并把选择接进会话 UI。**

1. **写入路径**：新增租户级模型配置存储（`model_settings` + 历史表，revision 单调递增）。
   `GET/PUT/DELETE /v1/settings/models` 读写当前生效的模型文档；写入前用同一套 `modelcfg`
   解析校验（含默认 selection 可执行、凭据可解析），失败 400 并给字段路径；并发写用 revision 守卫（409）。
2. **热更生效（不重启）**：写入成功后立即 `Runtime.Reload` 装配新代数——新 run 用新配置，
   在飞 run 用旧代数跑完；reload 失败则拒绝写入并保持旧配置继续服务。
   为此把 HTTP 边缘的两处 boot 捕获缝（`ValidateSelection`、`ModelCatalog`）改为**按请求读取当前代数**。
3. **来源与优先级**：`GET` 返回 `source`（`stored` 覆盖 / `deployment` 部署默认＝启动文件）与 `revision`；
   `DELETE` 清除覆盖回到部署默认。启动时：有覆盖用覆盖，否则用启动文档。
4. **客户端设置页**：NSH 客户端新增设置页——当前生效状态（来源/revision/默认 selection）、
   provider 列表（可见/不可执行原因/凭据状态）、模型列表（能力事实 + issues + 可选性）、
   默认模型与思考档编辑、保存/重置、字段级错误呈现。
5. **会话内模型选择**：聊天头部新增「本会话模型」选择器（provider/model/reasoning），
   之后每条输入携带该 selection；与设置页的「租户默认」明确区分。选中项从 `/v1/models` 取，
   不复制端点/凭据。
6. **审计与可观测**：每次写入记录 revision/时间/来源与操作者会话；boot 日志与 loadout 摘要反映
   实际身份（已具备），生产日志不再出现 `agent=fake`。

## Impact

- 受影响规格：`model-settings`（新）、`provider-config`（写入/热更/边缘缝按代读取的约束）、
  `agentapi`（设置端点）。
- 受影响代码：`internal/store`（新表与方法）、`internal/httpapi`（设置路由、按请求读取的校验缝）、
  `internal/servicehost`（Reload 调用面、当前代读取）、`cmd/keel`（装配）、`web/`（设置页与会话选择器）。
- 不受影响：内核事件契约（不新增事件类型）；selection 作为输入事实的语义；扁平配置兼容路径。

## Non-Goals

- 不做多租户控制面（`keel serve` 仍是单租户本地面；写入按注入租户落库）。
- 不做 provider 密钥的新存储（`access.env`/`access.ref` 语义不变，凭据仍来自环境或外部密钥引用）。
- 不做隐藏 provider 的治理面（hidden 仍不从解析结果出现；设置页只管理可见项并说明）。

## Why now

Owner 2026-11-09：「我不要演示，我要完整的产品，而不是阉割版」——模型可配是产品能力，
必须能配、能即时生效、能看见、能审计。
