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

- [x] C1 标题：列表与对话头部不出现裸 id；空标题显示「新会话＋相对时间」（列表 titleOf；对话头部同口径；搜索不再按 id 命中，避免「有结果无高亮」）
- [x] C2 `waiting` 态：存在未答问句时对话不显示加载动画（run-state 词表 waiting；思考块不转圈、段头不称工作中）；列表「等待确认」徽标＋静止琥珀标记（不点亮未读）
- [x] C3 消息操作：复制（回合合并纯文本）/ 时间 / 重试（走 `POST /v1/sessions/{id}/retry {turnId}`，turnId 来自投影 `t_<seq>`；pending 转圈禁用）；用户消息同款
- [x] C4 逐会话滚动意图与位置记忆（store.scrollState/saveScrollState＋maintainVisibleContentPosition）；「回到底部」仅脱离且溢出时出现；恢复期间的列表自举报位不被误当用户意图
- [x] C5 列表按 createdAt 排序（新行插顶部）、就地更新（已存在行保序更新，不重排）
- [x] C6 错误横幅（输入框上方；原因逐字来自账本；可关闭（本次打开内不再弹、重开会话再现）；有已提交输入才给重试）
- [x] C7 单一流式来源（同一条 AssistantText，只差光标）；「partial/尚未提交」「正在思考」全仓清除
- [x] C8 空态建议只预填（prefillComposer；TUI 断言 `app.value` 等于建议原文、空态仍在；发送失败把草稿与附件还给用户）

## C. 客户端（P1）

- [x] C9 工具家族分组（≥2 成组）＋摘要「N 个工具/命令，M 个失败｜全部完成｜X 个未完成」＋详情结果先行（参数 key=value）
- [x] C10 分段折叠与计时：段头「工作中 N 秒 / 已工作 N 秒 / 已停止」（事件时间戳派生，单 run 回退 runSummary.durationMs；进行中才用本地时钟，不写回）；最新答案在折叠之外
- [x] C11 思考：默认折叠，流式一行摘要 `Thinking · <最新一行>`，完成 `Thought · N 秒`
- [x] C12 待发送引导显示在流末尾（「等待引导当前任务…」＋引导原文＋注入说明；停止中 warning 版）；输入框上方的旧横幅整块删除
- [x] C13 通知按 decisionId 去重（`notifiedDecisions`；重连回放不重复弹）
- [x] C14 空态时间问候（早上好/中午好/下午好/晚上好/夜深了）

## Q. 验收

- [x] Q1 真实浏览器走查：首屏标题、等待态、复制/重试、滚动切换、错误横幅、空态预填（演示 9 张 + 线上 5 张截图，before/after 成对）
- [x] Q2 真实模型 E2E：重试端点跑通（25 PASS：retryOf 唯一、新 run 终态、原 13 条事件原位）+ hermetic 用例 8/8
- [x] Q3 全量验收电池 16/16（含 perf 549.6MB / 双进程 543.9MB）+ settings E2E 25 PASS 不退化
- [x] Q4 TUI 目标可编译、`nsh test --target tui` 7/7（含打磨门）
- [x] Q5 改动文件前后截图审计：59 个改动文件逐条归属（有页面/状态或显式「无视觉面①」），before/after 成对，UNCOVERED = 0（.agents/reports/keel/2026-11-09-workspace-polish/report.html §8）

## V. 复审与交付

- [x] V1 子代理对抗复核（体验口径/账本语义不被渲染器污染/无回归）：客户端/真值面 8/10；
      阻塞项「C2 在工具面的加载动效未闭合」已修（waiting 门全链 + 两条状态样本 + TUI 断言），
      非阻塞项（无输入回合不给重试、压缩空壳、TUI 等待标记静止、enrich 空跑、裸 id 审计）全部落地
- [x] V2 报告（before/after 截图）＋ 公网链接 ＋ archive
      （https://local-gaubee-keel-polish.fde2e.com/report.html · 2026-11-10 归档）
