# Proposal: keel-site（官网 keel.jixoai.com：中英双语 Core + Harness 文档）

## Why

keel 已开源（`jixoai/keel`），但没有对外官网与开发者文档：Core 的原理与二次开发路径、Harness 的
架构与**插件开发标准**都只散落在 `docs/*.md` 与归档的 openspec 里；`docs/AGENT.md:65` 的
`plugin.yaml` 说法甚至与实现不符。Owner 要求：**keel.jixoai.com**，中英双语，严谨客观详细地讲清
两条主线——(a) Core 的原理与二次开发；(b) Harness（原 Agent，统一改名）的架构、工作原理与插件开发标准。

DNS 事实：`keel.jixoai.com` 已 CNAME 到 `jixoai.github.io`；`jixoai/keel` 尚未配置 Pages。
Owner 有该仓 admin，可直接启用 Pages（CNAME=keel.jixoai.com）。

## What Changes

1. **站点**：仓库新增 `site/`（VitePress：静态、双语 locale、代码组、本地搜索、暗色主题对齐产品令牌），
   构建产物进 GitHub Pages（Actions workflow），自定义域 `keel.jixoai.com`。
2. **信息架构**（两种语言完全对等）：
   - **Core**：总览 → 架构（账本/内核/服务图/loadout/handoff）→ 事件与协议（事件种类、AgentAPI、
     幂等与提交分类）→ 存储与运行（PG/CAS/背压/溢出）→ **二次开发**（嵌入 runtime、写服务描述符、
     外部 backend、provider 适配、测试与验收）。
   - **Harness**：总览 → 架构（会话/回合/工具/审批/插件宿主）→ 工作原理（一次输入的完整路径、
     流式与部分提交、压缩、恢复）→ **插件开发标准**（清单 schema、三种 kind、生命周期、权限与审计、
     UI 承载 webview+react+solid-glass、测试与 conformance）→ 创造模式 → 运维（serve/TUI/部署）。
3. **可执行文档**：每条命令/代码块可复制可运行；关键流程配时序图与截图；版本化（与 release tag 对齐）。
4. **术语**：对外统一 **Harness**（旧称 Agent）；协议名 `AgentAPI`/`AgentBackend` 保留并给
   Harness 别名说明。术语表独立页。
5. **质量门**：`site` 构建零警告；双语导航/页面一一对应；死链检查；代码块 lint；
   Pages 部署后 smoke（HTTP 200 + 关键页可达 + 语言切换）。

## Non-Goals

- 不做动态后端/账号系统（静态站；需要交互的表单留后续）。
- 不翻译全部内部文档（只翻译面向开发者的两条主线 + 术语表/快速开始）。
- 不做博客/版本发布页（后续）。

## Impact

`site/`（新）、`.github/workflows/site.yml`（新）、`README.md`/`docs/*` 的术语与链接对齐；
Pages 配置（CNAME）。仓库代码不变。
