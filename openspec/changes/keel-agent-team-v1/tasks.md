# Tasks: keel-agent-team-v1

> 先冻结 Agent / AgentTeam / 插件能力树契约，再实现插件与 UI。UI 先交付部件
> （Agent 卡/编辑器、工具选择器、团队画布、启动模式、能力树），再组合设置页与会话启动。

## P0 契约与设计冻结

- [ ] 冻结 `user-requirements.md` 原话记录与裁决映射（模型预设 → AgentTeam；语音 TTS/STT；
      Agent 名字/头像/默认模型/工具/AGENTS.md；团队队长必选；CLI 能力树）
- [ ] 冻结 Agent schema：id/name/description/avatar/defaultModel/tools/systemPrompt/
      skills/permissions/teamCli
- [ ] 冻结 AgentTeam schema：captain（必选、唯一）、members（0..N）、team 配置文件
      （DB-backed）、teamCli 能力开关、session 快照
- [ ] 冻结插件能力树 schema：path/summary/description/args_schema/output_schema/
      permissions/danger/idempotent/callable_by/examples
- [ ] 冻结会话启动模式：standard（选模型）| team（选团队）
- [ ] 冻结工具注册源：toolId/来源插件/schema/权限/danger/callable_by
- [ ] 冻结 team 配置是 DB-backed 文件、CLI 修改 team 配置、不污染 model/agent provider
- [ ] **Owner 1A：旧模型预设直接删除，不做迁移/兼容层**；删除旧表/API/UI 与单模型下拉的旧配置路径
- [ ] 部件级设计：Agent 卡/编辑器、头像、工具选择器、团队画布、成员卡、Team CLI 面板、
      会话启动模式、插件能力树先过视觉验收

## P1 模型插件（model-plugin）

- [ ] `provider_types` / provider 实例 / 模型目录 / 参数 schema / 健康检查（沿用原设计）
- [ ] CLI：`keel model provider list|show|add|update|remove|test`、`keel model catalog`
- [ ] CLI 与 UI 共用 schema/租户边界；凭据只接受引用
- [ ] Provider 删除保护：被 Agent/Team 引用时拒绝并列出引用方
- [ ] 能力树声明每个 model CLI 子命令

## P2 Agent 插件（agent-plugin）

- [ ] Agent 表与 API：`/v1/agents`；租户隔离、归档、引用保护
- [ ] 默认模型绑定：只是默认，可被会话/任务覆盖
- [ ] 工具能力：从工具注册源选择；`agent_tools` 保存 toolId+开关+配置
- [ ] 系统提示词：inline / AGENTS.md 文件引用；进入 run system sections
- [ ] 头像：确定性 identicon（seed）、上传/URL 覆盖、失败回退
- [ ] 名字/描述/标签可编辑；默认值只是建议
- [ ] CLI：`keel agent list|show|create|update|delete|start|stop`
- [ ] 能力树声明 agent CLI；删除/归档引用保护
- [ ] Agent 库 UI 组件与编辑器

## P3 AgentTeam 插件（agent-team-plugin）

- [ ] Team 表/配置文件：`agent_teams`、`agent_team_members`；captain 必选唯一
- [ ] Team 配置是 DB-backed 文件；revision/hash；session 启动冻结快照
- [ ] 成员 0..N；成员是 Agent 引用/inline 定义；成员 label/description 可编辑
- [ ] Team CLI：`keel team list|show|create|update|delete|member add|remove|save|start`
- [ ] Team CLI 能力开关：关闭后从 captain 工具/CLI 面移除；返回 typed permission error
- [ ] 队长自组装：成员增删只改 team 配置，不污染 model/agent provider
- [ ] Team 删除/归档引用保护；运行中修改不影响当前 run
- [ ] 能力树声明 team CLI 子命令与 callable_by
- [ ] Team 画布/成员卡/Team CLI 面板 UI

## P4 会话启动与内核注入

- [ ] 会话 `mode` = standard|team；standard 保存 model 绑定，team 保存 teamId
- [ ] Standard：选模型/默认 Agent 启动；不要求团队
- [ ] Team：选有效团队启动；captain 为主 Agent；缺 captain 阻止启动
- [ ] run manifest 冻结 agent/team/tool/prompt hash
- [ ] 运行中切换模式/团队只影响下一次 run
- [ ] 成员复用既有 Agent/run 机制；不新增运行拓扑
- [ ] 会话 header：Standard 显示模型；Team 显示团队名+captain+成员入口

## P5 设置与 UI

- [ ] 完整设置页：模型 / Agents / AgentTeam / 工具 / 能力 / 客户端 / 插件 / 高级
- [ ] Agent 卡片、Agent 编辑器、头像生成、工具注册源选择器、AGENTS.md 编辑器
- [ ] 团队卡片、团队画布、captain 槽、成员库/新建 Agent、Team CLI 能力面板
- [ ] 会话启动模式选择（Standard/Team）、团队选择器、缺 captain 阻塞态
- [ ] 插件能力树 UI（命令/子命令/描述/权限/danger/callable_by/复制）
- [ ] 五态（空/加载/错误/权限不足/成功）、390px 单列、键盘可达
- [ ] shadcn 官方组件 + solid-glass；禁止手写仿制
- [ ] 组件画廊先行；组合页面后验收

## P6 Provider 适配器

- [ ] LLM 适配器与现有模型配置迁移
- [ ] 图像/视频适配器：Hermes `image_generate`/`video_generate` 形态；产物入 layerfs
- [ ] 语音适配器：TTS 文本→音频；STT 音频→transcript/时间戳；音色/语言参数
- [ ] 决策适配器：System One 0.2.0；TypeSafe/Cloudflare Clef/OpenRouter/OpenJev；
      noul/choice/score（box 仅 TypeSafe）、能力预检、shadow/enforce、fail-closed 默认
- [ ] Provider 崩溃隔离；usage 计量不写价格

## P7 验收

- [ ] 创建 Agent：名字/描述/头像/默认模型/工具/AGENTS.md；identicon 同 seed 稳定
- [ ] 创建团队：只配 captain 可启动；captain 必选唯一；缺 captain 阻止启动
- [ ] 队长 CLI 开启时可 `keel team member add`；关闭后命令消失并返回权限错误
- [ ] 队长增删成员只改 team 配置；model/agent provider 配置无变化
- [ ] Standard 选模型启动；Team 选团队启动；运行中切换只影响下一次 run
- [ ] 工具列表来自注册源；插件卸载后 UI/CLI/Agent 工具面同步
- [ ] 旧模型预设已删除，旧 preset 不能启动新 run；旧会话历史仍可读
- [ ] 凭据零泄漏（API/日志/事件/CLI 输出/UI DOM/team 配置）
- [ ] 组件画廊与组合页面视觉复核通过；390px 无溢出
