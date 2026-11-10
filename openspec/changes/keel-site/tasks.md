# Tasks: keel-site

> S=站点骨架 · C=Core 文档 · H=Harness 文档 · I=双语/术语 · Q=质量门 · D=部署 · V=复核。

## S. 站点骨架

- [ ] S1 `site/`（VitePress）+ 双语 locale + 暗色主题（对齐产品令牌）+ 本地搜索
- [ ] S2 导航/侧栏结构（Core 与 Harness 两条主线）+ 首页（定位、两条主线入口、快速开始）
- [ ] S3 构建脚本与 `.github/workflows/site.yml`（build → Pages）

## C. Core 文档

- [ ] C1 总览 + 架构（账本/内核/服务图/loadout/handoff/背压）
- [ ] C2 事件与协议（事件种类、提交分类、幂等、AgentAPI 参考——从归档 spec 提炼）
- [ ] C3 存储与运行（PG/CAS/溢出/资源预算）
- [ ] C4 二次开发（嵌入 runtime、服务描述符、外部 backend、provider 适配、测试与验收）

## H. Harness 文档

- [ ] H1 总览 + 架构（会话/回合/工具/审批/插件宿主）
- [ ] H2 工作原理（一次输入的完整路径、流式、恢复、压缩）
- [ ] H3 插件开发标准（清单、三种 kind、生命周期、权限/审计、UI 承载、测试/conformance）
- [ ] H4 创造模式（如何用、如何扩展、产物管理）
- [ ] H5 运维（serve/TUI/部署/诊断）

## I. 双语与术语

- [ ] I1 全部页面双语对等（同路径、同结构）
- [ ] I2 术语表页（Harness/Agent 别名、AgentAPI/AgentBackend、selection、loadout、run/handoff…）
- [ ] I3 术语清扫（对外文档与用户可见串统一 Harness；见 `keel-harness-naming`）

## Q. 质量门

- [ ] Q1 构建零错误；双语页面集合相等（脚本断言）
- [ ] Q2 死链检查（内部链接 + 图片）
- [ ] Q3 代码块 lint（Go/TS/YAML 至少可解析；命令块与仓库脚本对表）
- [ ] Q4 事实对表（关键断言带 `path:line`/命令）

## D. 部署

- [ ] D1 启用 GitHub Pages（CNAME=keel.jixoai.com）并验证 HTTPS
- [ ] D2 部署后 smoke：首页 / core / harness / plugins / zh 切换均 200

## V. 复核

- [ ] V1 子代理对抗复核（事实是否与代码一致、双语是否对等、是否还有 Agent 残留）
- [ ] V2 报告 + 归档
