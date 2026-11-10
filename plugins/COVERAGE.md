# Keel 官方插件覆盖矩阵（对齐 DeepSeek Harness）

> 基准：DSH `deepseek-ai/deepseek-harness`（master，`packages/` 53 组 / 379 包 / 25 bundle，
> 206 默认启用项）＋社区 `dsh-plugin` 生态（`topic` 前 20 ＋ `dsh-plugin in:name` 前 20，
> 2026-11-10 抓取）。keel 的插件 id 一律 `keel.<capability>`。
>
> 状态：`W1` = 本轮交付（内核 + 已有能力面官方插件化 + 示范新插件）；`W2` = 已登记、下一波；
> `W3` = 长期/受限能力；`absent` = 明确不做（必须给理由）。

## A. DSH 能力组 → keel 插件

| DSH 组 | 能力面 | keel 插件 id | kind | 波次 | 状态 |
|---|---|---|---|---|---|
| core | agent loop / session 日志 / 系统提示 / 工具注册表 | `keel.core` | in-process | W1 | planned |
| llm | 提供商中立的模型调用 + 重试 + 计量 | `keel.llm` | in-process | W1 | planned |
| session | 持久化 / 投影 / 标题 / 遥测 / 格式版本 | `keel.session` | in-process | W1 | planned |
| fs | 文件契约 + 本地/沙箱后端 + 文件工具 | `keel.fs` | in-process | W1 | planned |
| shell | bash/pwsh 执行（含持久终端） | `keel.shell` / `keel.shell.pwsh` | in-process | W1 / W2 | planned |
| tools | 工具执行管线（pre/execute/post/guard） | `keel.tools` | in-process | W1 | planned |
| interaction | 命令 / 审批 / 问句 / 权限预设 | `keel.interaction` | in-process | W1 | planned |
| guard | 重复工具提醒 / 单次超时策略 | `keel.guard` | in-process | W1 | planned |
| sandbox | 三档文件效应 + 平台后端 | `keel.sandbox` | in-process | W1 | planned |
| credentials | 凭据引用缝 + env/文件 provider + 授权流 | `keel.credentials` | in-process | W1 | planned |
| api | Remote 层（类型化 Host↔Client + 流 + 访问控制） | `keel.api` | in-process | W1 | planned |
| client | Web GUI（外壳/聊天/设置/插件市场/审批/…） | `keel.client-ui`（NSH 客户端本身） | in-process | W1 | planned |
| host | HTTP/SPA 宿主 / 目录选择 / open-in-app | `keel.host` | in-process | W1 | planned |
| boot | 启动/profile/patch/热重载/插件管理/市场 | `keel.boot` + `keel.plugin-manager` + `keel.plugin-market` | in-process | W1 | planned |
| bundle | 6 个 profile bundle | `keel.bundles`（serve/runtime/headless/tui 四种装配面） | in-process | W2 | planned |
| preset | 按 revision 换工具/提示词/技能 | `keel.preset` | in-process | W2 | planned |
| mcp | MCP server 连接（工具+资源） | `keel.mcp` | in-process | W1 | planned |
| skill | 技能发现/装载/工具 | `keel.skill` | in-process | W1 | planned |
| subagent | 委派 + 进程内外后端 + 工具 | `keel.subagent`（+`.claude-code`/`.codex` 适配器） | in-process/sidecar | W1 / W2 | planned |
| team | 子代理树任务协调 | `keel.team` | in-process | W2 | planned |
| workflow | 模型自写编排脚本扇出 | `keel.workflow` | in-process | W2 | planned |
| jobs | 后台作业控制 | `keel.jobs` | in-process | W1 | planned |
| schedule | Host 定时任务/提醒 | `keel.schedule` | in-process | W1 | planned |
| goal | 每会话持久目标 + 自动续跑 | `keel.goal` | in-process | W1 | planned |
| todo | `todo_write` 工具 | `keel.todo` | in-process | W1 | planned |
| plan | 计划模式 | `keel.plan` | in-process | W1 | planned |
| compaction | 自动压缩 / `/compact` / 结果修剪 | `keel.compaction` | in-process | W1 | planned |
| context | 指令/时间/文件引用/harness 上下文注入 | `keel.context`（+ `keel.context-memory` 新） | in-process | W1 | planned |
| deliverables | 交付物登记（文件/工作区变更） | `keel.deliverables` | in-process | W2 | planned |
| feedback | 会话/消息反馈采集 | `keel.feedback` | in-process | W2 | planned |
| settings | schema 派生配置表单 | `keel.settings` | in-process | W1 | planned |
| storage | 非会话持久存储（域数据） | `keel.storage` | in-process | W1 | planned |
| spill | 长文本溢出到存储 | `keel.spill` | in-process | W1 | planned |
| subprocess | 共享子进程服务 + 本机 provider | `keel.subprocess` | in-process | W1 | planned |
| terminal | 持久终端服务 | `keel.terminal` | in-process | W2 | planned |
| lsp | 代码导航（stdio provider + 工具） | `keel.lsp` | sidecar | W2 | planned |
| ssh | 远端 fs/subprocess/sandbox | `keel.ssh` | sidecar | W3 | planned |
| web | 搜索/抓取 + provider + 工具 | `keel.web` + `keel.web-search`（新） | in-process/sidecar | W1 | planned |
| workspace | workspace 实体与会话成员关系 | `keel.workspace` | in-process | W1 | planned |
| session-query | 历史会话检索/导出 | `keel.session-query` | in-process | W2 | planned |
| attachment | 图片附件能力 | `keel.attachment` | in-process | W1 | planned |
| document | Office→PDF | `keel.document` | sidecar | W3 | planned |
| computer-use | 桌面操作能力缝 | `keel.computer-use` | sidecar | W2 | planned |
| browser-use | 浏览器操作能力缝 | `keel.browser-use` | sidecar | W2 | planned |
| ptc-runtime | 程序化工具调用运行时 | `keel.ptc` | in-process | W2 | planned |
| sdk | 进程外 SDK（JSON-RPC + 客户端） | `keel.sdk` | in-process | W2 | planned |
| acp | Agent Client Protocol | `keel.acp` | in-process | W3 | planned |
| typert | TS 构建期类型图 + Host→Client 类型化调用 | — | — | — | absent（keel 全 Go + 静态客户端，无此构建层；等价能力由 `keel.api` 的生成式 schema 承担） |
| extensions | 运行时自省 + Cordis 运行器 + 动态插件卡（agent 现场写插件） | `keel.creation`（本变更新增） | in-process + webview | W1 | planned |
| experimental | 61 个原型（auto-review/cua/forge/liveshare/voice/stt/翻译/hook 协议/会话搜索/市场…） | 按能力面拆到 W3（见 D 节） | 混合 | W3 | registered |
| util | 共享工具库 | `keel.util` | in-process | W1 | planned |
| identity/telemetry | 匿名 id / OTel | `keel.telemetry` | in-process | W1 | planned |
| test-support | 无密钥测试台 / LLM mock-replay | `keel.testkit` | in-process | W1 | planned |
| vendor/cordis | Cordis 框架本体 | — | — | — | absent（keel 用 agentext + servicegraph 表达同一语义：依赖注入、可逆副作用、可替换 seams；不引入 TS 运行时） |
| apps | CLI / Web / Desktop / 市场生成器 | `keel.cli` / `keel.client-ui` / — | in-process | W1 / W3 | planned（Desktop W3） |

## B. DSH bundle → keel 装配面

| DSH bundle | keel 对应 | 波次 | 状态 |
|---|---|---|---|
| `dsh-base`（核心） | `keel serve` 内置插件集（W1 默认启用） | W1 | planned |
| `dsh-web-app`（GUI） | `keel serve -web-dir` + NSH 客户端 + WebView 插件面 | W1 | planned |
| `dsh-headless` | `cmd/runtime`（无 UI 装配） | W1 | planned |
| `dsh-sdk-app` / `dsh-sdk-minimal` | `keel sdk` 面（JSON-RPC） | W2 | planned |
| `dsh-acp-app` | `keel acp` | W3 | planned |
| 官方实验 bundle（auto-review/cua/forge/voice/cot/plugin-market/inspector/personalized…） | 逐能力面 W3（市场客户端 W1） | W3 | registered |
| `dsh-subagent-claude-code` / `-codex` | `keel.subagent.claude-code` / `.codex` | W2 | planned |

## C. 社区高星能力面 → keel

| 社区项目（★） | 能力面 | keel 插件 id | 波次 | 状态 |
|---|---|---|---|---|
| V1ki/dsh-plugin-subscriptions（579） | 订阅账号变 LLM provider（OAuth） | `keel.llm.subscription` | W3 | registered |
| Tencent/BrowserSkill（10,252） | 已登录真实浏览器供 agent 用 | `keel.browser-use`（接 Browser 产品） | W2 | planned |
| sixtysevenlf/dsh-blender-plugin（1,938） | Blender 直连（GUI 工具） | `keel.blender` | W3 | registered |
| OpenViking / WeKnora（41.8k / 37k） | 记忆 / RAG 知识库 | `keel.context-memory`（W1）/ `keel.knowledge`（W3） | W1 / W3 | planned |
| dsh-market / LivXue shop / hub / marketplace（11.7k 起） | 插件市场（浏览/一键装） | `keel.plugin-market`（索引客户端 W1；托管 W3） | W1 / W3 | planned |
| awesome-dsh-plugin（24k） | 插件索引/榜单 | `keel.plugin-market` 的 index 源 | W1 | planned |
| WYH/DSH-Transparent-UI（435） | 玻璃拟态主题 | `keel.theme.glass`（UI kit 的 solid-glass 令牌） | W2 | planned |
| xuanyuanzhifeng/dsh-plugin-agent-workflow（325） | Agent 工作流 | `keel.workflow` | W2 | planned |
| Noob-stupid/dsh-plugin-gating-hub（125） | 插件门禁/回滚/证据账本 | `keel.plugin-gate` | W2 | planned |
| cpj-dev/dsh-plugin-cc（104） | 与 Claude Code 桥接评审/委派 | `keel.subagent-adapter` | W2 | planned |
| ccch1mneyyy/dsh-Go（17.7k） | 手机本机跑 + 遥控电脑 | `keel.mobile-gateway` | W3 | registered |
| anywhere-labs / dataelement desktop（35k / 17.7k） | 桌面端 | `keel.desktop` | W3 | registered |
| anbeime/skill（9.3k）/ nuwa-skill（37.9k） | 技能商店 / 思维蒸馏 | `keel.skill-store` / `keel.skill.distill` | W2 / W3 | registered |
| poco-ai/agentero（162） | 科研文献库 | `keel.research.literature` | W3 | registered |
| Totoro-qaq/dsh-plugin-bridge（166） | 跨 preset 会话迁移 | `keel.session-migrate` | W3 | registered |

## D. 明确 absent / 延后（带理由）

- `typert`、`vendor/cordis`：TS/Cordis 专属构建与运行时；keel 的等价语义在 agentext + servicegraph，
  移植只会引入第二套插件运行时（违背单一真值）。
- `experimental/*` 61 个原型：逐个评估后按能力面并入 W3；不在 W1 承诺。
- 桌面端 / 移动端 / 公网市场托管：需要独立产品的发布与签名链路，登记 W3。
