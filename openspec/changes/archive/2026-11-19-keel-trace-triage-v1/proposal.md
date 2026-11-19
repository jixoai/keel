# Proposal: keel-trace-triage-v1

## Why

Owner 在真实使用中导出了一份 DevTools 性能/网络 trace（`Trace-20261113T225129.json.gz`），
观察到了异常行为，并指出**可能涉及信息泄露**。trace 是线上视角的事实来源：控制台报错、
失败请求、异常长任务、以及任何被写进 URL/请求头/日志的凭据都会出现在里面。

本 change 的任务不是「猜」，而是把 trace 读成证据：定位每一个异常行为、确认是否存在凭据或
个人数据泄露，并把修复按 OpenSpec 任务落地。

## What Changes

- **T1 取证**：解析 trace（DevTools TraceEvents），整理「异常清单」：console error/warning、
  4xx/5xx 请求、CSP/资源加载失败、长任务与布局抖动、重复请求。
- **T2 泄露审计**：在 trace 全量扫描凭据形态（Authorization/Bearer/cookie/api key/sk-*/token=）、
  用户输入与文件路径；确认是否存在客户端把服务端密钥、会话 cookie 或完整请求头写进日志/DOM/
  URL 的路径。**任何命中只记录脱敏后的位置与类型，不落原文**。
- **T3 代码对表**：把命中的行为映射回 `web/src/**`、`web/host.js`、`internal/**` 的具体路径；
  确认是产品缺陷还是测试/开发构建特有。
- **T4 修复**：按严重度（泄露 > 功能 > 性能）分派修复，每条带可复现与回归测试。
- **T5 验收**：修复后重放同一路径，trace 中不再出现该异常/泄露；报告 con

## Impact

- 依赖：`web/src/**`、`web/host.js`、`internal/httpapi/**`。
- 不修改 `keel-session-surfaces-v1` 与 `keel-demo-provider-v1` 的既有验收。
