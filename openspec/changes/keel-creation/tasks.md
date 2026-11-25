# Tasks: keel-creation

> 第一阶段（2026-11-25，`task/keel-creation`）：后端闭环 + 租户边界已落；真实浏览器 E2E
> 截图/公网报告/独立 reviewer/归档仍待第二阶段。

## 1. 现状与租户化

- [x] 1.1 审计现有 `keel.creation` catalog/scaffold/validate/install/remove 与 plugin manager
      的租户边界；列出部署级快照/双写点。
  - 结论：`plugins` 原为部署级 `id` 主键、manager 无 tenant、`plugin_audit` 可读
    `tenant_id IS NULL`；creation/API/plugin-UI/bridge 均未按请求 tenant 过滤。
- [x] 1.2 plugin install/state/audit 增加 tenant_id 过滤与唯一键；单租户=一个 tenant，
      不写第二套路径。
  - `plugins(tenant_id,id)` PK、`plugin_audit.tenant_id NOT NULL`；Manager 绑定
    `ManagerOptions.TenantID`；单租户部署就是这一个 tenant，无部署级分支。
- [x] 1.3 迁移/启动拒绝旧部署级状态，不保留双读；补跨租户负控。
  - schema 尾部迁移：空旧表原地升级；仍有部署级行的旧表启动即 `RAISE EXCEPTION`；
    旧 NULL-tenant 审计行删除后列收紧为 NOT NULL。负控见
    `TestPluginManagerTenantIsolation`、`TestCreationRefusesAnotherTenant`、
    `TestPluginAPIRefusesAnotherTenant`、`TestPluginUIHostsEnabledPluginOnly`（foreign tenant 404）。

## 2. Creation 闭环

- [x] 2.1 catalog 输出已装插件/能力/槽位/extensibility，字段与装配事实同源。
  - catalog 直接投影 `PluginView`（provides/permissions/requires/layer/slots/contributes/extensibility）。
- [x] 2.2 scaffold 生成 `plugin.yaml` + `extensibility.json` + `EXTENSIBILITY.md` + chunk；
      校验 fail closed，字段级错误。
  - 模板新增 `extensibility.json`、`EXTENSIBILITY.md`、`ui/settings.js`（Native chunk），
    `plugin.yaml` 声明 `contributes` + `extensibility`；`TestRenderProducesAnInstallablePackage`。
- [x] 2.3 approval decision：展示 id/来源/hash/请求权限/贡献槽位；拒绝/超时 fail closed。
  - `installationPrompt` 渲染 id/version/source/hash/permissions/slots；复用现有 decision seam，
    拒绝即不安装并写 `refused` 审计。
- [x] 2.4 install/enable/render/list/uninstall 幂等；卸载撤销工具/槽位/hook/触发/临时状态。
  - 同包 install/同 requestId 重放返回 `idempotent:true`，不写第二行；enable 已启用幂等；
    remove 后 row/snapshot/tool/staging 均无残留（`TestCreationScaffoldValidateInstallListRemove`）。
- [x] 2.5 审计/日志：tenant/plugin/action/result/阶段；脱敏。
  - Manager 的 install/enable/disable/uninstall/update/boot/discover/denial 行全部写 manager tenant；
    creation 行同样带 tenant；detail 只含 id/path/hash/权限名/槽位，不含正文或密钥。

## 3. Native chunk 与 UI

- [x] 3.1 首个 chunk 插件端到端：AI 写 → 装 → 渲染 metadata → 卸载零残留。
  - 后端闭环测试 `TestCreationScaffoldValidateInstallListRemove`；chunk-only 托管
    `TestPluginUIHostsDeclaredChunkEntries`；web 槽位同步沿用 W12 `syncPluginInventory`。
- [x] 3.2 web 插件页/槽位五态；TUI/headless 不可用形态显式标注。
  - 插件页 loading/empty/error/no-permission/success；终端显示
    「只渲染 chunk/data；页面/iframe/package 需浏览器」；槽位沿用 loading/failure/unavailable。
- [x] 3.3 权限门：`plugin.invoke` / `plugin.lifecycle.*`；无权限 403/隐藏动作。
  - HTTP matrix 已有 key；creation 工具经 `plugin.invoke`；UI `can()` 隐藏入口并显示缺权提示。

## 4. 验收

- [ ] 4.1 真 PG + 真实 UI E2E；每步截图；before/after 改动文件审计。
  - 第一阶段已跑真 PG store 测试与 `nsh test --target tui`，但未产出浏览器逐步截图/HTML 报告。
- [x] 4.2 跨租户、审批拒绝、校验失败、重复 requestId、卸载 diff 为零。
  - 上述测试名即证据；另 `TestCreationValidateReportsFieldErrors`、
    `TestCreationInstallRequiresTheApprovalPrimitive`。
- [ ] 4.3 `go test -race` 相关包 + web/TUI 回归；`openspec validate --strict`；独立 reviewer；
      HTML 报告公网链接；归档。
  - 已完成：`go test -race`（harnessext/harnessapi/cmd/keel/pluginpkg/architecture/fileplugin/
    store）、`nsh check`、`nsh test --target tui`（409 passed）、`openspec validate keel-creation --strict`。
  - 未完成：独立 reviewer、HTML 报告公网链接、archive。
