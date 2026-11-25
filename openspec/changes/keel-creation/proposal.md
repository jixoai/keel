# Proposal: keel-creation

> 立项：2026-11-25。来源：确认 creation 是必须能力；现有 `keel.creation` 已有 catalog/
> scaffold/validate/install/remove，但“AI 现场写 Native 插件 → 装 → 渲染 → 卸载无残留”的
> G8 端到端仍 Deferred。
> 定位：creation 是“让 AI 现场创建插件”的产品能力，不是代码生成演示，也不是 marketplace。

## Why

`keel.creation` 已有只读发现、脚手架、校验、安装/删除工具，但当前缺口是端到端产品闭环：

- AI 写出的插件是否真的能被装进当前租户并渲染；
- 安装是否需要审批、失败是否 fail closed、字段级错误是否可见；
- 卸载后是否零残留（工具、hook、UI 槽位、审计、状态）；
- TUI/headless 对不可用形态是否显式降级；
- 插件状态是否按租户隔离，能否在单租户/私有部署中复用同一代码路径。

本 change 把 creation 做成可验收的端到端能力，先做一个 Native chunk 插件，不铺插件市场。

## What Changes

1. **Creation 流程**：AI 通过 `creation` 工具发现 extensibility → 生成插件包 → 校验 →
   审批 → 安装 → 渲染 → 列举/删除；每步有结构化结果与字段级错误。
2. **Native chunk 闭环**：首个验收形态为 `chunk`（NSH 动态导入，同 VM）；装后出现在 UI
   槽位/工具面，卸载后零残留；`package`/`iframe`/`data` 四形态按现有边界，不在本 change 全铺。
3. **租户隔离**：创建的插件安装/状态 MUST 按 tenant_id 存储；单租户部署 = 一个 tenant，
   不另写一套代码路径。安装/更新/卸载不得跨租户。
4. **审批与权限**：creation/install 需要 `plugin.invoke`/`plugin.lifecycle` 与 decision
   审批；拒绝/超时 fail closed；审计不含插件正文/密钥。
5. **UI/降级**：web 渲染 chunk；TUI/headless 对不可用形态显式标注；五态（加载/空/错/
   无权限/成功）必须有设计。
6. **可观测**：创建/校验/安装/卸载的结构化日志与审计；失败可追踪到字段/阶段。

## Capabilities

### New Capabilities

- `creation-mode`: AI 现场创建 Native 插件的发现、脚手架、校验、审批、租户级安装、
  渲染和零残留卸载。

### Modified Capabilities

<!-- 若实现需要修改 plugin-system/plugin-ui-extension 的既有 requirement，
     实现阶段补精确 delta；本提案不虚构修改。 -->

## Impact

- `internal/agentext`/`internal/pluginpkg`/`internal/harnessext`/`internal/toolsurface`；
  web/TUI 插件安装/渲染面；`plugin_state` 租户化（若当前仍是部署级）。
- 复用现有 `keel.creation` 工具与插件生命周期；不新建第二套插件框架。
- 测试：真实 UI E2E、权限/审批、租户隔离、卸载 diff、TUI 降级。
- 不涉及 macOS 公证/特权 helper；特殊权限由外置程序实现。

## Out of scope

- 插件 marketplace、跨租户共享、计费。
- 一次铺满 42 个 W12 插件；只做 creation 所需的 Native chunk 验收。
- 让 AI 直接执行未审批的高权限插件代码。
