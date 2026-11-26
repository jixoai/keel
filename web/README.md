# Keel · NSH 客户端（web + TUI）

`web/` 是 keel 的 NSH 客户端：同一份 TSX 经 `nsh` 打包为 **web 静态站点**，同一份代码
用 NSH **TUI target** 在终端运行。UI 只消费 `packages/chat-surface/src/types.ts` 的冻结契约
和 Harness API（`/v1/*`），不直连内核。

## NSH 版本与用到的特性

| 项 | 值 |
|---|---|
| CLI | **nsh 0.7.1 (f78eace)**，2026-11-26 由 0.6.1 (2f51d86) 升级 |
| 安装 | `curl -fsSL https://nsh.dev/install.sh \| NSH_VERSION=v0.7.1 sh -s --`（校验 `sha256` 通过） |
| 组件 | `web`、`tui`（由 `nsh package/run/test` 自动安装）；Rust 工具链 + `wasm32-unknown-unknown` + `llvm-tools` + `wasm-bindgen-cli 0.2.129`（web 打包的 nshm 前置） |
| 关键特性 | `List alignment="bottom"`、`--target tui` 开发宿主、`TextView` markdown/streaming、`Picker`、`host` 通道、`nsh test --target tui`、`nshm`（`nsh add` + `nsh.lock`）、`app.fake` |
| 本地参考检出 | `~/Development/GitHub/longbridge/nsh` |
| 0.7.0/0.7.1 升级适配 | ① `Speech` 从 `nsh` 移到 **`nsh-extra`**：`nsh add nsh-extra --features speech`，`import { Speech } from "nsh-extra"`；测试从 `app.speechRequests()/hear()/endSpeech()` 改为 `await app.fake("nsh-extra/speech", "requests"\|"hear"\|"end", ...)`；② `nsh.lock` 记录 nshm 版本与 checksum；③ web 打包现在会编译 Rust/wasm，需要 rustup 的 wasm target、`llvm-tools` 的 `llvm-ar` 与匹配版本的 `wasm-bindgen-cli` |

**为什么锁 v0.7.1**：0.7 把 `Speech` 抽成官方 `nsh-extra` 模块，是当前稳定 release；迁移后
`nsh check` 0 错、`nsh test --target tui` 全绿、`nsh package --target web` 可产出静态站点。
**不跟随 nightly**：nightly 是滚动目标，不能作为可复现的发布输入（0.6.x nightly 曾移除
`Speech` 导出导致 web 构建失败）。web 打包需要：
`rustup target add wasm32-unknown-unknown && rustup component add llvm-tools && cargo install wasm-bindgen-cli --version 0.2.129 --locked`，
并让 `~/.cargo/bin` 与 rustup 的 `llvm-ar` 目录在 `PATH` 内。

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
nsh 0.7 的 web runtime 含 wasm；部署若设置 CSP，`script-src` 需要 `'self' 'wasm-unsafe-eval'`，
否则语音等 nshm 能力会被浏览器拒绝。
注意当前 `nsh package` 会在 `-o` 目录下再建一个以 app 名命名的子目录（`Keel/`），
部署时以该子目录为站点根。

## 目录

```
web/
├── App.tsx                    入口：路由决定 shell（工作区 / 设置页）；工作区宽屏侧栏+主区 / 窄屏两屏；自动打开最近会话
├── App.tui.test.tsx           TUI 行为测试（会话/审批/引导/停止/partial + 设置页 + 会话模型选择器）
├── nsh.json                   app 元数据、外观、theme、capabilities、web.host
├── theme.json                 solid-glass 设计令牌（light/dark）
├── host.js                    web host：keel.notify / keel.info / keel.route / keel.navigate
├── src/
│   ├── data/
│   │   ├── contract.ts        冻结契约的 type re-export + UI 侧扩展类型（含 settings/secrets/plugins/selection）
│   │   ├── wire.ts            事件 payload 解析/格式化 + selection/模型能力格式化
│   │   ├── core.ts            @keel/chat-surface 核心的宿主适配层（附件索引/partial 访问）
│   │   ├── http.ts            fetch + SSE EventSource、SurfaceCommands、设置/密钥/插件/任务看板 API
│   │   ├── fixture.ts         本地 scripted keel 服务（演示/QA，含设置面与任务看板演示数据）
│   │   ├── superagent.ts      任务看板状态：四类计数、收件箱、answer/resume/stop 与刷新
│   │   └── store.ts           会话列表、打开/发送/停止/审批、模式切换、重连、路由、设置/密钥/插件状态
│   ├── screens/               session-list / conversation / messages / composer / settings / model-picker
│   │   └── books/             工作区外章节：任务看板 / 员工 / 团队 / 身份 / 记录
│   └── ui/                    theme tokens、lucide 风格单色 Canvas 图标、primitives
```

## 桌面壳

- `nsh.json`：`singleInstance: true`、`schemes: ["keel"]`。
- 深链：`keel://session/<uuid>`、`keel://session/<uuid>/decision/<id>`、`keel://settings`；
  非法链接只提示，不导航（`src/desktop/links.ts` 纯函数，单测覆盖）。
- 托盘：显示/设置/退出；全局快捷键 `mod+shift+k` 唤回；关闭最后窗口不退出（tray 退出）。
- 仅桌面 host 生效（`nsh/desktop` 的 `isAvailable`）；web/TUI 是 no-op。

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
- **Composer**：web 用 `nsh/base` RichEditor（Markdown 快捷输入、粘贴 HTML 导入、
  发送为 Markdown），Enter 发送 / Shift+Enter 换行；TUI 与原生/手机暂保持纯文本 Input；
  高度 5 行封顶；
  queue/guide 切换（缺省 queue；空闲时一律按 queue 发送）、运行中发送 + 独立停止按钮
  （stop barrier 状态）、附件（Picker + `/v1/files`）、「等待引导当前任务…」、服务端降级回队列并保序。
- **审批**：decision 卡（问题/选项/推荐/剩余时间/超时/取消），一键作答后卡片变为 `answered`，
  同一卡片提交中禁用、重复提交幂等。
- **连接**：header 与侧栏显示连接状态；断线重连中显示 `after` 游标；fixture 提供「断线演示」
  验证缺口补发与去重。
- **通知**：web 构建通过 `host.js` 的 `keel.notify` 在审批到达时发系统通知（需权限）；
  非 web 宿主自动 no-op。

## 任务看板（change keel-collab-superagent, 4.2/4.3 产品 UI）

「02 任务」是 superagent 工单模式的产品面，web 与 TUI 共用同一份 TSX：

- **路由与入口**：`/superagent`（深链 `/superagent/<ticketId>`），主导航「任务」与命令面板
  「打开任务看板」都按 `task.read` 能力门显示；窄屏底部标签同语义。
- **数据面**：`GET /v1/superagent/tickets`（Ticket + Mandate + 当前 Assignment + 计数）与
  `GET /v1/superagent/attention`（开放问题收件箱）；写入口是
  `POST /v1/superagent/attention/{id}/answer`、`POST /v1/superagent/tickets/{id}/resume|stop`。
  `src/data/http.ts` 用现有 CSRF/`KeelApiError` 词汇实现，`src/data/superagent.ts` 持有页面状态与
  读写动作（单一 transport seam：`store.superagentApi()`），不另建 fetch 通道。
- **五态**：加载骨架、空（说明来源事件如何产生工单 + 刷新/去工作区）、错误（服务端原因 +
  请求编号 + 重试；503 说「未接通」）、权限不足（缺 `task.read`，四段式说明 + 申请入口）、
  成功（四类计数、工单列表、Ticket 详情、待人工收件箱）。空/错/权限态不注入任何示例行；
  fixture 只在 `?fixture=1` / `host.params.fixture` 下提供。
- **四类计数**：待人工（blocked 或有开放 Attention）/ 执行中 / 完成 / 失败（failed 或 cancelled），
  一票只进一个桶，未知状态单列 `other`，四个数加总等于可见行数。
- **Ticket 详情**：Mandate（发起人/来源/强度/策略/撤销）、Worker（当前 Assignment、Bot、Session、
  最近 report 的 summary 或原始 JSON、错误）、开放 Attention，以及恢复/停止动作。
- **收件箱**：问题正文、来源线程、选项与自由文本、截止时间（过期就地禁用并说明服务端会拒绝）；
  作答走 durable 写，成功后与看板/计数一起刷新；失败按卡片显示服务端原因（403 另说明 operator 授权）。
- **权限门**：读=`task.read`；写=`task.write` 且服务端 `OperatorAuthorizer` 另行判定。只读账号
  看得到看板，但答复/恢复/停止控件不渲染，页面明确写出缺少 `task.write`；服务端 403 显示原因，
  operator 未接通与 503 未启用均使用中文说明。
- **实时刷新**：identity stream 新增 `superagent` kind；只有已尝试加载的看板/收件箱才重读
  （未打开不请求；错误态也会自愈重读）；事件落在首屏加载中时在本次结束后补读一次；
  后台重读不清空写操作的成功/失败反馈，手动「刷新」保留为兜底并清空反馈。
- **视觉**：计数瓦片 + 收件箱卡片 + 工单行/详情 band，全部 solid-glass 令牌 + lucide 单色；
  ≥1024 列表与详情同屏（详情 420px），<1024 列表/详情分屏，≤767 单列、计数 2×2、44px 控件。
- **验证**：`src/screens/books/superagent.tui.test.tsx`（成功面、最新在前、answer 选项/自由文本、
  resume/stop、待投递失败行与重试/清除、五态、只读、写失败、390/768）与
  `src/data/superagent.test.ts`（计数/时限/词表/202 等待投递/409 投递失败/待投递路由）；
  `nsh test --target tui --filter 'superagent'` → `20 tests: 20 passed, 0 failed`
  （11 TUI + 6 data + 3 identity-stream 失效/竞态事件）；全量 `nsh test --target tui` →
  `437 tests: 437 passed, 0 failed`。

## 设置页与会话模型选择（change keel-model-settings）

**设置是独立的面**：路由 `/settings`（深链直达，`keel serve` 的 SPA 回退把页面交给客户端，
客户端 boot 时经 `host.js` 的 `keel.route` 读 `location.pathname` 决定 shell；`keel.navigate`
把应用内路由写回地址栏，浏览器前进/后退经 `popstate` → `keel.route` 事件跟随）。
非 web 宿主（TUI/桌面/测试）没有 URL，路由留在应用内。入口：会话列表 header 的齿轮与
底部「设置」入口；返回「工作区」按钮回到工作区，**原会话保持打开**（store 跨 shell 切换持有
`activeId`/engine，narrow 布局的屏幕位也保留）。

设置页的分区（同一套 primitives/theme，不引入第二套组件体系）：

- **模型接入**：两级结构。第一级＝「当前默认模型」卡（provider / 模型 / 思考档 / 来源与版本，
  右侧只有「更换」「刷新」）＋ 一个 Provider 一张卡（显示名、已配置/未配置/不可用徽标、
  模型 chips、右下「配置」）；空态给「添加 Provider」引导。第二级＝点「配置」打开的 Dialog，
  只编辑这一个 Provider（接口协议 / 服务地址 / 凭据引用 `${NAME}`·`env:NAME`·`secret:NAME` /
  模型列表可增删；保存并生效 / 取消 / 删除该 Provider 二次确认）。原始 JSON 降到卡片列表
  底部的「高级」折叠（默认收起，直接编辑整份配置，保存后立即生效）。
  保存 PUT 整份文档（带 `expectedRevision`）：400 的 `issues` 按字段路径原样列出，409 给出
  「文档已被其它写入更新 + 当前 revision」并给刷新入口；成功后显示新 revision 并刷新
  `/v1/providers` + `/v1/models`。
- **密钥**：`GET /v1/settings/secrets` 只返回名字与创建/更新时间；写入/覆盖（值输入框 masked，
  保存后清空，值永不回显、不进列表）；两步删除确认；`available:false` 时显示服务端 `reason`
  禁用表单（永不回退明文）。
- **插件**：`GET /v1/plugins`（generation、插件状态/版本/provides/requires/tools/hooks/triggers/
  diagnostics/启停时间、hook 统计、工具清单）与 `GET /v1/triggers`（计划、启停、次数、上次结果）；
  每个触发器有「触发」按钮（`POST /v1/triggers/{id}/fire`，manual 来源），结果显示/错误原样呈现；
  明示「插件在启动时装载，这里看状态与控制触发，不做代码级配置」。

**会话内模型选择器**：会话 header 的 chip 显示本会话的模型（与租户默认分开显示；未选择时显示
租户默认）；弹层列出 `/v1/models` 中 `selectable` 的模型（按 provider 分组）与思考档，可一键
「恢复默认」。选择是**输入事实**：之后每条 `POST /v1/sessions/{id}/input` 带
`selection:{providerId,modelId,reasoningLevel?}`（`HttpCommands.send/sendWithAttachments` 的
`SendOptions`），不写租户文档；清除选择后输入不再带该字段（服务端回租户默认）。

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
6. **`List alignment="bottom"` 只在 nightly**：发布版 0.3.0 缺失（本仓已用 0.6.0 nightly）。
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
