# Tasks: keel-plugin-core

> M=清单/包 · L=生命周期/API · S=sidecar · W=WebView · P=权限/审批 · C=创造模式 · T=TUI · O=官方套件 · Q=验收 · R=复核。

## M. 清单与包

- [ ] M1 `internal/pluginpkg`：`plugin.yaml` schema + 校验（id 文法、semver、kind、provides 形状、
      权限形状、engines 区间）+ 字段级错误
- [ ] M2 发现：`<plugins-root>/<id>/plugin.yaml` 扫描、冲突（同 id 多版本）语义、损坏清单隔离
- [ ] M3 版本门：`engines.keel` 与当前版本比对（安装 + 启用双门）
- [ ] M4 包 hash 与受管存储（只允许 plugins-root 内；拒绝符号链接外逃）

## L. 生命周期与 API

- [ ] L1 状态机 installed→enabled→disabled→uninstalled（+failed），与 agentext generation 对齐
- [ ] L2 `GET /v1/plugins`（清单+状态+提供物）、`POST /v1/plugins/{id}/enable|disable|uninstall|update`、
      `POST /v1/plugins/install`（本地目录/git）
- [ ] L3 启停的副作用可逆：工具/hook/trigger/UI 全撤销（停用后模型侧不可见）
- [ ] L4 热装/热更后 UI 清单立即可见（SSE 或快照版本号）
- [ ] L5 审计表 + `GET /v1/plugins/audit`（时间/操作者/插件/版本/动作/结果/目标）

## S. sidecar

- [ ] S1 插件 sidecar 协议（stdio 行 JSON-RPC：initialize/tools.list/tools.call/health/close）
- [ ] S2 进程监督：env 白名单、进程组回收、崩溃→failed（可重连）、无孤儿
- [ ] S3 工具注册（`<plugin>.<tool>` 命名）、超时与取消

## W. WebView

- [ ] W1 `/plugins/<id>/*` 静态路由（仅已安装且启用；禁止越界）
- [ ] W2 `nsh.json` 增 `webview` 能力 + Web 目标 `WebView` 承载页（`/plugins/<id>` 路由）
- [ ] W3 桥接 API `keel.plugin.*`（state/call/approve/subscribe），不下发宿主密钥
- [ ] W4 插件 UI 模板（webview + react + solid-glass；主题令牌由宿主注入）+ 脚手架

## P. 权限与审批

- [ ] P1 工具白名单派发校验；net host 白名单；secrets 引用白名单
- [ ] P2 安装/启用/升级的 durable 审批（复用 decisions），拒绝不生效
- [ ] P3 越权拒绝写审计

## C. 创造模式（`keel.creation`，内置插件）

- [ ] C1 插件骨架：工具 `creation.plan`/`creation.build`（NL→插件包）+ Web UI（对话式）
- [ ] C2 生成：manifest + 宿主入口 + UI（模板）+ 测试/校验；失败回滚清理
- [ ] C3 安装→启用→返回深链；产物列举/停用/删除；审计
- [ ] C4 审批门（生成涉及写盘/网络/工具时）

## T. TUI

- [ ] T1 插件行「打开」动作：`openUrl`（`nsh.json` 能力）+ 不支持时 URL 文本兜底
- [ ] T2 TUI 断言：不渲染插件 UI、调用 openUrl（URL 断言）

## O. 官方插件套件（W1）

- [ ] O1 `plugins/COVERAGE.md` 矩阵（DSH 53 组 + 25 bundle + 社区高星；每行映射/波次/状态/理由）
- [ ] O2 既有能力面官方插件化（in-process 清单 + 默认启用）：fs / shell / web-search / web-fetch /
      session / compaction / mcp / skills / subagent / goal / todo / plan / jobs / schedule / terminal /
      browser-use / computer-use / storage / spill / attachments / session-query / deliverables /
      feedback / settings / plugin-manager
- [ ] O3 新能力插件（sidecar/webview 示范）：`keel.web-search`（真检索）、`keel.context-memory`
      （持久记忆注入）、`keel.plugin-market`（索引客户端：discover/install）
- [ ] O4 插件示例与脚手架（`plugins/template-ui/`：webview+react+solid-glass 最小可跑）

## Q. 验收

- [ ] Q1 单测/集成：清单校验、版本门、依赖拓扑、停用撤销、越权拒绝、审计、sidecar 崩溃隔离
- [ ] Q2 E2E（hermetic）：install→enable→工具可见→调用→disable→不可见→uninstall→审计
- [ ] Q3 创造模式 E2E：一句话→生成→安装→UI 可开→删除
- [ ] Q4 TUI 断言：插件入口 openUrl/URL 文本
- [ ] Q5 真机：线上 serve 走查（安装/启用/UI/创造模式/停用）+ 截图
- [ ] Q6 既有全量门不退化（go test -race、acceptance-all、真实模型 E2E）

## R. 复核与交付

- [ ] R1 子代理对抗复核（协议/生命周期/权限/审计/创造模式回滚/TUI）
- [ ] R2 覆盖矩阵复核（不许空行、absent 有理由）
- [ ] R3 报告 + 归档
