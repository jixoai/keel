# Tasks: keel-plugin-core

> M=清单/包 · L=生命周期/API · S=sidecar · W=WebView · P=权限/审批 · C=创造模式 · T=TUI · O=官方套件 · Q=验收 · R=复核。

## M. 清单与包

- [x] M1 `internal/pluginpkg`：`plugin.yaml` schema + 校验（id 文法、semver、kind、provides 形状、
      权限形状、engines 区间）+ 字段级错误
- [x] M2 发现：`<plugins-root>/<id>/plugin.yaml` 扫描、冲突（同 id 多版本）语义、损坏清单隔离
- [x] M3 版本门：`engines.keel` 与当前版本比对（安装 + 启用双门）
- [x] M4 包 hash 与受管存储（只允许 plugins-root 内；拒绝符号链接外逃）

## L. 生命周期与 API

- [x] L1 状态机 installed→enabled→disabled→uninstalled（+failed），与 agentext generation 对齐
- [x] L2 `GET /v1/plugins`（清单+状态+提供物）、`POST /v1/plugins/{id}/enable|disable|uninstall|update`、
      `POST /v1/plugins/install`（本地目录/git）
- [x] L3 启停的副作用可逆：工具/hook/trigger/UI 全撤销（停用后模型侧不可见）
- [x] L4 热装/热更后 UI 清单立即可见（SSE 或快照版本号）
- [x] L5 审计表 + `GET /v1/plugins/audit`（时间/操作者/插件/版本/动作/结果/目标）

## S. sidecar

- [x] S1 插件 sidecar 协议（stdio 行 JSON-RPC：initialize/tools.list/tools.call/health/close）
- [x] S2 进程监督：env 白名单、进程组回收、崩溃→failed（可重连）、无孤儿
- [x] S3 工具注册（`<plugin>.<tool>` 命名）、超时与取消

## W. WebView

- [x] W1 `/plugins/<id>/*` 静态路由（仅已安装且启用；禁止越界）
- [x] W2 `nsh.json` 增 `webview` 能力 + Web 目标 `WebView` 承载页（`/plugins/<id>` 路由）
- [x] W3 桥接 API `keel.plugin.*`（state/call/approve/subscribe），不下发宿主密钥
- [x] W4 插件 UI 模板（webview + react + solid-glass；主题令牌由宿主注入）+ 脚手架

## P. 权限与审批

- [x] P1 工具白名单派发校验；net host 白名单；secrets 引用白名单
- [x] P2 安装/启用/升级的 durable 审批（复用 decisions），拒绝不生效
- [x] P3 越权拒绝写审计

## C. 创造模式（`keel.creation`，内置插件）

- [x] C1 插件骨架：工具 `creation.plan`/`creation.build`（NL→插件包）+ Web UI（对话式）
- [x] C2 生成：manifest + 宿主入口 + UI（模板）+ 测试/校验；失败回滚清理
- [x] C3 安装→启用→返回深链；产物列举/停用/删除；审计
- [x] C4 审批门（生成涉及写盘/网络/工具时）

## T. TUI

- [x] T1 插件行「打开」动作：`openUrl`（`nsh.json` 能力）+ 不支持时 URL 文本兜底
- [x] T2 TUI 断言：不渲染插件 UI、调用 openUrl（URL 断言）

## O. 官方插件套件（W1）

- [x] O1 `plugins/COVERAGE.md` 矩阵（DSH 53 组 + 25 bundle + 社区高星；每行映射/波次/状态/理由）
- [x] O2 既有能力面官方插件化（in-process 清单 + 默认启用）：fs / shell / web-search / web-fetch /
      session / compaction / mcp / skills / subagent / goal / todo / plan / jobs / schedule / terminal /
      browser-use / computer-use / storage / spill / attachments / session-query / deliverables /
      feedback / settings / plugin-manager
- [x] O3 新能力插件（sidecar/webview 示范）：`keel.web-search`（真检索）、`keel.context-memory`
      （持久记忆注入）、`keel.plugin-market`（索引客户端：discover/install）
- [x] O4 插件示例与脚手架（`plugins/template-ui/`：webview+react+solid-glass 最小可跑）

## Q. 验收

- [x] Q1 单测/集成：清单校验、版本门、依赖拓扑、停用撤销、越权拒绝、审计、sidecar 崩溃隔离
- [x] Q2 E2E（hermetic）：install→enable→工具可见→调用→disable→不可见→uninstall→审计
- [x] Q3 创造模式 E2E：一句话→生成→安装→UI 可开→删除
- [x] Q4 TUI 断言：插件入口 openUrl/URL 文本
- [x] Q5 真机：线上 serve 走查（安装/启用/UI/创造模式/停用）+ 截图
- [x] Q6 既有全量门不退化（go test -race、acceptance-all、真实模型 E2E）

## R. 复核与交付

- [x] R1 子代理对抗复核（协议/生命周期/权限/审计/创造模式回滚/TUI）
- [x] R2 覆盖矩阵复核（不许空行、absent 有理由）
- [x] R3 报告 + 归档

## 已登记边界（交付时核实）

- **权限强制**覆盖「越过宿主边界的每一次能力请求」（工具派发、宿主中介 fetch、secret 引用、子进程环境）；in-process 与 sidecar **都不做沙箱**，sidecar 自己开 socket 不受 `permissions.net` 约束——要走宿主 brokered 通道才被治理。已写进实现注释与站点文档。
- `permissions.fs` 目前是声明式的，`plugin-data` / `read:plugin-package` 没有执行点；插件数据目录由 `KEEL_PLUGIN_DIR` 推导（无 `KEEL_PLUGIN_DATA_DIR` 一等公民）。
- 工具发布名归一：`.` → `_`，保留 `-`；因此 `keel.web-search` 的搜索工具发布为 `keel_web-search_search`（映射写进各插件 README 与 COVERAGE）。
- DSH 379 包不逐个移植：能力面矩阵见 `plugins/COVERAGE.md`，W1 交付内核 + 三个真插件（web-search / context-memory / plugin-market）+ 已有能力面插件化；W2/W3 为登记项。
- 不做 Cordis 运行时移植、不做公网市场服务端（本轮）。
