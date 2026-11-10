# Proposal: keel-plugin-core（一切皆插件：插件内核 + 官方插件套件 + 创造模式）

## Why

Owner 裁决：keel 的内核「没开发好」——插件覆盖不全。现状（侦察证据）：

- 插件协议**只有 Go 结构体**（`internal/agentext/plugin.go`：ID/Version/Provides/Tools/Hooks/
  Triggers/Effects/Start/Stop），没有清单文件、没有发现/安装/启停/卸载 API、没有包存储、
  没有沙箱与权限、没有 WebView 承载；生产只在 boot 装配一次（`lifecycle:"boot"`）。
- 唯一真实插件是 `keel.example`（2 工具/3 hook/1 webhook 触发）；web 设置页「插件」区看到的
  `demo.workflow` 只是 fixture 假数据。
- 对比基准 DeepSeek Harness（DSH）：「Everything is a Plugin」——`packages/` 下 **53 组 / 379 个包**，
  25 个可安装 bundle，206 个默认启用项；插件＝`apply(ctx)` + 依赖注入 + 可逆副作用，
  连 agent-loop、tools、沙箱、会话存储、UI 壳都由同一协议提供；第三方可**替换**而不只是扩展
  （Service Definition / Provider / Consumer 三角色）。
- 创造模式在 keel 里零命中；TUI 已能渲染插件区，但既无插件 UI 承载也无「打开浏览器」路径。

## What Changes

1. **插件包与清单**：`$KEEL_HOME/plugins/<id>/plugin.yaml`（id/version/kind/description/engines/
   provides{解放到 tools/hooks/triggers/ui}/requires/permissions/entry/source）。三种 kind：
   - `in-process`：编译进二进制的官方插件（注册表 + 清单启用/停用）；
   - `sidecar`：外部进程，复用 `internal/gateway` 的 stdio JSON-RPC（可 Node/TS/Python/任意），
     声明工具（可选 hooks/triggers）；
   - `webview`：UI 资源包（默认栈 webview + react + solid-glass），后端可选 sidecar。
2. **发现/安装/启停/卸载/升级**：插件目录扫描（复用 `internal/skills` 的发现与冲突语义）、
   安装（校验 manifest + 版本门 + 包 hash + 写审计）、`enable/disable/uninstall/update` 全走 HP，
   状态机与 agentext generation 对齐；热装后 UI 清单与工具立即刷新（SSE/快照）。
3. **UI 承载**：`/plugins/<id>/*` 静态资源路由 + WebView 承载（`nsh.json` 增 `webview` 能力）；
   插件 UI 只能用宿主下发的主题令牌与桥接 API（`keel.plugin.*`：调用宿主、读状态、审批）。
4. **权限与沙箱**：manifest 声明 `fs/net/secrets/tools` 白名单；sidecar 走 gateway 的 env 白名单、
   进程组回收与可选中继；安装/启用/升级需审批（durable decisions），拒绝即不生效。
5. **审计**：插件生命周期与调用归属可查（`GET /v1/plugins/audit`：时间/操作者/插件/版本/结果）。
6. **创造模式（本身是一个官方插件 `keel.creation`）**：对话式自然语言 → 生成插件包
   （webview+react+solid-glass 模板）→ 校验 → 安装 → 启用 → 返回 UI 深链；带审批、失败回滚、
   产物可列举/停用/删除；生成过程与产物写审计。
7. **TUI 行为**：插件 UI 不在终端渲染；插件行提供「打开」→ `openUrl(https://…/plugins/<id>)`，
   宿主不支持时显示 URL 文本（可复制）。
8. **官方插件套件（对齐 DSH，分波次）**：以 DSH 53 组能力面为矩阵（见 `plugins/COVERAGE.md`），
   每个能力面映射到 keel 插件 id、波次与状态；W1 交付内核 + 已具备能力面的「官方插件化」
   + 至少 3 个新能力插件（web-search / context-memory / plugin-market 索引客户端）；
   W2/W3 为登记项（社区高星能力面：浏览器直连、桌面操作、移动网关、透明主题、插件市场、子代理桥……）。

## Non-Goals（本轮）

- 不做 DSH 的 Cordis 运行时移植（keel 用 Go 的 agentext + servicegraph 等价表达；
  但协议语义对齐：依赖注入、可逆副作用、可替换的 capability seams）。
- 不做插件公网市场服务端（只做索引客户端 + 本地安装；市场托管留给后续）。
- 不把 379 个 DSH 包逐个移植；按 `plugins/COVERAGE.md` 的能力面矩阵交付与登记。

## Impact

后端：`internal/agentext`（清单化/生命周期 API）、`internal/agentapi`（插件 API/审计/UI 路由）、
`cmd/keel`（装配、静态路由、openUrl 协作）、`internal/gateway`（sidecar 复用）、`internal/store`
（插件表/审计表）。前端：插件面（见 `keel-harness-shell`）。插件源：仓库新增 `plugins/<id>/`。
