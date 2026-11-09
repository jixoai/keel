# Tasks: keel-workspace-polish

> C=客户端 · S=服务端支撑 · Q=验收 · V=复审/报告。P0 先做，P1 视时间；每条都要有截图/断言。

## S. 服务端支撑

- [x] S1 `GET /v1/sessions` 行带 `title`（首条用户消息派生，60 字符内）与 `createdAt`；无消息为空标题
      （internal/agentapi/sessions.go；客户端 enrich 也改为「不拿裸 id 当标题」）
- [x] S2 `POST /v1/sessions/{id}/retry {turnId}`：追加新 run（不改写历史；新 user.message 带
      `retryOf: <原输入 seq>` 作为账本事实；故意不幂等；400/404 语义见 internal/httpapi/retry.go）
- [x] S3 回合视图/事件带分段起止时间：投影 `WorkSegment.startedAt/endedAt` 由事件自带 timestamp 派生
      （`touchSegment`），`TurnView.error` 由 assistant.message/run.finished 的 errorMessage 派生；
      进行中的段允许渲染器用本地时钟，不写回

## C. 客户端（P0）

- [ ] C1 标题：列表与对话头部不出现裸 id；空标题显示「新会话＋相对时间」
- [ ] C2 `waiting` 态：存在未答问句时对话不显示加载动画；列表「等待确认」徽标不点亮未读
- [ ] C3 消息操作：最后一段助手文本 复制/时间；用户消息可复制；重试接 S2
- [ ] C4 逐会话滚动意图与位置记忆；「回到底部」仅脱离且溢出时出现
- [ ] C5 列表按 createdAt 排序、就地更新（仅状态变化不重排）
- [ ] C6 错误横幅（输入框上方、可关闭、重开会话再现、可恢复带重试）
- [ ] C7 单一流式来源；去掉「partial/尚未提交」与「正在思考」文案
- [ ] C8 空态建议只预填

## C. 客户端（P1）

- [ ] C9 工具家族分组＋摘要（数量/失败/已停止）＋结果先行详情
- [ ] C10 分段折叠与计时（工作中/已工作/已停止），最新答案在折叠之外
- [ ] C11 思考流式中折叠＋一行摘要＋完成耗时
- [ ] C12 待发送引导显示在流末尾并标注「等待引导当前任务…」
- [ ] C13 通知按 decisionId 去重，重连回放不重复弹（store 侧已落地：`notifiedDecisions`；
      屏幕侧由 C 项一并验证）
- [ ] C14 空态时间问候

## Q. 验收

- [ ] Q1 真实浏览器走查：首屏标题、等待态、复制/重试、滚动切换、错误横幅、空态预填（截图成对）
- [ ] Q2 真实模型 E2E：重试端点跑通（新 run、旧回合保留）
- [ ] Q3 既有 18 门 + settings change 的 E2E 不退化
- [ ] Q4 TUI 目标可编译、`nsh test --target tui` 绿
- [ ] Q5 改动文件前后截图审计（Owner 2026-11-04 规则）：每个改动文件在覆盖表里有页面/状态归属，
      视觉面产出 before/after 成对截图，UNCOVERED = 0（落 .agents/reports/keel/2026-11-09-workspace-polish/）

## V. 复审与交付

- [ ] V1 子代理对抗复核（体验口径/账本语义不被渲染器污染/无回归）
- [ ] V2 报告（before/after 截图）＋ 公网链接 ＋ archive
