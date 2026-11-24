# Proposal: keel-site-architecture-refresh

> 立项：2026-11-24（v0.2.0 发布后，Owner 指出「官网还在聊旧的架构」）。
> 范围：`site/` 文档内容，不改产品代码。

## Why

官网（`site/docs/`，`https://keel.jixoai.com`）的大部分架构页写于 harness v1 时期：
叙述的是单进程/有状态运行时、旧的插件结构、旧的恢复与数据模型；而 v0.2.0 之后的真实形态是
Go 无状态运行时 + PostgreSQL 事件账本 + 多租户/placement/lease/handoff + loadout/assembly +
能力面插件 + SIGKILL 恢复/压缩 checkpoint + 投影 API。问题不是「个别版本号」：

- 新用户按官网理解会得到错误的心智模型（例如以为会话在进程内存、重启即丢）；
- 集成者会按旧命令/旧 manifest 写插件，装不上；
- 下载页已切 v0.2.0，但架构页仍停在 v0.1.0 的世界，站内自相矛盾。

## What Changes

1. **审计驱动的改写**：逐页对表当前代码/规格，产出「已交付事实 → 页面」的差距清单；
   按 P1（误导）/P2（过时）/P3（打磨）分级，中英文同批改。
2. **旧架构叙述下线**：单进程/有状态、旧插件 kind、旧恢复/存储描述、旧 `nsh` 版本指令、
   旧下载/构建命令，一律替换为当前事实（带 `path:line` 或命令证据）。
3. **补齐当前架构页面**：loadout/assembly（四层装配）、placement/lease/handoff、
   session-query/投影、plugin slots/贡献、tenancy/RLS/配额、release/upgrade
   ——只补官网缺失且用户/集成者需要的，不复制内部设计文档。
4. **站内一致性**：导航/交叉链接/术语（Harness、Loadout、Placement、Slot、Bot/TeamBot）统一；
   下载页与 v0.2.0 资产一致；`site/scripts` 的四道门（死链、双语对等、引用、EN-CJK）全绿。
5. **验收**：构建 + 部署成功；关键页面（架构总览、harness 总览、插件标准、下载页）
   经 vision 子代理四档/双端视觉复核；引用门 100% 命中。

## Impact

- 仅 `site/docs/**`、必要的导航配置与 `site/scripts` 证据；不改 Go/web 产品代码。
- 部署走既有 `.github/workflows/site.yml`（push main → GitHub Pages）。
- 规格：`site` 增「架构事实新鲜度」要求（主线归档/大版本发布后页面必须对表）。
