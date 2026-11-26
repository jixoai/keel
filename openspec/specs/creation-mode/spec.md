# creation-mode Specification

## Purpose
TBD - created by archiving change keel-creation. Update Purpose after archive.

## Requirements

### Requirement: AI 创建插件的完整闭环

系统 SHALL 提供 creation 流程：只读发现 extensibility、生成插件包、校验、审批、安装、
列举/渲染、禁用/卸载。每一步 MUST 返回结构化结果；校验失败 MUST fail closed 并给出字段级
错误；安装/卸载 MUST 幂等，重复 requestId 不产生第二份状态。

#### Scenario: 从意图到安装
- **WHEN** AI 根据已装插件目录创建一个 Native chunk 插件包
- **THEN** 校验通过、审批通过后安装成功，插件贡献的 UI/工具可见

#### Scenario: 字段错误
- **WHEN** `plugin.yaml` 缺少必填字段或 manifest 非法
- **THEN** 校验失败，返回字段路径与原因，不进入安装

### Requirement: 租户级插件状态

用户创建/安装的插件状态 MUST 按 tenant_id 隔离；列表/安装/启停/卸载 MUST 按当前 tenant
过滤；跨租户安装/读取/卸载 MUST 返回 404/403。单租户部署 MUST 只使用一个 tenant，不另写
代码路径。MUST NOT 保留部署级与租户级双写。

#### Scenario: 跨租户不可见
- **WHEN** 租户 A 安装的插件被租户 B 查询/卸载
- **THEN** 返回 404/403，且不改变 A 的状态

#### Scenario: 单租户复用
- **WHEN** 私有部署只有一个 tenant
- **THEN** 使用同一套租户级安装/渲染/卸载路径

### Requirement: 审批与权限

creation 调用 MUST 需要 `plugin.invoke`；安装/更新/卸载 MUST 需要对应 `plugin.lifecycle.*`。
安装前 MUST 通过 decision 展示插件 id、来源、hash、请求权限和将贡献的槽位；拒绝/超时 MUST
fail closed，MUST NOT 先安装后补审批。审计 MUST 不含插件正文/密钥。

#### Scenario: 拒绝安装
- **WHEN** 审批人拒绝或超时
- **THEN** 插件不安装、不注册工具/槽位，审计记录拒绝原因

### Requirement: Native chunk 渲染与零残留卸载

首版 MUST 验收一个 `chunk` 形态插件：安装后能渲染贡献的 UI 槽位或工具；TUI/headless 对
不可用形态 MUST 显式标注，不得假装成功。卸载 MUST 撤销工具、槽位、hook、触发和临时状态，
MUST 产生零残留 diff。

#### Scenario: 安装后渲染
- **WHEN** chunk 插件安装并启用
- **THEN** web 对应槽位渲染插件内容；工具面按声明出现

#### Scenario: 卸载零残留
- **WHEN** 禁用并卸载插件
- **THEN** 工具/槽位/hook/触发/临时文件全部消失，重复查询返回 not installed

### Requirement: 五态与可观测

创建/安装/渲染界面 MUST 覆盖加载、空、错误、无权限、成功五态；创建/校验/安装/卸载 MUST
写结构化审计与日志，包含 tenant/plugin/action/result/阶段，MUST NOT 包含插件正文或密钥。

#### Scenario: 无权限
- **WHEN** 没有 `plugin.lifecycle.install` 的账号打开创建/安装入口
- **THEN** UI 显示无权限态，后端返回 403，不泄露插件内容

### Requirement: 不引入第二套插件框架

creation MUST 复用现有 pluginpkg/harnessext/plugin manager；MUST NOT 新增第二套插件运行时、
DI 框架或 marketplace。特殊权限/提权 MUST 由外置程序实现，keel MUST NOT 内置特权 helper。

#### Scenario: 复用生命周期
- **WHEN** creation 安装插件
- **THEN** 走现有 install/enable/disable/uninstall 生命周期与审计
