# WEBUI — NSH 客户端（web + TUI）

`web/` 是 keel 的第一方客户端：**同一份 TSX** 经 NSH 打包成 web 静态站点，也在 NSH 的
TUI target 里运行（`keel tui`）。它只消费两样东西：`@keel/chat-surface` 的冻结契约
（`packages/chat-surface/src/types.ts`）与 Harness API（`docs/HARNESS.md`）；不直连内核。
细节（目录/主题/屏幕/QA 产物）见 `web/README.md`。

## 1. 构建与托管

```bash
cd web
nsh check                                   # 类型检查
nsh test --target tui                       # 同一份代码在终端宿主上的 5 条行为测试
cd ..
scripts/package-web.sh                      # 静态站点 → web/dist/web/keel/Keel/（含 theme.json）
```

`scripts/package-web.sh [out-dir]` 包一层 `nsh package --target web`：nsh 只产出静态站点，
而 `keel serve` 需要 `theme.json` 与 `index.html` 同目录（`loadHostTheme` 把它交给插件 UI，
插件才能继承宿主令牌），所以脚本把 `web/theme.json` 一并放进去。发布流水线
（`.github/workflows/release.yml`）走同一个脚本。

`keel serve` 同源托管该产物（`web/dist` 下的嵌套根会被自动识别）；未构建时 `/` 返回 JSON 404
并给出构建指引。TUI 由 `keel tui` 启动（`KEEL_TUI_BIN` 或 `nsh run --target tui` 开发宿主；
`nsh package` 没有 tui target，见 `web/README.md:167`）。

## 2. 两种数据模式

| 模式 | 数据源 | 用途 |
| --- | --- | --- |
| 实时 | Harness API + SSE（`fetch` 流式解析，`after`/`Last-Event-ID` 续传） | 真实服务 |
| 演示 | `src/data/fixture.ts` 本地 scripted 服务（覆盖聊天/3 并行工具/审批/引导/压缩/失败/空会话/断线） | 视觉走查、无后端演示 |

启动时探测 `/v1/capabilities`：失败自动回退演示模式，侧栏可手动切换「演示/实时」。

## 3. 与核心的接法

- 投影与状态机在 `@keel/chat-surface`（零 DOM、61 个单测）；`web/src/data/core.ts` 只是宿主
  适配层（附件索引、partial 访问），`UiEngine` 是 UI 面向的窄接口。
- 聊天列表用 NSH `List alignment="bottom"`：新内容从底部出现，贴底才跟随流式。
- queue/guide：composer 切换投递模式；运行中发送显示「等待引导当前任务…」；服务端把 guide
  降级为 queue 时，事件里 `delivery=queue`，UI 回到队列并保序；停止走 stop barrier
  （`run.finished` 才解除）。
- 审批：decision 卡一键作答，重复提交幂等；超时/取消可见。

## 4. NSH 版本策略

**pin release `v0.6.1`**（`nsh 0.6.1 (2f51d86)`，2026-11-24 由 0.6.0 nightly `f577c51` 升级；
不再跟随 nightly，理由见 `web/README.md:19-23`）：`List alignment=bottom` 与 `--target tui`
开发宿主均已具备；需要新特性直接向作者提。

## 5. 视觉证据

- `web/qa/screens/*.png`：21 个 web 关键状态（深/浅色、移动端、审批、引导、停止、partial、
  重连、搜索、删除确认…）。
- `web/qa/tui/*.png|txt`：6 个 TUI 终端状态（cell dump 渲染）。
- `web/qa/tui-test.log`：TUI 测试 5/5。
