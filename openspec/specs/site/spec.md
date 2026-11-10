# site Specification

## Purpose
TBD - created by archiving change keel-site. Update Purpose after archive.

## Requirements

### Requirement: 站点与部署

站点 MUST 是静态构建（`site/`），构建产物 MUST 部署到 GitHub Pages 的自定义域
`https://keel.jixoai.com`（CNAME 已指向 `jixoai.github.io`），部署 MUST 由 GitHub Actions 自动完成
（push 到 main 触发，构建失败即不部署）。

#### Scenario: 构建成功
- **WHEN** 推送文档改动到 main
- **THEN** Actions 构建通过并发布；`https://keel.jixoai.com/` 返回 200 且页面为新内容

#### Scenario: 构建失败不发布
- **WHEN** 站点构建失败（断链/坏代码块/缺失页面）
- **THEN** 工作流失败且线上保持上一版（不留半成品）

### Requirement: 双语对等

站点 MUST 提供中文与英文两套完整导航与页面，两者 MUST 一一对应（同一路径、同一章节结构），
顶部 MUST 可切换语言且保持当前页面。

#### Scenario: 语言切换保位
- **GIVEN** 位于 `/zh/harness/plugins/lifecycle`
- **WHEN** 切换到 English
- **THEN** 落到 `/harness/plugins/lifecycle`（对应页），而不是首页

### Requirement: Core 主线（原理 + 二次开发）

Core 文档 MUST 覆盖：架构总览、事件账本与提交分类、内核与 PI loop、服务图与 loadout、handoff 与
背压、事件种类与 AgentAPI 契约、存储（PG/CAS/溢出）、以及**二次开发**：嵌入 runtime、写服务描述符、
接外部 backend、写 provider 适配器、测试与验收（含 `acceptance-all` 口径）。

#### Scenario: 二次开发可照做
- **WHEN** 开发者按「写一个服务描述符并接入 runtime」一页操作
- **THEN** 能跑通示例（命令与代码可复制），并在本地看到该服务被 ACTIVE

### Requirement: Harness 主线（架构 + 插件标准）

Harness 文档 MUST 覆盖：一次输入的完整路径（账本→claim→loop→工具→提交→投影）、流式与部分提交、
审批/问句、恢复与压缩、插件宿主；以及**插件开发标准**：清单 schema、三种 kind（in-process/sidecar/
webview）、生命周期与可逆副作用、权限与审计、UI 承载（webview+react+solid-glass）、测试与 conformance、
创造模式。

#### Scenario: 插件从零到可用
- **WHEN** 开发者按插件标准写一个 `webview` 插件
- **THEN** 文档给出的脚手架可直接安装并启用，插件 UI 在宿主中打开

### Requirement: 严谨性与可验证性

文档 MUST 与代码事实一致（关键断言带 `path:line` 或命令）；MUST NOT 出现已实现但不存在的说法
（如 `plugin.yaml` 若未实现则不得写成已实现）；术语 MUST 统一为 Harness（`AgentAPI`/`AgentBackend`
保留并注别名）。

#### Scenario: 事实对表
- **WHEN** 审查任一页的技术断言
- **THEN** 都能在仓库中找到对应实现或命令证据

### Requirement: 质量门

站点 MUST 通过：构建零错误、双语页面集合相等、死链检查、代码块 lint（Go/TS 可编译或可解析）、
部署后 smoke（首页/两条主线入口/插件标准页/语言切换均 200）。

#### Scenario: 死链被拦
- **WHEN** 页面引用不存在的路径
- **THEN** 构建/检查失败并指出来源页与目标
