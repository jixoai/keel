# Tasks: keel-model-settings

> S=存储 · A=API · R=热更/装配 · U=客户端 · Q=验收 · V=复审/报告。

## S. 存储（internal/store）

- [x] S1 `model_settings` 表（tenant_id PK, revision, document jsonb, updated_at）＋历史表
      `model_settings_history`（revision, document, source, created_at），boot 期幂等 DDL
      （schema.sql；`internal/store/model_settings_test.go` 断言幂等与形状）
- [x] S2 方法：`GetModelSettings` / `SetModelSettings`（revision 单调 +1，写历史）/
      `DeleteModelSettings`（删覆盖并写一条 history）/ 读取当前 revision（advisory lock 串行化写入）
- [x] S3 单测：读写/历史/revision 单调/跨租户隔离/并发 expectedRevision（`model_settings_test.go`）

## A. API（internal/httpapi + cmd/keel）

- [x] A1 `GET/PUT/DELETE /v1/settings/models`（cookie 会话 + 租户注入；不引入第二套鉴权）
- [x] A2 PUT：modelcfg 校验（含默认 selection 可执行）→ store → Reload → 200 摘要；
      校验失败 400 带 `issues`；revision 冲突 409；Reload 失败拒绝写入并保持旧配置
      （拒绝时 `restore(ctx)` 让旧代数继续服务；已实证：拒绝写入后 worker 仍能领取新任务）
- [x] A3 单测：形状/字段路径/409/Reload 拒绝/跨租户不可写（`internal/agentapi/settings_test.go`）

## R. 热更与边缘缝（internal/servicehost + cmd/keel）

- [x] R1 `Runtime.Reload` 接入设置写路径（用当前文档 + 新 Models 段），并保证失败不 adopt
- [x] R2 HTTP 边缘缝按请求读取当前代数（`ValidateSelection`、`ModelCatalog`），移除 boot 捕获
      （`ModelsLive` 闭包按请求取当前 generation）
- [x] R3 进程内测试：写入→新 run 用新身份、在飞 run 用旧代数、DELETE 回默认
      （`ApplyModels` 测试；关键修复：装配上下文不得跟随 HTTP 请求——`ApplyModels` 用
      `context.WithoutCancel(ctx)`，激活看门狗否则会在请求结束时取消领取，
      `applymodels_worker_test.go` 在修复前必失败）

## K. 密钥存储（internal/store + internal/servicehost + cmd/keel）

- [x] K1 `tenant_secrets(tenant_id, name, ciphertext bytea, created_at, updated_at)`（幂等 DDL）＋
      方法 Get/List/Put/Delete（值用既有 `store.SecretBox` 加密，AAD 绑 tenant+name；列表不含值）
- [x] K2 `GET/PUT/DELETE /v1/settings/secrets[/{name}]`：值永不回读；未配置 `KEEL_SECRET_KEY` 时 503 + 原因
- [x] K3 modelcfg 解析：`secret:NAME` → 密钥存储（`env:NAME`/`${NAME}` 仍查环境）；
      校验期也能判定 ref 可解析（保存配置时不误报）；密钥名文法与引用文法等宽
      （`^[A-Za-z_][A-Za-z0-9_]{0,63}$`，含单测）
- [x] K4 单测：加密落盘/不回读/跨租户隔离/未配置密钥盒 fail-closed/secret 引用可解析（`secrets_test.go`）

## P. 插件清单（internal/agentapi）

- [x] P1 `GET /v1/settings/plugins`（或复用 `/v1/plugins` 并补 triggers/fire）：generation/状态/tools/hooks/
      triggers/diagnostics；`POST /v1/triggers/{id}/fire` 已有，补齐返回体
- [x] P2 单测：未接线 501、快照直出、触发结果

## U. 客户端（web/，NSH）

- [x] U1 **独立设置页**：`/settings` 路由（深链直达），从工作区入口进入/返回；分区导航 模型/密钥/插件；
      工作区不动（App.tsx 路由壳；host.js 的 `keel.route`/`keel.navigate` 把 URL 与路由同步）
- [x] U2 模型分区：当前生效（source/revision/默认 selection）＋ provider 列表（不可执行原因）＋
      模型列表（能力/selectable/issues）＋默认模型与思考档编辑＋原始文档编辑＋保存/重置＋字段级错误
- [x] U3 密钥分区：写入/覆盖/删除（值只输入不回显）、列表只有名字与时间、未启用时说明原因
- [x] U4 插件分区：清单/状态/工具/hook/触发器/诊断 + 手动触发并显示结果 + 装载语义说明
- [x] U5 会话内模型选择器（provider/model/reasoning）→ 之后每条输入带 selection；
      与租户默认分开显示；没有选择时不带字段
- [x] U6 客户端类型/契约增量（wire.ts/contract.ts：settings 形状、secrets、plugins、selection）
- [x] U7 TUI 目标下同一份代码可编译（`nsh check` + `nsh test --target tui` 6/6，含设置页与选择器的 TUI 用例）
- [x] U8 数据源真值：fixture 态显式标注「演示数据 · 未连接服务」＋「重试连接」探针（设置页不再把
      fixture 数据伪装成产品状态；`DataSourceStrip`）

## Q. 验收

- [x] Q1 真实模型 E2E：写入→热更→下一条输入用新模型（线路+run.finished 证明）→ DELETE 回默认
      （`scripts/real-model-e2e.sh --settings`，21 checks 全绿；含录制代理对线路的逐字断言）
- [x] Q2 校验门：坏文档 400 带字段路径；旧配置继续服务；revision 不变（E2E + 单测）
- [x] Q3 在飞 run 不受影响（进程内 + 说明；`applymodels_worker_test.go` 的激活上下文用例）
- [x] Q4 浏览器 E2E：设置页读/写/重置（截图），会话选择器切换模型并生效
      （三区走查 + UI 写入密钥 + 选择器 chip；截图见 2026-11-09-provider-config 报告与打磨报告）
- [x] Q5 既有 18 门 + R1 不退化（复审后统一复跑：16/16 ALL GATES PASS，perf 523.8MB / 双进程 522.7MB）

## V. 复审与交付

- [x] V1 子代理对抗复核（凭据/跨租户/热更一致性/UI 可用性）：运行时/设置面 7/10、客户端/真值面 8/10；
      阻塞项「启动回退防砖化」已修并 scratch 真机复验（health 200 + issues[storedOverride] + 日志两条），
      非阻塞项（retry 复验 selection、引用文法限长、设置写串行化、文档 selection 词表、回滚日志）全部落地
- [x] V2 报告（HTML 图文）＋ 公网链接 ＋ archive
      （https://local-gaubee-keel-polish.fde2e.com/report.html · 2026-11-10 归档）
