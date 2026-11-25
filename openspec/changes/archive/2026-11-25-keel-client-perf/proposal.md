# Proposal: keel-client-perf

> 立项：2026-11-25（P2 客户端产品化：1000 条消息长会话，主线切片）。

## Why

`client-shell` 要求 1000 条消息可交互、滚动保真、离屏增量创建。当前没有可重复的压力样例，
也没有实测证据；这是 P2 剩余的主线项（updater 依赖签名环境，先做 perf）。

## What Changes

1. `?fixture=1&stress=1000` 生成 500 轮（1000 条 user+assistant）压力会话；仅 dev transport。
2. 实测 1440/390：首屏、滚到顶/底、贴底跟随、输入；修掉期间暴露的滚动/渲染缺陷。
3. 门禁与证据：nsh check/build/package、TUI 聚焦回归、截图/数字。

## Impact

- 代码：`web/src/data/fixture.ts`、`store.ts`；可能修 `List` 滚动跟随。
- 不改服务端/协议；默认 fixture 不变。
