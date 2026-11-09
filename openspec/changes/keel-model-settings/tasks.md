# Tasks: keel-model-settings

> S=存储 · A=API · R=热更/装配 · U=客户端 · Q=验收 · V=复审/报告。

## S. 存储（internal/store）

- [ ] S1 `model_settings` 表（tenant_id PK, revision, document jsonb, updated_at）＋历史表
      `model_settings_history`（revision, document, source, created_at），boot 期幂等 DDL
- [ ] S2 方法：`GetModelSettings` / `SetModelSettings`（revision 单调 +1，写历史）/
      `DeleteModelSettings`（删覆盖并写一条 history）/ 读取当前 revision
- [ ] S3 单测：读写/历史/revision 单调/跨租户隔离/并发 expectedRevision

## A. API（internal/httpapi + cmd/keel）

- [ ] A1 `GET/PUT/DELETE /v1/settings/models`（cookie 会话 + 租户注入；不引入第二套鉴权）
- [ ] A2 PUT：modelcfg 校验（含默认 selection 可执行）→ store → Reload → 200 摘要；
      校验失败 400 带 `issues`；revision 冲突 409；Reload 失败拒绝写入并保持旧配置
- [ ] A3 单测：形状/字段路径/409/Reload 拒绝/跨租户不可写

## R. 热更与边缘缝（internal/servicehost + cmd/keel）

- [ ] R1 `Runtime.Reload` 接入设置写路径（用当前文档 + 新 Models 段），并保证失败不 adopt
- [ ] R2 HTTP 边缘缝按请求读取当前代数（`ValidateSelection`、`ModelCatalog`），移除 boot 捕获
- [ ] R3 进程内测试：写入→新 run 用新身份、在飞 run 用旧代数、DELETE 回默认

## K. 密钥存储（internal/store + internal/servicehost + cmd/keel）

- [ ] K1 `tenant_secrets(tenant_id, name, ciphertext bytea, created_at, updated_at)`（幂等 DDL）＋
      方法 Get/List/Put/Delete（值用既有 `store.SecretBox` 加密；列表不带与值有关的任何信息）
- [ ] K2 `GET/PUT/DELETE /v1/settings/secrets[/{name}]`：值永不回读；未配置 `KEEL_SECRET_KEY` 时 503 + 原因
- [ ] K3 modelcfg 解析：`secret:NAME` → 密钥存储（`env:NAME`/`${NAME}` 仍查环境）；
      校验期也要能判定 ref 可解析（保存配置时不误报）
- [ ] K4 单测：加密落盘/不回读/跨租户隔离/未配置密钥盒 fail-closed/secret 引用可解析

## P. 插件清单（internal/agentapi）

- [ ] P1 `GET /v1/settings/plugins`（或复用 `/v1/plugins` 并补 triggers/fire）：generation/状态/tools/hooks/
      triggers/diagnostics；`POST /v1/triggers/{id}/fire` 已有，补齐返回体
- [ ] P2 单测：未接线 501、快照直出、触发结果

## U. 客户端（web/，NSH）

- [ ] U1 **独立设置页**：`/settings` 路由（深链直达），从工作区入口进入/返回；分区导航 模型/密钥/插件；
      工作区不动
- [ ] U2 模型分区：当前生效（source/revision/默认 selection）＋ provider 列表（不可执行原因）＋
      模型列表（能力/selectable/issues）＋默认模型与思考档编辑＋原始文档编辑＋保存/重置＋字段级错误
- [ ] U3 密钥分区：写入/覆盖/删除（值只输入不回显）、列表只有名字与时间、未启用时说明原因
- [ ] U4 插件分区：清单/状态/工具/hook/触发器/诊断 + 手动触发并显示结果 + 装载语义说明
- [ ] U5 会话内模型选择器（provider/model/reasoning）→ 之后每条输入带 selection；
      与租户默认分开显示；没有选择时不带字段
- [ ] U6 客户端类型/契约增量（wire.ts/contract.ts：settings 形状、secrets、plugins、selection）
- [ ] U7 TUI 目标下同一份代码可编译（不要求完整交互，但不得破坏）

## Q. 验收

- [ ] Q1 真实模型 E2E：写入→热更→下一条输入用新模型（线路+run.finished 证明）→ DELETE 回默认
- [ ] Q2 校验门：坏文档 400 带字段路径；旧配置继续服务；revision 不变
- [ ] Q3 在飞 run 不受影响（进程内 + 说明）
- [ ] Q4 浏览器 E2E：设置页读/写/重置（截图），会话选择器切换模型并生效
- [ ] Q5 既有 18 门 + R1 不退化

## V. 复审与交付

- [ ] V1 子代理对抗复核（凭据/跨租户/热更一致性/UI 可用性），阻塞修完再复审
- [ ] V2 报告（HTML 图文）＋ 公网链接 ＋ archive
