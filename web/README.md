# Keel · NSH 客户端（web + TUI）

`web/` 是 keel 的 NSH 客户端：同一份 TSX 经 `nsh` 打包为 **web 静态站点**，同一份代码
用 NSH **TUI target** 在终端运行。UI 只消费 `packages/chat-surface/src/types.ts` 的冻结契约
和 AgentAPI（`/v1/*`），不直连内核。

## NSH 版本与用到的特性

| 项 | 值 |
|---|---|
| CLI | **nsh 0.4.0 (bba76ef nightly)**，2026-11-08 安装 |
| 安装 | `curl -fsSL https://nsh.dev/install.sh \| sh -s -- --nightly`（校验 `sha256` 通过） |
| 组件 | `web`、`tui`（由 `nsh package/run/test` 自动安装） |
| 关键特性 | `List alignment="bottom"`（聊天列表底部对齐）、`--target tui` 开发宿主、`TextView` markdown/streaming、`Picker`、`host` 通道、`nsh test --target tui`（in-memory terminal） |
| 本地参考检出 | `~/Development/GitHub/longbridge/nsh` |

**为什么必须 nightly**：发布版 0.3.0（4e95ae5）既没有 `List alignment=bottom`，也没有
`--target tui`；两者都在官方 main 上，nightly 已包含。本仓按 Owner 指令 pin main/nightly，
不锁 release。

## 构建与运行

```bash
cd web

nsh check                                   # 类型检查（生成 .nsh/types）
nsh build                                   # 打包 bundle
nsh package --target web -o dist/web/keel   # 静态站点：dist/web/keel/Keel/
nsh run --target web --port 13680           # 开发宿主（端口只用 13610–13799）
nsh test --target tui                       # 同一份代码在 TUI 宿主上跑测试
nsh run --target tui                        # 真实终端开发宿主
```

静态站点用任意静态服务器托管即可（`keel serve` 会把 `dist/web/keel/Keel/` 挂在根路径）。
注意当前 `nsh package` 会在 `-o` 目录下再建一个以 app 名命名的子目录（`Keel/`），
部署时以该子目录为站点根。

## 目录

```
web/
├── App.tsx                    入口：宽屏侧栏+主区 / 窄屏两屏；自动打开最近会话
├── App.tui.test.tsx           TUI 行为测试（会话/审批/引导/停止/partial）
├── nsh.json                   app 元数据、外观、theme、capabilities、web.host
├── theme.json                 solid-glass 设计令牌（light/dark）
├── host.js                    web host：keel.notify / keel.info / keel.network
├── src/
│   ├── data/
│   │   ├── contract.ts        冻结契约的 type re-export + UI 侧扩展类型
│   │   ├── wire.ts            事件 payload 解析/格式化
│   │   ├── core.ts            @keel/chat-surface 核心的宿主适配层（附件索引/partial 访问）
│   │   ├── http.ts            fetch + SSE EventSource、SurfaceCommands、会话 API
│   │   ├── fixture.ts         本地 scripted keel 服务（演示/QA）
│   │   └── store.ts           会话列表、打开/发送/停止/审批、模式切换、重连
│   ├── screens/               session-list / conversation / messages / composer
│   └── ui/                    theme tokens、lucide 风格单色 Canvas 图标、primitives
└── qa/                        web 截图、TUI cell dump/渲染与运行日志
```

## 与 headless 核心的接法

- **契约单源**：`src/data/contract.ts` 只做 `export type ... from "../../../packages/chat-surface/src/types"`，
  再加 UI 需要的扩展（`SessionSummary`、`AttachmentRef`、`UiCommands`、`UiEngine`）。
  `packages/chat-surface/src/types.ts` **未改动**。
- **核心已落地并接线**：`src/data/core.ts` 是 `@keel/chat-surface`（`createSurfaceEngine`）的
  宿主适配层——投影、快照→流→重连补齐、queue/guide/stop 状态机全部由核心承担；适配层只加
  冻结契约留给宿主的附件索引与 partial 访问。`UiEngine` 是 UI 面向的窄接口，核心在它背后。
- **UiEngine 的额外方法**（不改冻结契约）：`setConnection`、`setSessionId`、`attachmentsFor`、
  `removeOptimistic`、`resetStopping`、`partialText/partialTurnId`；核心未提供的由适配层空实现
  或映射（例如 stopping 屏障由核心 intent 持有）。
- **事件源**：`HttpEventSource` 用 `fetch` + `ReadableStream`/`TextDecoder` 解析 SSE
  （`id:/event:/data:`、心跳注释、`partial` 帧），断线指数退避重连并以 `after`/`Last-Event-ID`
  续传；重连 `open` 后先 `GET /events?after=` 补缺口，再交给核心按 seq 去重。
- **命令面**：`POST /v1/sessions/{id}/input`、`/cancel`、`/decisions/{decisionId}`、
  `POST /v1/files`（raw body）。`sendWithAttachments` 是 UI 扩展：在 input body 里额外带
  `attachments`（`{fileId,name,mime,size}`），不影响冻结的 `send()`。
- **fixture**：`FixtureClient` 与 HTTP 客户端同形（list/create/delete/source/commands），
  种子覆盖聊天、3 并行工具、审批、引导等待、压缩、失败、空会话；启动时探测
  `/v1/capabilities`，失败即回退 fixture，侧栏可手动切换「演示/实时」。

## 主题（solid-glass）

`theme.json` 是 shadcn `cssVars` 形状的 solid-glass 令牌，取值来源为 fde2e 既有
`fde2e-cu-v0/apps/desktop/src/renderer/vendor/solid-glass.css`（solid-glass@6e8903e）：
颜色名 1:1 保留（`background/panel/card/popover/primary/...`、`glass-*`、`brand*`、
`status-running/done/interrupted`、`chart-*`、`sidebar-*`），另有 `radius`、`radius-card`、
`radius-control`、`radius-pill`、`font-sans/font-mono` 与语义状态色 `success/warning/danger/info`
及 `*-tint`。

- `cssVars.light` / `cssVars.dark` 分别承载两套值；NSH 随系统 appearance 自动切换，
  代码里没有 `isDark` 分支、没有内联色值。
- Solid Glass 的「边缘完成度」用令牌组合在 `src/ui/theme.ts` 的 `glass.panel/card/popover/control/state/field`
  里：rim（`glass-rim`）+ hairline（`glass-edge`）+ shade（`glass-*`），面色中性、无渐变。
- 动效曲线与间距以 `motion`/`space` 代码常量统一（NSH 主题文件不承载曲线/阴影字符串）。

## 屏幕与状态

- **会话列表**：新建、两步删除确认、搜索、空/加载/错误态、最后消息摘要与时间、运行中/失败/已取消标识。
- **会话视图**：用户/助手消息、thinking 折叠、工具组（组内按事件顺序、可展开参数/结果/耗时/失败）、
  图片与附件、压缩分隔、run 状态、`run.finished` 摘要、消息级流式与 partial 开关。
- **聊天列表**：`<List alignment="bottom">`，新内容从底部出现；列表贴底才跟随流式，
  上滚后显示「回到底部」而不强制拉回。
- **Composer**：queue/guide 切换（缺省 queue；空闲时一律按 queue 发送）、运行中发送 + 独立停止按钮
  （stop barrier 状态）、附件（Picker + `/v1/files`）、「等待引导当前任务…」、服务端降级回队列并保序。
- **审批**：decision 卡（问题/选项/推荐/剩余时间/超时/取消），一键作答后卡片变为 `answered`，
  同一卡片提交中禁用、重复提交幂等。
- **连接**：header 与侧栏显示连接状态；断线重连中显示 `after` 游标；fixture 提供「断线演示」
  验证缺口补发与去重。
- **通知**：web 构建通过 `host.js` 的 `keel.notify` 在审批到达时发系统通知（需权限）；
  非 web 宿主自动 no-op。

## QA 产物

- `qa/screens/*.png`：headless Chrome 截取的 21 个关键状态（列表/对话/工具卡/引导等待+发送/停止中/
  已停止/压缩/partial 流式/重连/搜索/新建/删除确认/浅色/移动端列表与对话）。
- `qa/tui/*.png|txt`：`nsh test --target tui` 的 in-memory terminal cell dump 经
  `qa/tui-render.mjs` 渲染的 6 个终端状态（列表/审批/已作答/对话/引导等待/已停止）。
- `qa/tui-test.log`：TUI 测试 5/5 通过的日志；`qa/tui-run.log`：真实 `nsh run --target tui`
  在 120×40 PTY 中的启动日志。
- 重新生成：
  ```bash
  nsh package --target web -o dist/web/keel
  (cd dist/web/keel/Keel && python3 -m http.server 13680 --bind 127.0.0.1)
  NODE_PATH=<含 playwright-core 的 node_modules> node qa/screenshot.mjs
  nsh test --target tui
  NODE_PATH=<同上> node qa/tui-render.mjs
  ```

## 已知限制 / 待向 NSH 作者提

1. **TUI 只能跑不能打包**：`nsh package` 没有 tui target；`keel tui` 目前只能走开发宿主
   （`nsh run --target tui`）或按官方 `nsh-platform-tui` 嵌入。建议官方提供可分发 TUI 产物的方式。
2. **web 产物目录多一层**：`nsh package --target web -o dist/web/keel` 实际输出
   `dist/web/keel/Keel/`；本仓按此部署，若能让 `-o` 直接作为站点根更符合直觉。
3. **TUI 截图不是位图**：`app.screenshot` 在 `--target tui` 下返回 `nsh-tui-cells` JSON（终端 cell），
   不是 PNG；本仓 `qa/tui-render.mjs` 自行渲染。建议文档化该格式或提供位图。
4. **Web Push / service worker**：`nsh package --target web` 没有公开自定义 service worker/public 文件的
   入口，本仓只做 host 通知；真正的 Web Push 需官方支持。
5. **`nsh test --target tui` 的终端尺寸不可配**（观测 49×53 cells）；窄终端适配已做，但不能模拟宽终端。
6. **`List alignment="bottom"` 只在 nightly**：发布版 0.3.0 缺失（本仓已用 0.4.0 nightly）。
7. **自定义组件的 `testID` 透传**：经 `Press` 包装后 `id:` 选择器找不到（`Input` 直接使用正常），
   本仓测试改用 `label:`；若为官方 bug 建议修复，否则请在文档说明限制。

## 后端契约缺口（本地客户端已按宽容形状处理）

- `GET /v1/sessions` 的 spec 只要求 `sessionId/createdAt/updatedAt/lastSeq/runStatus/lastMessageAt`，
  没有标题/摘要；客户端在缺失时用 `GET /v1/sessions/{id}/events` 尾部事件补全（最多前 12 个会话）。
- `DELETE /v1/sessions/{id}` 不在 agent-api spec / 现有 runtime 路由中；客户端已接，服务端未提供时
  会在 UI 显示删除失败，不吞错。
- `decision` 事件 payload 未在 spec 定形；客户端接受 `decisionId|id`、`question|prompt|title`、
  `options[{optionId|id,label|title,description}]`、`recommendation`、`status`、`answer`、
  `timeoutAt|expiresAt`。
- partial 帧未定形；客户端假设 SSE `event: partial`，`data` 为 `{text, turnId?}` 的**累计**文本
  （若服务端发 delta，需要一行适配）。
- 附件不在冻结的 `SurfaceCommands.send` / `InputView` 中；客户端以 `attachments` 扩展字段发送，
  并在 UI 侧按 requestId 索引展示。若核心/协议定形，改 `HttpCommands.sendWithAttachments` 一处即可。
- `/v1/sessions` 与 `/v1/capabilities` 之外的扩展面（plugins/triggers）本期 UI 未消费。
