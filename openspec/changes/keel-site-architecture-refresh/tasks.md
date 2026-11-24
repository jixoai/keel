# Tasks: keel-site-architecture-refresh

> 页面清单以 `site/docs/**` 为准（英文 30 页 + zh 镜像）。审计子代理产出的逐页差距清单
> 落到 `evidence/audit.md`，本任务表按主题分组执行；每组中英文同批改。

## 1. 审计与基线

- [ ] 1.1 逐页审计（旧架构叙述/旧版本命令/旧插件模型/缺失主题），产出
      `evidence/audit.md`（页面 | 优先级 | 问题 | 当前事实证据 | 建议）。
- [ ] 1.2 基线：当前四道门（links/parity/citations/EN-CJK）与构建结果记录到
      `evidence/baseline.txt`；线上关键页截图（before）到 `evidence/before/`。

## 2. Core 组（架构/事件/存储/二次开发）

- [ ] 2.1 `core/index`、`core/architecture`：进程/运行时模型、账本、多租户、placement/lease/
      handoff、loadout/assembly 的当前事实；删除单进程/有状态旧叙述。
- [ ] 2.2 `core/events`、`core/agentapi`：事件 kind 家族、SSE 游标/续传、Harness API 面、
      投影读取（含本轮的 external projection）与 v2 kind。
- [ ] 2.3 `core/storage`：PG 账本/CAS/溢出/RLS/配额；删除旧文件/内存描述。
- [ ] 2.4 `core/extend/*`（embed-runtime、external-backend、provider-adapter、
      service-descriptor、testing）：与当前 seam/装配一致；示例命令可复制。

## 3. Harness 组（一次输入/恢复/审批/运维）

- [ ] 3.1 `harness/index`、`harness/architecture`、`harness/anatomy-of-a-turn`：
      claim→loop→工具→提交→投影的当前路径；事件种类与账本口径。
- [ ] 3.2 `harness/streaming-and-partial`、`harness/recovery-and-compaction`：
      partial/断线续传、SIGKILL 恢复、checkpoint/压缩的当前语义。
- [ ] 3.3 `harness/approvals-and-questions`、`harness/mcp-apps`、`harness/operations`、
      `harness/creation-mode`：决策/问句、MCP apps、运维口径、创造模式的当前状态
      （Deferred 的必须写 Deferred，不写成已交付）。

## 4. 插件组（标准/生命周期/权限/UI/测试）

- [ ] 4.1 `harness/plugins/index`、`kinds`、`manifest`：plugin.yaml/扩展面/slots 贡献的
      当前契约；删除旧 kind 叙述；三种 kind 与四形态的对应关系说清。
- [ ] 4.2 `harness/plugins/lifecycle`、`permissions`、`testing`：安装/启停/卸载/HMR 回滚、
      权限与审计、conformance/负控的当前命令。
- [ ] 4.3 `harness/plugins/ui`：theme.json 令牌、槽位宿主、失败隔离、TUI 降级；
      与 `web/src/plugins/**` 的当前实现对齐。

## 5. 缺失的当前架构页面

- [ ] 5.1 决策：哪些主题需要新页（loadout/assembly、placement/lease、session-query/投影、
      slots/贡献、tenancy/RLS/配额、release/upgrade），哪些并入现有页。
- [ ] 5.2 新页/扩展节：每页带可复制命令与 `path:line`，中英文同批；导航与交叉链接更新。

## 6. 站内一致性与验收

- [ ] 6.1 下载/首页/术语：v0.2.0 资产、术语（Harness/Loadout/Placement/Slot/Bot/TeamBot）、
      交叉链接与导航一致。
- [ ] 6.2 四道门全绿：`check-links`、`parity`、`check-citations`、`check-en-cjk`；
      `pnpm --dir site build` 成功。
- [ ] 6.3 部署：site.yml 成功；关键页公网 200 且为新内容；after 截图到 `evidence/after/`。
- [ ] 6.4 vision 子代理复核关键页（四档/双端）；P0/P1 清零。
- [ ] 6.5 归档本 change（docs-only，`--skip-specs` 或按 site 规格合入）。
