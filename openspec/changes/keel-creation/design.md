# Design: keel-creation

## 1. 流程

```text
AI intent
  → creation.catalog（只读：已装插件/能力/槽位）
  → creation.scaffold（生成 plugin.yaml + 资源 + 校验提示）
  → creation.validate（fail closed，字段级错误）
  → approval（decision；风险/权限/来源可见）
  → install（租户级；幂等）
  → UI slot / tool surface 生效
  → creation.list / render
  → uninstall（disable → remove；零残留）
```

每一步都可重试、幂等、审计；失败不留下半安装状态。

## 2. 插件状态租户化

```text
plugin_install(tenant_id, plugin_id, version, state, manifest_hash)
plugin_audit(tenant_id, plugin_id, action, result, actor)
```

- 单租户部署只有一个 tenant，代码路径不分支；
- 列表/安装/启停/卸载都按 tenant 过滤；
- 系统内置插件可对所有租户可见，但用户创建/安装的插件不跨租户共享；
- 现有部署级快照若存在，迁移到租户视图，不保留双写。

## 3. Native chunk 验收

- 只做一个 `chunk` 形态：NSH 动态导入，同 VM；
- 插件包包含 `plugin.yaml`、`extensibility.json`、`EXTENSIBILITY.md`、前端 chunk；
- 安装后贡献一个 UI 槽位或工具；TUI 对不可用槽位显式标注；
- 卸载后：工具/槽位/hook/触发/状态/临时文件全部撤销，diff 为零；
- 不引入第二套插件运行时/DI 框架。

## 4. 权限与审批

- creation 工具：`plugin.invoke`；安装/更新/卸载：`plugin.lifecycle.*`；
- 安装前 decision 展示插件 id/来源/hash/请求权限/将贡献的槽位；
- 拒绝/超时 fail closed；不允许“先装再补审批”；
- 审计只记录 id/阶段/结果/权限摘要，不记录插件正文/密钥。

## 5. UI/UX

- 插件页：已安装、可发现、创建中、失败；字段级校验错误；
- 五态：加载/空/错误/无权限/成功；
- TUI/headless：不可用槽位显式说明，不假装渲染成功；
- web E2E：AI 写 → 安装 → 渲染 → 卸载；每步截图。

## 6. 兼容

- Alpha 零兼容：旧部署级 plugin_state 直接迁到租户视图；不保留双读。
- 现有 `keel.creation` 工具名/动作保留；新增租户字段与审批结果。
