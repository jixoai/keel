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

## U. 客户端（web/，NSH）

- [ ] U1 设置页：当前生效（source/revision/默认 selection）＋ provider 列表（不可执行原因）＋
      模型列表（能力/selectable/issues）＋默认模型与思考档编辑＋保存/重置＋字段级错误
- [ ] U2 会话内模型选择器（provider/model/reasoning）→ 之后每条输入带 selection；
      与租户默认分开显示；没有选择时不带字段
- [ ] U3 客户端类型/契约最小增量（wire.ts：settings 形状、selection 已在 input）
- [ ] U4 TUI 目标下同一份代码可编译（不要求完整交互，但不得破坏）

## Q. 验收

- [ ] Q1 真实模型 E2E：写入→热更→下一条输入用新模型（线路+run.finished 证明）→ DELETE 回默认
- [ ] Q2 校验门：坏文档 400 带字段路径；旧配置继续服务；revision 不变
- [ ] Q3 在飞 run 不受影响（进程内 + 说明）
- [ ] Q4 浏览器 E2E：设置页读/写/重置（截图），会话选择器切换模型并生效
- [ ] Q5 既有 18 门 + R1 不退化

## V. 复审与交付

- [ ] V1 子代理对抗复核（凭据/跨租户/热更一致性/UI 可用性），阻塞修完再复审
- [ ] V2 报告（HTML 图文）＋ 公网链接 ＋ archive
