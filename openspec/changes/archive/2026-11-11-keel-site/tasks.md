# Tasks: keel-site

> S=站点骨架 · C=Core 文档 · H=Harness 文档 · I=双语/术语 · Q=质量门 · D=部署 · V=复核。

## S. 站点骨架

- [x] S1 `site/`（VitePress）+ 双语 locale + 暗色主题（对齐产品令牌）+ 本地搜索
- [x] S2 导航/侧栏结构（Core 与 Harness 两条主线）+ 首页（定位、两条主线入口、快速开始）
- [x] S3 构建脚本与 `.github/workflows/site.yml`（build → Pages）

## C. Core 文档

- [x] C1 总览 + 架构（账本/内核/服务图/loadout/handoff/背压）
- [x] C2 事件与协议（事件种类、提交分类、幂等、AgentAPI 参考——从归档 spec 提炼）
- [x] C3 存储与运行（PG/CAS/溢出/资源预算）
- [x] C4 二次开发（嵌入 runtime、服务描述符、外部 backend、provider 适配、测试与验收）

## H. Harness 文档

- [x] H1 总览 + 架构（会话/回合/工具/审批/插件宿主）
- [x] H2 工作原理（一次输入的完整路径、流式、恢复、压缩）
- [x] H3 插件开发标准（清单、三种 kind、生命周期、权限/审计、UI 承载、测试/conformance）
- [x] H4 创造模式（如何用、如何扩展、产物管理）
- [x] H5 运维（serve/TUI/部署/诊断）

## I. 双语与术语

- [x] I1 全部页面双语对等（同路径、同结构）
- [x] I2 术语表页（Harness/Agent 别名、AgentAPI/AgentBackend、selection、loadout、run/handoff…）
- [x] I3 术语清扫（对外文档与用户可见串统一 Harness；见 `keel-harness-naming`）

## Q. 质量门

- [x] Q1 构建零错误；双语页面集合相等（脚本断言）
- [x] Q2 死链检查（内部链接 + 图片）
- [x] Q3 代码块 lint（Go/TS/YAML 至少可解析；命令块与仓库脚本对表）
- [x] Q4 事实对表（关键断言带 `path:line`/命令）

## D. 部署

- [x] D1 启用 GitHub Pages（CNAME=keel.jixoai.com）并验证 HTTPS
- [x] D2 部署后 smoke：首页 / core / harness / plugins / zh 切换均 200

## V. 复核

- [x] V1 子代理对抗复核（事实是否与代码一致、双语是否对等、是否还有 Agent 残留）
- [x] V2 报告 + 归档

## 已登记边界（交付时核实）

- 视觉复核首轮 HOLD 6.5，五条 major（浅色漏出/表格截断/英文页中文/中文 404/代码块硬切）与八条 minor 已全部修复并逐条 before/after 复验；站点已部署到 https://keel.jixoai.com（Pages + CI 绿）。
- 未覆盖：中文站搜索（只验了英文 "loadout"）、390 下的搜索结果页、真实触屏设备（headless 模拟视口）、键盘焦点序/读屏树、浅色主题（已删除该入口）、404 的 390 布局。
- 已知诚实缺口（站内如实标注为「规划中」）：Provider 文件/图片协议未实现；sidecar SIGKILL 清扫、per-plugin 资源预算未在 Go 侧实施。
