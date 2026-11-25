# Tasks: keel-creation

## 1. 现状与租户化

- [ ] 1.1 审计现有 `keel.creation` catalog/scaffold/validate/install/remove 与 plugin manager
      的租户边界；列出部署级快照/双写点。
- [ ] 1.2 plugin install/state/audit 增加 tenant_id 过滤与唯一键；单租户=一个 tenant，
      不写第二套路径。
- [ ] 1.3 迁移/启动拒绝旧部署级状态，不保留双读；补跨租户负控。

## 2. Creation 闭环

- [ ] 2.1 catalog 输出已装插件/能力/槽位/extensibility，字段与装配事实同源。
- [ ] 2.2 scaffold 生成 `plugin.yaml` + `extensibility.json` + `EXTENSIBILITY.md` + chunk；
      校验 fail closed，字段级错误。
- [ ] 2.3 approval decision：展示 id/来源/hash/请求权限/贡献槽位；拒绝/超时 fail closed。
- [ ] 2.4 install/enable/render/list/uninstall 幂等；卸载撤销工具/槽位/hook/触发/临时状态。
- [ ] 2.5 审计/日志：tenant/plugin/action/result/阶段；脱敏。

## 3. Native chunk 与 UI

- [ ] 3.1 首个 chunk 插件端到端：AI 写 → 装 → 渲染 → 卸载零残留。
- [ ] 3.2 web 插件页/槽位五态；TUI/headless 不可用形态显式标注。
- [ ] 3.3 权限门：`plugin.invoke` / `plugin.lifecycle.*`；无权限 403/隐藏动作。

## 4. 验收

- [ ] 4.1 真 PG + 真实 UI E2E；每步截图；before/after 改动文件审计。
- [ ] 4.2 跨租户、审批拒绝、校验失败、重复 requestId、卸载 diff 为零。
- [ ] 4.3 `go test -race` 相关包 + web/TUI 回归；`openspec validate --strict`；独立 reviewer；
      HTML 报告公网链接；归档。
