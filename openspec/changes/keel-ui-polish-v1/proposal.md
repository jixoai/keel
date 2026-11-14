# Proposal: keel-ui-polish-v1

## Why

功能面已经能跑，但界面细节还没有按「上线级产品」过一遍。Owner 指定以
<https://jakub.kr/skills> 的技能集为方法：`better-interface`、`better-ui`、`better-typography`、
`better-colors`、`better-accessibility`、`better-layout`、`better-writing`。这是一条**改进线**，
不改变主线（会话面板 / demo provider / trace 修复）的行为与验收。

## What Changes

- 用上述技能对现有界面做一次系统评审，产出**带优先级的清单**（可访问性 / 布局 / 文案 / 排版 /
  颜色 / 细节）。
- 只实施「不改行为、不改信息架构」的修复：焦点与键盘可达、ARIA 与可读名称、对比度、间距与
  对齐、字号/行高/字距、空态与错误文案、按钮与图标的细节状态。
- 每条修复 MUST 有「问题 → 证据 → 改动 → 回归门」四段；MUST NOT 借机重构组件或改数据流。
- 主线正在改的文件（会话 surface 的 demo 移除、speech TTS 接线）以主线为准，冲突时让位。

## Impact

- 客户端：`web/src/ui/kit/**`、`web/src/screens/**`（样式与文案）。
- 不改变任何接口、协议、OpenSpec 既有 requirement；不改 `internal/**`。
