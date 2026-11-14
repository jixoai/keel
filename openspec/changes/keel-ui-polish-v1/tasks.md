# Tasks: keel-ui-polish-v1

> 状态核对基线：2026-11-14，逐条按 `audit.md` 的「实施」判定 + 当前工作区代码取证。
> 条目编号沿用 `audit.md`（A/B/C/D 分档）。

## S1 评审（只读）

- [x] 按 better-interface 六维（可访问性/布局/文案/排版/颜色/细节）逐屏评审
      → `audit.md` 汇总 22 条（P0 3 / P1 8 / P2 11；C1–C7 与 D1–D4）
- [x] 按 better-accessibility 检查键盘、焦点可见、ARIA、可读名称、表单错误、动效与缩放
      → A1（焦点环 1.14:1/1.16:1）、A2（键盘不可达删除会话）、A3（徽章对比度）、
        B1（2.5.3 标签在名称中）、B8（Toast 关闭键无焦点/无 24px 命中区）均带实测数值
- [x] 按 better-typography / better-colors / better-layout 检查字号阶梯、对比度、对齐与换行
      → B4（无边框控件 6px 净距）、B5（10px 触底）、B6（无行高约束）、B7（时间戳被挤）、
        A3（ink 2.46–3.85 → 7.19–8.72）
- [x] 按 better-writing 检查空态/错误/确认文案是否给出下一步
      → B2（登录 401 无下一步）、B3（命令面板空态只教换词）
- [x] 产出 `audit.md`：编号 / 维度 / 严重度 / 证据 / 建议
      → 每条含「维度 → 证据（含文件行号与实测数值/截图路径）→ 影响面 → 建议 → 实施」

## S2 实施（不改行为）

- [x] P0/P1 项逐条修复（焦点、可读名称、对比度、溢出与截断）
      → **P0 全 3 条**：
        A1 `web/src/ui/kit/components/styles.tsx` — `focusRing()` 改 `alpha(color.foreground, 0.65)`
          （实测深 5.78:1 / 浅 3.73:1，三种底色全过 1.4.11 的 3:1），静止态 `borderColor` 仍 `color.ring`；
        A2 `web/src/screens/session-list.tsx` — 行加 `focused` 信号（`onFocus`/`onBlur`，:179/:198-199），
          溢出菜单条件改 `props.wide && (hovered() || focused() || props.active)`（:273）；
        A3 `web/src/ui/kit/components/badge.tsx` — 四档语义徽章 label ink 改 `color.foreground`
          （2.46–3.85 → 7.19–8.72），语义色只留 face/border/icon。
      **P1 已实施 7 条**：
        B1 `web/src/screens/settings.tsx` — 可见 label 与 `accessibilityLabel` 同为「保存模型配置」
          （:791-792），`ProviderEditorCard` description 与 `web/README.md:124` 同步；
          测试定位点不变（`web/App.tui.test.tsx:105,118` 走 `label:保存模型配置`）；
        B2 `web/src/screens/login.tsx` — 常驻 hint 说明口令来源（:65），401 文案补下一步（:38-39），
          其余错误分支原样返回；
        B4 `web/src/screens/session-list.tsx` — 头部三枚图标按钮收进 `gap: space.md` 的组（:414 起）；
        B5 `web/src/screens/{session-list,plugins,session-status-pane}.tsx` — 5 处承载信息的 10px→12px
          （会话行时间戳、侧栏副标题、状态栏 fact 标签、插件表头、插件 `Fact` 标签）；
        B6 `web/src/ui/kit/components/{empty,card,field}.tsx` — `EmptyDescription` /
          `CardHeader.description` / `Field` 的 hint 与 error 给 `Math.round(size * lineHeight.normal)` 1.5 行高；
        B7 `web/src/screens/session-list.tsx` — 时间戳 `flexShrink: 0`（:259），预览保留 `ellipsis`；
        B8 `web/src/ui/kit/components/toast.tsx` — 「×」`Text` 换 kit `IconButton`（:115，
          可读名仍 `关闭提示`，`overlay.test.tsx:58` 定位点不变）。
- [~] 排版与间距收敛到令牌（不新增魔法数）
      → 部分完成：D2 `web/src/ui/kit/components/styles.tsx` 的 `menuRow()` 高度改
        `controlHeight("default")`；`dialog.tsx` 关闭键走 kit 尺寸语义（`radius.sm` +
        `disabledOpacity` + `focusRing()`）。
        **未完成（已登记）**：`dialog.tsx:105-106` 的关闭键仍是字面量 `24`、`badge.tsx:95`
        仍是 `height: 20`——`audit.md` 的 D2 建议包含后者，但「实施结果」只记了 `menuRow`，
        实际代码两处仍是魔法数；留作下一轮与 C4 的魔法数收敛同批。
- [~] 文案改为「告诉用户下一步」
      → 部分完成：B2（登录：常驻 hint 说明口令来源 + 401 补「去哪确认口令」）已做。
        **未做**：B3/C7（命令面板空态补「清除搜索」按钮）——补按钮是新增一个交互控件，
        超出「不改行为」的边界，`audit.md` 已记为未做项。
- [x] 每条附回归测试或既有测试覆盖说明
      → 覆盖面：A1/A3/B4/B5/B6/B7/D2/D4 是纯样式值与令牌替换，无行为分支，由既有 TUI 全量
        （84/84）与 `nsh check` 覆盖；B1 由 `web/App.tui.test.tsx:105,118` 的 `label:保存模型配置`
        断言覆盖；B8 由 `overlay.test.tsx:58` 的 `label:关闭提示` 覆盖；B2 的文案分支无测试
        （呈现层字符串，`audit.md` 已说明 store 不动）。
        **未新增测试**：`audit.md` 的「实施结果」表未列新增测试文件，本 change 以既有 82→84 的
        TUI 全量与 `nsh check` 作回归门，不虚报新增用例。

## S3 门禁

- [x] `nsh check` / `nsh test --target tui` 全绿
      → 2026-11-14 实跑：`nsh check` exit 0；`nsh test --target tui` **84 tests: 84 passed,
        0 failed**（1m 57s）。`audit.md` 记的 82/82 是同一 suite 的较早计数（之后新增了用例），
        两者都绿，计数差异不是回归
- [x] 改动文件清单 + 每条 before/after 截图（视觉项）
      → 清单见 `audit.md`「实施结果」（12 个源文件 + `web/README.md` 1 行）；
        before：`03-workspace.png`、`04-session.png`、`05-model-picker.png`、`06-settings.png`、
        `07-focus-input-crop.png`、`14-light-workspace.png`、`14-light-sidebar-crop.png`；
        after：`20-after-workspace-dark.png`、`22-after-focus-row.png`（+`-crop`）、
        `23-after-badge-crop.png`、`24-after-header-crop.png`（均在 `/tmp/ui-polish/`）
- [x] 明确列出「未做」与原因
      → `audit.md`「未做项」表 6 条：B3/C7（新增交互控件超边界）、C3（需 `aria-disabled`，
        属交互形态变更）、C4（设置页 39 处 10px 逐处调会改信息层级）、C6（offline 配色的语义
        判定需 Owner/主线确认）、D1（文件额度优先 P0/P1）、320px 视口（`agent-browser` 无
        viewport 命令，标未覆盖）

## 未做项补充登记（本轮核对新发现）

- [x] `dialog.tsx` 关闭键 `width/height: 24` 字面量与 `badge.tsx` `height: 20` 仍非令牌
      → 原因：`audit.md` 的 D2 建议包含它，但「实施结果」只记了 `menuRow`；实际代码两处仍是
        字面量。属 P2 细节，不阻塞本 change 的 P0/P1 验收，转入下一轮（与 C4 的魔法数收敛同批）。
