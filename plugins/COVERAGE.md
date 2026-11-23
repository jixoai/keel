# Keel 官方插件覆盖矩阵（对齐 DeepSeek Harness）

> 基准（2026-11-06 亲自核对）：DSH `deepseek-ai/deepseek-harness` master `fcc0ad462172`
> （2026-11-04T08:40:00Z）。`packages/` 实测 **53 个能力组 / 370 个 workspace 包**
> （`packages/<组>/<包>/package.json`；若把 fixtures/templates 也算上，`packages/` 下共
> 379 个 `package.json`）/ **15 个 bundle**（6 个随包 profile bundle：`base`、`web-app`、
> `headless`、`sdk-app`、`sdk-minimal`、`acp-app` ＋ 9 个 `experimental/*-bundle`）。
> web profile 主层 = `dsh-base` 99 行 + `dsh-web-app` 121 行，去重 196 行、171 行启用；
> 另有 4 个 preset patch（standard/ptc/minimal/cordis）。
>
> 社区面（同一天抓取）：GitHub `topic:dsh-plugin` **29,208** 仓、
> `dsh-plugin in:name` **5,854** 仓；按 star 排序各取前 30 逐条核对，C/F 节只收录
> 描述明确是 DSH 插件/市场/技能/桌面端的项目，star 数为抓取时刻值。
>
> 机器可读版本＝`plugins/keel.plugin-market/catalog.json`（**102 条**：53 官方组 + 33 社区 +
> 3 已交付 + 13 规划中的第一方插件），含每条的 `keelPluginId` / `status` / `wave` / `dshGroups`、
> 真实 DSH 包名与来源链接。**catalog 是唯一机读真源**：本文件 §A/§B/§C 的每行都经
> `scripts/plugin-catalog.mjs --check` 映射到 catalog；§H 由 `--write` 生成，禁止手工编辑。
>
> 状态（闭集）：`delivered` = 已交付可安装；`partial` = 部分能力已交付；`planned` = 已登记、
> 下一波；`registered` = 已登记、长期/受限能力；`merged` = 并入其他插件/实体（必须在
> `coverage.note` 或顶层 `retiredIds` 给出目的地）；`absent` = 明确不做（必须给理由）。
> 波次（闭集）：`W0.5` / `W1` / `W2` / `W3` / `never`。

## A. DSH 能力组 → keel 插件

> 「keel 插件 id」列里加粗的是本轮真正交付的插件；同一行其余插件仍是登记状态时，状态列会
> 分开写。每组的完整 DSH 包名见 `plugins/keel.plugin-market/catalog.json` 的 `dshPackages`。

| DSH 组 | 能力面（摘要） | keel 插件 id | kind | 波次 | 状态 |
|---|---|---|---|---|---|
| core | agent loop / session 日志 / 系统提示 / 工具注册表 | `keel.core` | in-process | W1 | planned |
| llm | 提供商中立的模型调用 + 重试 + 计量 | `keel.llm` | in-process | W1 | planned |
| session | 持久化 / 投影 / 标题 / 遥测 / 格式版本 | `keel.session` | in-process | W1 | planned |
| fs | 文件契约 + 本地/沙箱后端 + 文件工具 | `keel.fs` | in-process | W1 | planned |
| shell | bash/pwsh 执行（含持久终端） | `keel.shell` / `keel.shell.pwsh` | in-process | W1 / W2 | planned |
| tools | 工具执行管线（pre/execute/post/guard） | `keel.tool-executor` | in-process | W1 | planned |
| interaction | 命令 / 审批 / 问句 / 权限预设 | `keel.interaction` | in-process | W1 | planned |
| guard | 重复工具提醒 / 单次超时策略 | `keel.supervisor` | in-process | W1 | merged（并入 `keel.supervisor` 护栏） |
| sandbox | 三档文件效应 + 平台后端 | `keel.sandbox` | in-process | W1 | planned |
| credentials | 凭据引用缝 + env/文件 provider + 授权流 | `keel.credentials` | in-process | W1 | planned |
| api | Remote 层（类型化 Host↔Client + 流 + 访问控制） | `keel.api` | in-process | W1 | planned |
| client | Web GUI（外壳/聊天/设置/插件市场/审批/…） | `keel.client-ui`（NSH 客户端本身） | in-process | W1 | planned |
| host | HTTP/SPA 宿主 / 目录选择 / open-in-app | `keel.host` | in-process | W1 | planned |
| boot | 启动/profile/patch/热重载/插件管理/市场 | `keel.boot` + `keel.plugin-manager` + **`keel.plugin-market`** | in-process | W1 | partial（`keel.plugin-market` **delivered**；boot/plugin-manager planned） |
| bundle | 6 个 profile bundle | `keel.bundles`（serve/runtime/headless/tui 四种装配面） | in-process | W2 | planned |
| preset | 按 revision 换工具/提示词/技能 | `keel.presets` | in-process | W2 | planned（由 `keel.preset` 改名） |
| mcp | MCP server 连接（工具+资源） | `keel.mcp` | in-process | W1 | planned |
| skill | 技能发现/装载/工具 | `keel.skills` | in-process | W1 | planned（由 `keel.skill` 改名） |
| subagent | 委派 + 进程内外后端 + 工具 | `keel.subagent`（+`.claude-code`/`.codex` 适配器） | in-process/sidecar | W1 / W2 | planned |
| team | 子代理树任务协调 | — | — | W2 | absent（不设团队协调插件；协调由 `keel.tasks` 父子卡 + `keel.subagent` 承担） |
| workflow | 模型自写编排脚本扇出 | `keel.workflow` | in-process | W2 | planned |
| jobs | 后台作业控制 | `keel.jobs` | in-process | W1 | planned |
| schedule | Host 定时任务/提醒 | `keel.schedule` | in-process | W1 | planned |
| goal | 每会话持久目标 + 自动续跑 | `keel.tasks` | in-process | W1 | merged（目标并入 `keel.tasks`；自动续跑归 `keel.supervisor`） |
| todo | `todo_write` 工具 | `keel.tasks` | in-process | W1 | merged（待办是 `keel.tasks` 的动作） |
| plan | 计划模式 | `keel.tasks` | in-process | W1 | merged（计划模式是任务卡视图） |
| compaction | 自动压缩 / `/compact` / 结果修剪 | `keel.compaction` | in-process | W1 | delivered（压缩核心 + W12 tool-result-pruner/image-offload 已交付；capability，无独立插件实体） |
| context | 指令/时间/文件引用/harness 上下文 | `keel.context` + **`keel.context-memory`** | in-process / sidecar | W1 | `keel.context-memory` **delivered**；`keel.context` planned |
| deliverables | 交付物登记（文件/工作区变更） | `keel.deliverables` | in-process | W2 | planned |
| feedback | 会话/消息反馈采集 | `keel.feedback` | in-process | W2 | planned |
| settings | schema 派生配置表单 | `keel.settings` | in-process | W1 | planned |
| storage | 非会话持久存储（域数据） | `keel.storage` | in-process | W1 | planned |
| spill | 长文本溢出到存储 | `keel.spill` | in-process | W1 | delivered（`internal/spill` + `internal/spillblob`；租户 blob 引用可恢复；内建插件实体） |
| subprocess | 共享子进程服务 + 本机 provider | `keel.subprocess` | in-process | W1 | planned |
| terminal | 持久终端服务 | `keel.terminal` | in-process | W2 | planned |
| lsp | 代码导航（stdio provider + 工具） | `keel.lsp` | sidecar | W2 | planned |
| ssh | 远端 fs/subprocess/sandbox | `keel.ssh` | sidecar | W3 | planned |
| web | 搜索/抓取 + provider + 工具 | `keel.web` + **`keel.web-search`** | in-process / sidecar | W1 | `keel.web-search` **delivered**；`keel.web` planned |
| workspace | workspace 实体与会话成员关系 | — | — | W2 | merged（并入 Group；放置与协作单元，不是插件） |
| session-query | 历史会话检索/导出 | `keel.session-query` | in-process | W2 | planned |
| attachment | 图片附件能力 | `keel.attachment` | in-process | W1 | planned |
| document | Office→PDF | `keel.document` | sidecar | W3 | planned |
| computer-use | 桌面操作能力缝 | `keel.computer-use` | sidecar | W2 | planned |
| browser-use | 浏览器操作能力缝 | `keel.browser-use` | sidecar | W2 | planned |
| ptc-runtime | 程序化工具调用运行时 | `keel.ptc` | in-process | W2 | planned |
| sdk | 进程外 SDK（JSON-RPC + 客户端） | `keel.sdk` | in-process | W2 | planned |
| acp | Agent Client Protocol | `keel.acp` | in-process | W3 | registered |
| typert | TS 构建期类型图 + Host→Client 类型化调用 | — | — | — | absent（keel 全 Go + 静态客户端，无此构建层；等价能力由 `keel.api` 的生成式 schema 承担） |
| extensions | 运行时自省 + Cordis 运行器 + 动态插件卡（agent 现场写插件） | `keel.creation` | in-process + webview | W1 | partial（v1 脚手架/校验/审批安装/列举/删除已交付；v2 扩展面聚合与 Native chunk 主路径未完成） |
| experimental | 61 个原型（auto-review/cua/forge/liveshare/voice/stt/翻译/hook 协议/会话搜索/市场…） | — | — | W3 | absent（不设单体插件；按能力面拆到 W3，见 D 节） |
| util | 共享工具库 | `keel.util` | in-process | W1 | planned |
| identity/telemetry | 匿名 id / OTel | `keel.telemetry` | in-process | W1 | planned |
| test-support | 无密钥测试台 / LLM mock-replay | `keel.testkit` | in-process | W1 | planned |
| vendor/cordis | Cordis 框架本体 | — | — | — | absent（keel 用 agentext + servicegraph 表达同一语义：依赖注入、可逆副作用、可替换 seams；不引入 TS 运行时） |
| apps | CLI / Web / Desktop / 市场生成器 | `keel.cli` / `keel.client-ui` / — | in-process | W1 / W3 | planned（Desktop W3） |

## B. DSH bundle → keel 装配面

> 实测口径：bundle 包 = `packages/bundle/*`（6 个）＋ `packages/experimental/*-bundle`
> （9 个）；`apps/web/tests/fixtures/plugins/fixture-bundle` 是测试夹具，不计入。

| DSH bundle | keel 对应 | 波次 | 状态 |
|---|---|---|---|
| `dsh-base`（核心） | `keel.bundles`（base bundle）+ `keel serve` 内置插件集（W1 默认启用） | W1 | planned |
| `dsh-web-app`（GUI） | `keel.bundles`（web mode）+ `keel serve -web-dir` + NSH 客户端 + WebView 插件面 | W1 | planned |
| `dsh-headless` | `keel.bundles`（headless mode）+ `cmd/runtime`（无 UI 装配） | W1 | planned |
| `dsh-sdk-app` / `dsh-sdk-minimal` | `keel.bundles`（sdk mode）+ `keel sdk` 面（JSON-RPC） | W2 | planned |
| `dsh-acp-app` | `keel.bundles`（acp mode）+ `keel acp` | W3 | planned |
| 官方实验 bundle（badge-skill / cot-translation / cua / forge / plugin-market / ralph / session-titles / terminal / voice-input） | `keel.bundles` 的实验 row；逐能力面 W3（市场索引 W1 已交付） | W3 | registered |
| `dsh-subagent-claude-code` / `-codex` | `keel.subagent.claude-code` / `.codex` | W2 | planned |

## C. 社区高星能力面 → keel

> star 数为 2026-11-06 抓取值（GitHub API）；仓库名已按实际仓名核对（旧表里
> `WYH/DSH-Transparent-UI`、`poco-ai/agentero` 是简称/误名，已修正为真实仓名）。

| 社区项目（★） | 能力面 | keel 插件 id | 波次 | 状态 |
|---|---|---|---|---|
| V1ki/dsh-plugin-subscriptions（580） | 订阅账号变 LLM provider（OAuth） | `keel.llm.subscription` | W3 | registered |
| Tencent/BrowserSkill（10,261） | 已登录真实浏览器供 agent 用 | `keel.browser-use`（接 Browser 产品） | W2 | planned |
| sixtysevenlf/dsh-blender-plugin（1,941） | Blender 直连（GUI 工具，15 个工具） | `keel.blender` | W3 | registered |
| volcengine/OpenViking（41,897）/ Tencent/WeKnora（37,053） | 记忆 / RAG 知识库 | `keel.context-memory`（W1，已交付）/ `keel.knowledge`（W3） | W1 / W3 | delivered / registered |
| dsh-market/dsh-market（11,741）/ LivXue/dsh-plugin-shop（663）/ dshplugin/dsh-plugin-hub（503） | 插件市场（浏览/一键装） | `keel.plugin-market`（索引客户端 W1，已交付；托管 W3） | W1 / W3 | delivered / registered |
| awesome-dsh-plugin/awesome-dsh-plugin（24,105）/ AdamPlatin123/dsh-plugin-radar（1,498）/ polang233/dsh-plugin-index（212） | 插件索引/榜单 | `keel.plugin-market` 的索引来源 | W1 | delivered（索引源） |
| WYH66666666/DSH-Transparent-UI-Plugin（435） | 玻璃拟态主题 | `keel.theme.glass`（UI kit 的 solid-glass 令牌） | W2 | planned |
| xuanyuanzhifeng/dsh-plugin-agent-workflow（326） | Agent 工作流 | `keel.workflow` | W2 | planned |
| Noob-stupid/dsh-plugin-gating-hub（125） | 插件门禁/回滚/证据账本 | `keel.plugin-gate` | W2 | planned |
| cpj-dev/dsh-plugin-cc（104） | 与 Claude Code 桥接评审/委派 | `keel.subagent-adapter` | W2 | planned |
| ccch1mneyyy/dsh-Go（17,966） | 手机本机跑 + 遥控电脑 | `keel.mobile-gateway` | W3 | registered |
| anywhere-labs/dsh-desktop（35,563）/ dataelement/dsh-desktop（17,789） | 桌面端 | `keel.desktop` | W3 | registered |
| anbeime/skill（9,363）/ alchaincyf/nuwa-skill（37,929） | 技能商店 / 思维蒸馏 | `keel.skill-store` / `keel.skill.distill` | W2 / W3 | registered |
| poco-ai/agentero-dsh-plugin（169） | 科研文献库 | `keel.research.literature` | W3 | registered |
| Totoro-qaq/dsh-plugin-bridge（166） | 跨 preset 会话迁移 | `keel.session-migrate` | W3 | registered |

## D. 明确 absent / 延后（带理由）

- `typert`、`vendor/cordis`：TS/Cordis 专属构建与运行时；keel 的等价语义在 agentext + servicegraph，
  移植只会引入第二套插件运行时（违背单一真值）。
- `experimental/*` 61 个原型：逐个评估后按能力面并入 W3；不在 W1 承诺。
- 桌面端 / 移动端 / 公网市场托管：需要独立产品的发布与签名链路，登记 W3。

### D.1 合并/废弃 id 的逐条处置（`catalog.retiredIds`）

| 旧 id | 处置 | 目的地与理由 |
|---|---|---|
| `keel.goal` | merged | `keel.tasks`（目标即任务卡）；自动续跑归 `keel.supervisor` |
| `keel.todo` | merged | `keel.tasks` 的动作 |
| `keel.plan` | merged | `keel.tasks` 的计划视图 |
| `keel.team` | absent | 不做团队协调插件；由 `keel.tasks` 父子卡 + `keel.subagent` 承担 |
| `keel.workspace` | merged | Group（放置与协作单元，不是插件） |
| `keel.guard` | merged | `keel.supervisor`（重复提醒/超时策略并入护栏） |
| `keel.skill` | merged | `keel.skills`（复数统一，能力面不变） |
| `keel.preset` | merged | `keel.presets`（复数统一，能力面不变） |
| `keel.tools` | merged | `keel.tool-executor`（工具执行管线） |

### D.2 catalog 独有 id（不在 `plugin-inventory.md` §3 的 60 项设计清单内）

这些 id 是 §A/§C 已登记的覆盖目标，保留在 catalog 中并逐条给出处置；状态与波次以 catalog
的 `status`/`wave` 为准，不因「设计清单未列」而删除。

| id | 处置 | 理由 |
|---|---|---|
| `keel.core` | 保留（W1 planned） | §A core 行；内核 seam 的装配名 |
| `keel.interaction` | 保留（W1 planned） | §A interaction 行 |
| `keel.session` | 保留（W1 planned） | §A session 行 |
| `keel.ssh` | 保留（W3 planned） | §A ssh 行；远端执行面走自研契约 |
| `keel.sandbox` | 保留（W1 planned） | §A sandbox 行；本 change 不新增沙箱要求 |
| `keel.sdk` | 保留（W2 planned） | §A sdk 行 |
| `keel.api` | 保留（W1 planned） | §A api 行；等价 typert 的生成式 schema |
| `keel.client-ui` | 保留（W1 planned） | §A client 行；NSH 客户端本身 |
| `keel.host` | 保留（W1 planned） | §A host 行 |
| `keel.util` | 保留（W1 planned） | §A util 行；共享工具库 |
| `keel.acp` | 保留（W3 registered） | §A acp 行；登记不承诺 |
| `keel.desktop` | 保留（W3 registered） | §C 桌面端；需独立发布/签名链路 |
| `keel.mobile-gateway` | 保留（W3 registered） | §C 移动接入；登记不承诺 |
| `keel.llm.subscription` | 保留（W3 registered） | §C 订阅账号 provider；第三方额度不在官方承诺内 |
| `keel.plugin-gate` | 保留（W2 planned） | §C 插件门禁/回滚/证据账本 |
| `keel.session-migrate` | 保留（W3 registered） | §C 跨 preset 会话迁移 |
| `keel.research.literature` | 保留（W3 registered） | §C 科研文献库 |
| `keel.blender` | 保留（W3 registered） | §C Blender 直连 |
| `keel.theme.glass` | 保留（W2 planned） | §C 玻璃主题；落在 solid-glass 令牌 |
| `keel.subagent-adapter` | 保留（W2 planned） | §C Claude Code 桥接；与 `keel.oar` 同族 |

## E. 已交付的官方插件（可安装）

三个插件都是 `kind: sidecar`，零依赖 Node（≥20）脚本，随仓库 `plugins/` 分发；包结构校验用
`go run ./plugins/tools/verifypkg`（内核真校验器 + 真握手 + 真 `tools/list`）。

### `keel.web-search` — 联网搜索

- 工具（宿主发布名 / 侧车本地名）：`keel_web-search_search` / `search`（命名规则：
  `keel.web-search` + `search`，`.`→`_`、`-` 保留；任务书规范名 `keel_web_search_search`
  是同规则下插件 id 无连字符的写法）
- 依赖：`node`（PATH 上，≥20）；出网主机 `www.bing.com`、`cn.bing.com`、
  `html.duckduckgo.com`、`hn.algolia.com`、`api.github.com`、`registry.npmjs.org`
- 权限：`permissions.net`（上述 6 个 host）；无 secrets / fs
- 安装：`POST /v1/plugins/install {"path":"<repo>/plugins/keel.web-search"}`（202 → 答 allow →
  带同一 `requestId` 的 `decisionId` 重发 → 201）
- 验证：`plugins/keel.web-search/README.md` 的 stdio 手测（真返回 Bing 结果；断网/被墙路径
  返回 `isError:true` + 每个上游的 `dns_error`/`tls_error`/`timeout`）。

### `keel.context-memory` — 上下文记忆

- 工具：`keel_context-memory_remember` / `keel_context-memory_recall`（侧车本地名
  `remember` / `recall`）
- 依赖：`node`（≥20）；本地磁盘（插件自己的数据目录）
- 权限：`permissions.fs: ["plugin-data"]`；无 net / secrets
- 存储：`<datadir>/memory.ndjson`；数据目录 = `KEEL_CONTEXT_MEMORY_DIR` ＞
  `<plugins root>/.data/keel.context-memory` ＞ `<cwd>/.data`
- 安装：`POST /v1/plugins/install {"path":"<repo>/plugins/keel.context-memory"}`（同上审批路径）
- 验证：README 的 2 写 + 2 读手测；并发 20 写全部成功、`count: 20`；重启后
  `22 entries / 1 corrupt line` 仍可读（坏行隔离）。

### `keel.plugin-market` — 官方插件市场索引

- 工具：`keel_plugin-market_search` / `keel_plugin-market_info`（侧车本地名
  `search` / `info`）
- 依赖：`node`（≥20）；包内静态索引 `catalog.json`（102 条）
- 权限：`permissions.fs: ["read:plugin-package"]`；无 net / secrets
- 安装：`POST /v1/plugins/install {"path":"<repo>/plugins/keel.plugin-market"}`（同上审批路径）
- 验证：README 的 `search "memory"`（4 条）、`info "dsh.web"`（partial → `keel.web-search`）、
  `status=["delivered"]`（4 条）；未命中返回 `isError:true`。

安装/启用都走 durable 审批（202 → 答 allow → 带同一 `requestId` 的 `decisionId` 重发 →
201/200）；真实 `keel serve` 的完整 curl 序列、桥调用与进程回收证据见本次交付汇报（W1
走查记录）与各插件 README 的「手工验证」节。

## F. 官方/社区核对样本（各 15 条，附来源链接）

### F.1 DSH 官方能力组（15 条；包名为 `packages/<组>/` 下实测）

| # | 组 | 组内真实包（节选） | 来源 |
|---|---|---|---|
| 1 | core | `agent`, `agent-loop`, `session`, `system-prompt`, `tool-executor-native`, `tool-executor-ptc`, `tools` | https://github.com/deepseek-ai/deepseek-harness/tree/master/packages/core |
| 2 | llm | `llm`, `llm-deepseek`, `llm-deepseek-api-key`, `llm-deepseek-account`, `llm-retry`, `token-meter` | https://github.com/deepseek-ai/deepseek-harness/tree/master/packages/llm |
| 3 | session | `session-log-deepseek`, `session-persistence`, `session-projection`, `session-format-v0-to-v1`…`v3-to-v4`, `session-title` | https://github.com/deepseek-ai/deepseek-harness/tree/master/packages/session |
| 4 | fs | `fs`, `fs-local`, `fs-sandbox`, `fs-patch`, `tool-fs`, `tool-fs-search`, `tool-str-replace-editor` | https://github.com/deepseek-ai/deepseek-harness/tree/master/packages/fs |
| 5 | shell | `bash-local`, `bash-sandbox`, `pwsh-local`, `pwsh-sandbox`, `tool-bash`, `tool-bash-persistent` | https://github.com/deepseek-ai/deepseek-harness/tree/master/packages/shell |
| 6 | web | `web`, `web-search-deepseek`, `web-fetch-http`, `web-deepseek-api`, `tool-web` | https://github.com/deepseek-ai/deepseek-harness/tree/master/packages/web |
| 7 | context | `agent-instructions`, `file-reference`, `file-reference-local`, `session-reference`, `time-context`, `tmux-context` | https://github.com/deepseek-ai/deepseek-harness/tree/master/packages/context |
| 8 | mcp | `mcp-client`, `mcp-manager`, `mcp-resources` | https://github.com/deepseek-ai/deepseek-harness/tree/master/packages/mcp |
| 9 | subagent | `subagent`, `subagent-claude-code`, `subagent-codex`, `subagent-acp`, `subagent-dsh-sdk`, `tool-subagent` | https://github.com/deepseek-ai/deepseek-harness/tree/master/packages/subagent |
| 10 | interaction | `commands`, `permission-presets`, `tool-ask-user`, `user-approval`, `user-questions` | https://github.com/deepseek-ai/deepseek-harness/tree/master/packages/interaction |
| 11 | boot | `app-boot`, `cmdline`, `config-editor`, `hmr`, `plugin-manager`, `plugin-market` | https://github.com/deepseek-ai/deepseek-harness/tree/master/packages/boot |
| 12 | client | 70 个 UI 包，含 `ui-chat`、`ui-settings-plugins`、`ui-plugin-market`、`ui-approval`、`ui-theme` | https://github.com/deepseek-ai/deepseek-harness/tree/master/packages/client |
| 13 | sandbox | `sandbox`, `sandbox-local`, `sandbox-policy`, `sandbox-windows-acl` | https://github.com/deepseek-ai/deepseek-harness/tree/master/packages/sandbox |
| 14 | credentials | `credentials`, `credentials-local`, `authorization`, `deepseek-account`, `deepseek-account-platform` | https://github.com/deepseek-ai/deepseek-harness/tree/master/packages/credentials |
| 15 | bundle | `base`, `web-app`, `headless`, `sdk-app`, `sdk-minimal`, `acp-app`（+ 9 个 experimental `*-bundle`） | https://github.com/deepseek-ai/deepseek-harness/tree/master/packages/bundle |

### F.2 社区靠前（15 条；star 数为 2026-11-06 抓取）

| # | 仓库（★） | 一句话 | 来源 |
|---|---|---|---|
| 1 | `awesome-dsh-plugin/awesome-dsh-plugin`（24,105） | DSH 插件精选列表 | https://github.com/awesome-dsh-plugin/awesome-dsh-plugin |
| 2 | `sixtysevenlf/dsh-blender-plugin`（1,941） | Blender 直连（15 个模型工具） | https://github.com/sixtysevenlf/dsh-blender-plugin |
| 3 | `AdamPlatin123/dsh-plugin-radar`（1,498） | 插件生态雷达/分类 | https://github.com/AdamPlatin123/dsh-plugin-radar |
| 4 | `LivXue/dsh-plugin-shop`（663） | 每日刷新的插件市场 | https://github.com/LivXue/dsh-plugin-shop |
| 5 | `V1ki/dsh-plugin-subscriptions`（580） | 订阅账号变 LLM provider | https://github.com/V1ki/dsh-plugin-subscriptions |
| 6 | `bruc3van/awesome-dsh-plugin`（508） | 每日抓取复核的插件索引 | https://github.com/bruc3van/awesome-dsh-plugin |
| 7 | `dshplugin/dsh-plugin-hub`（503） | 插件市场/插件大全 | https://github.com/dshplugin/dsh-plugin-hub |
| 8 | `WYH66666666/DSH-Transparent-UI-Plugin`（435） | 玻璃质感主题 | https://github.com/WYH66666666/DSH-Transparent-UI-Plugin |
| 9 | `xuanyuanzhifeng/dsh-plugin-agent-workflow`（326） | Agent 工作流 | https://github.com/xuanyuanzhifeng/dsh-plugin-agent-workflow |
| 10 | `oh-my-dsh/dsh-plugin-upgrade-skill`（318） | 插件随 dsh 版本升级 | https://github.com/oh-my-dsh/dsh-plugin-upgrade-skill |
| 11 | `lili-luo/dsh-plugin-bundle`（256） | 插件组合包 | https://github.com/lili-luo/dsh-plugin-bundle |
| 12 | `awesome-dsh-plugin/dsh-find-plugin`（230） | agent 内实时搜插件 | https://github.com/awesome-dsh-plugin/dsh-find-plugin |
| 13 | `polang233/dsh-plugin-index`（212） | 开放插件市场索引 | https://github.com/polang233/dsh-plugin-index |
| 14 | `poco-ai/agentero-dsh-plugin`（169） | 科研文献库 | https://github.com/poco-ai/agentero-dsh-plugin |
| 15 | `Totoro-qaq/dsh-plugin-bridge`（166） | 跨 preset 会话迁移 | https://github.com/Totoro-qaq/dsh-plugin-bridge |

## G. 核对方法与未能核实的条目

- 官方面：`gh api repos/deepseek-ai/deepseek-harness/git/trees/master?recursive=1`，统计
  `packages/**/package.json`；53 组、370 workspace 包、15 bundle、web profile 行数均由此树与
  `packages/bundle/*/cordis.patch.yml` 直接计数得出（commit `fcc0ad462172`）。
- 社区面：`gh api search/repositories?q=topic:dsh-plugin&sort=stars` 与
  `q=dsh-plugin+in:name&sort=stars`，前 30 条逐条读 description 后收录；未收录的 topic 命中
  多为泛 agent/设计工具（如 `morluto/rea`、`nexu-io/open-design`），无法确认是 DSH 插件。
- **未能核实（如实标注，不编造）**：
  - 旧文件头里的“25 bundle”“206 默认启用项”“379 包”无法在 `fcc0ad462172` 上复现到同一
    口径（实测为 15 bundle、370 workspace 包 / 379 个 `package.json`、web profile 196 行 /
    171 行启用）。本文已改用可复现口径；旧数字不再引用。
  - 社区项目的 star 数随时变化；本表只是 2026-11-06 的快照。
  - 部分社区仓库是否“可直接 `dsh plugin` 安装”未逐个真机安装验证（只核对了仓库描述与
    插件形态）；`keel.plugin-market` 的 `source.discovery` 记录了抓取口径。
  - 官方 61 个 experimental 原型未逐个评估（只按能力面登记 W3）。

<!-- keel-plugin-catalog:begin -->
## H. Catalog 机读真源（自动生成）

> 本段由 `node scripts/plugin-catalog.mjs --write` 生成，`--check` 校验；
> 禁止手工编辑。`plugins/keel.plugin-market/catalog.json` 是唯一机读真源。

| 条目 | keel 插件 id | 状态 | 波次 | dshGroups |
|---|---|---|---|---|
| `community.AdamPlatin123.dsh-plugin-radar` | `keel.plugin-market` | partial | W1 | — |
| `community.alchaincyf.nuwa-skill` | `keel.skill.distill` | registered | W3 | — |
| `community.anbeime.skill` | `keel.skill-store` | registered | W2 | — |
| `community.anywhere-labs.dsh-desktop` | `keel.desktop` | registered | W3 | — |
| `community.awesome-dsh-plugin.awesome-dsh-plugin` | `keel.plugin-market` | partial | W1 | — |
| `community.awesome-dsh-plugin.dsh-find-plugin` | `keel.plugin-market` | partial | W1 | — |
| `community.bradeGithub.DSH-Plugins-Marketplace` | `keel.plugin-market` | partial | W1 | — |
| `community.bruc3van.awesome-dsh-plugin` | `keel.plugin-market` | partial | W1 | — |
| `community.ccch1mneyyy.dsh-Go` | `keel.mobile-gateway` | registered | W3 | — |
| `community.Clarklevis1995.dsh-plugin-mobile-gateway` | `keel.mobile-gateway` | registered | W3 | — |
| `community.cpj-dev.dsh-plugin-cc` | `keel.subagent-adapter` | planned | W2 | — |
| `community.dataelement.dsh-desktop` | `keel.desktop` | registered | W3 | — |
| `community.dsh-market.dsh-market` | `keel.plugin-market` | partial | W1 | — |
| `community.dshplugin.dsh-plugin-hub` | `keel.plugin-market` | partial | W1 | — |
| `community.Ebony-Vinyl.dsh-our-free-model` | `keel.llm.subscription` | registered | W3 | — |
| `community.EverMind-AI.EverOS` | `keel.context-memory` | delivered | W1 | — |
| `community.lili-luo.dsh-plugin-bundle` | `keel.bundles` | planned | W2 | — |
| `community.LivXue.dsh-plugin-shop` | `keel.plugin-market` | partial | W1 | — |
| `community.luobosibing2.dsh-jev-plugin` | `keel.interaction` | planned | W2 | — |
| `community.MemTensor.MemOS` | `keel.context-memory` | partial | W1 | — |
| `community.Nagi-ovo.dsh-find-plugins` | `keel.plugin-market` | partial | W1 | — |
| `community.Noob-stupid.dsh-plugin-gating-hub` | `keel.plugin-gate` | registered | W2 | — |
| `community.oh-my-dsh.dsh-plugin-upgrade-skill` | `keel.plugin-gate` | registered | W2 | — |
| `community.poco-ai.agentero-dsh-plugin` | `keel.research.literature` | registered | W3 | — |
| `community.polang233.dsh-plugin-index` | `keel.plugin-market` | partial | W1 | — |
| `community.sixtysevenlf.dsh-blender-plugin` | `keel.blender` | registered | W3 | — |
| `community.Tencent.BrowserSkill` | `keel.browser-use` | planned | W2 | — |
| `community.Tencent.WeKnora` | `keel.knowledge` | registered | W3 | — |
| `community.Totoro-qaq.dsh-plugin-bridge` | `keel.session-migrate` | registered | W3 | — |
| `community.V1ki.dsh-plugin-subscriptions` | `keel.llm.subscription` | registered | W3 | — |
| `community.volcengine.OpenViking` | `keel.knowledge` | registered | W3 | — |
| `community.WYH66666666.DSH-Transparent-UI-Plugin` | `keel.theme.glass` | planned | W2 | — |
| `community.xuanyuanzhifeng.dsh-plugin-agent-workflow` | `keel.workflow` | planned | W2 | — |
| `dsh.acp` | `keel.acp` | registered | W3 | `dsh.acp` |
| `dsh.api` | `keel.api` | planned | W1 | `dsh.api` |
| `dsh.attachment` | `keel.attachment` | planned | W1 | `dsh.attachment` |
| `dsh.boot` | `keel.plugin-market` | partial | W1 | `dsh.boot` |
| `dsh.browser-use` | `keel.browser-use` | planned | W2 | `dsh.browser-use` |
| `dsh.bundle` | `keel.bundles` | planned | W2 | `dsh.bundle` |
| `dsh.client` | `keel.client-ui` | planned | W1 | `dsh.client` |
| `dsh.compaction` | `keel.compaction` | planned | W1 | `dsh.compaction` |
| `dsh.computer-use` | `keel.computer-use` | planned | W2 | `dsh.computer-use` |
| `dsh.context` | `keel.context-memory` | partial | W1 | `dsh.context` |
| `dsh.core` | `keel.core` | planned | W1 | `dsh.core` |
| `dsh.credentials` | `keel.credentials` | planned | W1 | `dsh.credentials` |
| `dsh.deliverables` | `keel.deliverables` | planned | W2 | `dsh.deliverables` |
| `dsh.document` | `keel.document` | planned | W3 | `dsh.document` |
| `dsh.experimental` | — | absent | W3 | `dsh.experimental` |
| `dsh.extensions` | `keel.creation` | partial | W1 | `dsh.extensions` |
| `dsh.feedback` | `keel.feedback` | planned | W2 | `dsh.feedback` |
| `dsh.fs` | `keel.fs` | planned | W1 | `dsh.fs` |
| `dsh.goal` | `keel.tasks` | merged | W1 | `dsh.goal` |
| `dsh.guard` | `keel.supervisor` | merged | W1 | `dsh.guard` |
| `dsh.host` | `keel.host` | planned | W1 | `dsh.host` |
| `dsh.identity` | `keel.telemetry` | planned | W1 | `dsh.identity` |
| `dsh.interaction` | `keel.interaction` | planned | W1 | `dsh.interaction` |
| `dsh.jobs` | `keel.jobs` | planned | W1 | `dsh.jobs` |
| `dsh.llm` | `keel.llm` | planned | W1 | `dsh.llm` |
| `dsh.lsp` | `keel.lsp` | planned | W2 | `dsh.lsp` |
| `dsh.mcp` | `keel.mcp` | planned | W1 | `dsh.mcp` |
| `dsh.plan` | `keel.tasks` | merged | W1 | `dsh.plan` |
| `dsh.preset` | `keel.presets` | planned | W2 | `dsh.preset` |
| `dsh.ptc-runtime` | `keel.ptc` | planned | W2 | `dsh.ptc-runtime` |
| `dsh.sandbox` | `keel.sandbox` | planned | W1 | `dsh.sandbox` |
| `dsh.schedule` | `keel.schedule` | planned | W1 | `dsh.schedule` |
| `dsh.sdk` | `keel.sdk` | planned | W2 | `dsh.sdk` |
| `dsh.session` | `keel.session` | planned | W1 | `dsh.session` |
| `dsh.session-query` | `keel.session-query` | planned | W2 | `dsh.session-query` |
| `dsh.settings` | `keel.settings` | planned | W1 | `dsh.settings` |
| `dsh.shell` | `keel.shell` | planned | W1 | `dsh.shell` |
| `dsh.skill` | `keel.skills` | planned | W1 | `dsh.skill` |
| `dsh.spill` | `keel.spill` | planned | W1 | `dsh.spill` |
| `dsh.ssh` | `keel.ssh` | planned | W3 | `dsh.ssh` |
| `dsh.storage` | `keel.storage` | planned | W1 | `dsh.storage` |
| `dsh.subagent` | `keel.subagent` | planned | W1 | `dsh.subagent` |
| `dsh.subprocess` | `keel.subprocess` | planned | W1 | `dsh.subprocess` |
| `dsh.team` | — | absent | never | `dsh.team` |
| `dsh.telemetry` | `keel.telemetry` | planned | W1 | `dsh.telemetry` |
| `dsh.terminal` | `keel.terminal` | planned | W2 | `dsh.terminal` |
| `dsh.test-support` | `keel.testkit` | planned | W1 | `dsh.test-support` |
| `dsh.todo` | `keel.tasks` | merged | W1 | `dsh.todo` |
| `dsh.typert` | — | absent | never | `dsh.typert` |
| `dsh.util` | `keel.util` | planned | W1 | `dsh.util` |
| `dsh.web` | `keel.web-search` | partial | W1 | `dsh.web` |
| `dsh.workflow` | `keel.workflow` | planned | W2 | `dsh.workflow` |
| `dsh.workspace` | — | merged | W2 | `dsh.workspace` |
| `keel.boot` | `keel.boot` | planned | W1 | `dsh.boot` |
| `keel.cli` | `keel.cli` | planned | W1 | — |
| `keel.context` | `keel.context` | planned | W1 | `dsh.context` |
| `keel.context-memory` | `keel.context-memory` | delivered | W1 | `dsh.context` |
| `keel.oar` | `keel.oar` | partial | W3 | — |
| `keel.plugin-manager` | `keel.plugin-manager` | planned | W1 | `dsh.boot` |
| `keel.plugin-market` | `keel.plugin-market` | delivered | W1 | `dsh.boot`, `dsh.util` |
| `keel.presets` | `keel.presets` | planned | W2 | `dsh.preset` |
| `keel.shell.pwsh` | `keel.shell.pwsh` | planned | W2 | `dsh.shell` |
| `keel.skills` | `keel.skills` | planned | W1 | `dsh.skill` |
| `keel.subagent.claude-code` | `keel.subagent.claude-code` | planned | W2 | `dsh.subagent` |
| `keel.supervisor` | `keel.supervisor` | planned | W1 | `dsh.guard`, `dsh.goal` |
| `keel.tasks` | `keel.tasks` | planned | W1 | `dsh.goal`, `dsh.todo`, `dsh.plan` |
| `keel.tool-executor` | `keel.tool-executor` | planned | W1 | `dsh.core` |
| `keel.ui` | `keel.ui` | planned | W1 | `dsh.client` |
| `keel.web` | `keel.web` | planned | W1 | `dsh.web` |
| `keel.web-search` | `keel.web-search` | delivered | W1 | `dsh.web` |

<!-- keel-plugin-catalog:end -->
