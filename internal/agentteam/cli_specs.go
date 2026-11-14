// This file declares the concrete CLI capability trees of the three plugins the
// harness ships with: the model plugin, the agent plugin and the agent-team
// plugin.
//
// The declaration lives in this package because the providers, agents and teams
// it manages are defined here, and because every consumer — --help, the
// settings page, creation mode, the agent tool face and the captain's team CLI —
// must read one and the same tree. The values are ordinary
// pluginpkg.CLICapability nodes: pluginpkg's manifest validation is the only
// validator, so a node the host could not serve cannot survive Validate.
//
// Two things are deliberate:
//
//   - Every spec carries a Required command set written out separately from its
//     capabilities. Dropping a sub-command therefore fails validation ("required
//     command is not declared") instead of shrinking the tree to whatever was
//     left and validating against itself.
//   - A capability is never derived from the manifest at render time. The path,
//     the summary, the schemas, the permissions, the danger class, the
//     idempotency and the callers are declared together, so --help, the settings
//     page and the agent tool face cannot disagree about a command.
//
// Specs:
//   - openspec/changes/keel-agent-team-v1/specs/plugin-capabilities/spec.md
//   - openspec/changes/keel-agent-team-v1/specs/provider-registry/spec.md
//   - openspec/changes/keel-agent-team-v1/specs/agent-registry/spec.md
//   - openspec/changes/keel-agent-team-v1/specs/agent-team/spec.md
package agentteam

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/jixoai/keel/internal/modelprovider"
	"github.com/jixoai/keel/internal/pluginpkg"
)

// PluginCLISpec is one plugin's concrete CLI capability tree, the manifest
// identity that carries it, and the command set the plugin's spec freezes.
type PluginCLISpec struct {
	// PluginID is the manifest id of the plugin the tree belongs to.
	PluginID string
	// Version is the plugin manifest version.
	Version string
	// Description is the one-line plugin description shown in listings.
	Description string
	// Capabilities are the declared commands, in declaration order. Use CLI for
	// a copy that a caller may keep.
	Capabilities []pluginpkg.CLICapability
	// Required is the command set the plugin's spec freezes: every path listed
	// here must be declared by Capabilities. It is stored separately on purpose
	// — see the file comment — and a spec without it is a validation error, so
	// coverage can never be claimed vacuously.
	Required [][]string
}

// ModelPluginCLISpec returns the model plugin's capability tree
// (`keel model ...`): the six provider management commands and the catalog
// command frozen by specs/provider-registry.
func ModelPluginCLISpec() PluginCLISpec {
	commands := []cliCommand{
		{
			path:    []string{"model", "provider", "list"},
			summary: "列出模型 Provider",
			description: "按类型与状态列出当前租户的 Provider 摘要，是设置页列表与 Agent 发现可用模型来源的同一数据面。" +
				"输出只含 credentialRef 引用，不含凭据明文；本命令不发起上游请求，健康状态要显式运行 keel model provider test。",
			args: objectSchema(map[string]any{
				"type":   stringProperty("按 Provider 类型过滤；类型由插件声明，首批为 " + strings.Join(modelProviderTypes, "/") + "，插件可新增"),
				"status": enumProperty(providerStatuses, "按启用状态过滤"),
			}),
			output:      arrayOf(schemaRef("model_provider_summary")),
			permissions: []string{"model.read"},
			danger:      pluginpkg.DangerRead,
			idempotent:  true,
			callableBy:  callerAll,
			examples:    []string{"keel model provider list --type image"},
		},
		{
			path:    []string{"model", "provider", "show"},
			summary: "查看单个模型 Provider",
			description: "输出单个 Provider 的类型、endpoint、credentialRef 引用、启用状态与模型清单，并列出引用它的 Agent/Team。" +
				"不输出凭据明文；引用信息与 remove 的删除保护同源。",
			args:        objectSchema(map[string]any{"providerId": stringProperty("Provider id")}, "providerId"),
			output:      schemaRef("model_provider_detail"),
			permissions: []string{"model.read"},
			danger:      pluginpkg.DangerRead,
			idempotent:  true,
			callableBy:  callerAll,
			examples:    []string{"keel model provider show prov_ds"},
		},
		{
			path:    []string{"model", "provider", "add"},
			summary: "新增模型 Provider",
			description: "用凭据引用创建 Provider：接受类型、名称、credentialRef 与 endpoint。类型由插件声明，" +
				"CLI 不维护封闭枚举（插件新增 embedding 等类型无需改 CLI）；credentialRef 可留空，此时 Provider " +
				"进入 needs_auth 而不是拒绝创建。凭据只接受引用，CLI 不接受也不回显明文；与 UI 使用同一 schema 与租户校验。" +
				"类型创建后不可变：换类型等于新建 Provider。",
			args: objectSchema(map[string]any{
				"type":          stringProperty("Provider 类型（插件声明，如 llm/image/tts/stt；插件可新增）"),
				"name":          stringProperty("展示名"),
				"credentialRef": stringProperty("凭据引用（如 vault://img）；留空表示稍后补，状态为 needs_auth"),
				"endpoint":      stringProperty("上游 base URL；留空使用插件默认"),
			}, "type", "name"),
			output:      schemaRef("model_provider_summary"),
			permissions: []string{"model.write"},
			danger:      pluginpkg.DangerWrite,
			idempotent:  false,
			callableBy:  callerUIAgent,
			examples:    []string{"keel model provider add --type image --name img --credential-ref vault://img"},
		},
		{
			path:    []string{"model", "provider", "update"},
			summary: "更新模型 Provider",
			description: "按 id 更新可编辑字段（名称、endpoint、credentialRef、启用状态）；providerId 与 type 不可变。" +
				"同一组值重复提交结果相同，所以重试安全；凭据仍然只接受引用。",
			args: objectSchema(map[string]any{
				"providerId":    stringProperty("Provider id"),
				"name":          stringProperty("展示名"),
				"credentialRef": stringProperty("凭据引用；留空表示不改"),
				"endpoint":      stringProperty("上游 base URL；留空表示不改"),
				"status":        enumProperty(providerStatuses, "启用状态"),
			}, "providerId"),
			output:      schemaRef("model_provider_summary"),
			permissions: []string{"model.write"},
			danger:      pluginpkg.DangerWrite,
			idempotent:  true,
			callableBy:  callerUIAgent,
			examples:    []string{"keel model provider update prov_ds --status disabled"},
		},
		{
			path:    []string{"model", "provider", "remove"},
			summary: "删除模型 Provider（被引用时拒绝）",
			description: "删除 Provider 实例。被任何 Agent 或 Team 引用时拒绝执行并列出引用方，必须先解除引用或改为禁用；" +
				"不做级联改写，也不在运行时静默回退到其它厂商。属破坏性操作，只允许人工面调用。",
			args:        objectSchema(map[string]any{"providerId": stringProperty("Provider id")}, "providerId"),
			output:      objectSchema(map[string]any{"removed": stringProperty("被删除的 Provider id")}, "removed"),
			permissions: []string{"model.write"},
			danger:      pluginpkg.DangerDestructive,
			idempotent:  false,
			callableBy:  callerUIOnly,
			examples:    []string{"keel model provider remove prov_legacy"},
		},
		{
			path:    []string{"model", "provider", "test"},
			summary: "健康检查模型 Provider",
			description: "用 Provider 的凭据引用发起一次轻量真实请求，验证凭据、endpoint 与模型可用性。" +
				"不写入 Provider 配置，但会产生一次真实上游调用（可能计费），因此按写风险声明并沿用确认语义；重试安全。",
			args: objectSchema(map[string]any{
				"providerId": stringProperty("Provider id"),
				"modelId":    stringProperty("可选：只测试指定模型"),
			}, "providerId"),
			output: objectSchema(map[string]any{
				"ok":        booleanProperty("是否通过"),
				"latencyMs": integerProperty("上游往返耗时（毫秒）"),
				"error":     stringProperty("失败原因；成功时为空"),
			}, "ok"),
			permissions: []string{"model.read"},
			danger:      pluginpkg.DangerWrite,
			idempotent:  true,
			callableBy:  callerUIAgent,
			examples:    []string{"keel model provider test prov_ds"},
		},
		{
			path:    []string{"model", "catalog"},
			summary: "读取 Provider 的模型目录",
			description: "读取指定 Provider 的模型目录（模型 id、能力、上下文与参数 schema），供 Agent 默认模型选择与选项校验使用。" +
				"--refresh 触发一次上游重新拉取并刷新缓存，不修改 Provider 配置。",
			args: objectSchema(map[string]any{
				"providerId": stringProperty("Provider id"),
				"refresh":    booleanProperty("向上游重新拉取并刷新缓存"),
			}, "providerId"),
			output:      arrayOf(schemaRef("model_catalog_entry")),
			permissions: []string{"model.read"},
			danger:      pluginpkg.DangerRead,
			idempotent:  true,
			callableBy:  callerAll,
			examples:    []string{"keel model catalog prov_ds --refresh"},
		},
	}
	return PluginCLISpec{
		PluginID:     "keel.model",
		Version:      "1.0.0",
		Description:  "模型 Provider 插件：Provider 实例、模型目录与健康检查的 CLI 管理面。",
		Capabilities: capabilities(commands),
		Required:     modelRequiredPaths(),
	}
}

// AgentPluginCLISpec returns the agent plugin's capability tree
// (`keel agent ...`): list/show/create/update/delete/start/stop.
func AgentPluginCLISpec() PluginCLISpec {
	commands := []cliCommand{
		{
			path:    []string{"agent", "list"},
			summary: "列出 Agent",
			description: "列出当前租户的 Agent（id、名称、描述、默认模型引用、工具数量、是否归档），" +
				"是设置页 Agent 库与团队成员选择器的同一数据面。不输出凭据明文。",
			args: objectSchema(map[string]any{
				"query":           stringProperty("按名称或描述搜索"),
				"includeArchived": booleanProperty("是否包含已归档 Agent"),
			}),
			output:      arrayOf(schemaRef("agent_summary")),
			permissions: []string{"agent.read"},
			danger:      pluginpkg.DangerRead,
			idempotent:  true,
			callableBy:  callerAll,
			examples:    []string{"keel agent list --query 设计"},
		},
		{
			path:    []string{"agent", "show"},
			summary: "查看 Agent 配置",
			description: "输出 Agent 的名称、描述、头像来源、默认模型引用、工具清单与来源、系统提示词来源（inline 或 AGENTS.md 文件）" +
				"与团队 CLI 能力开关。默认模型只是默认，可被会话/任务显式覆盖；不输出明文凭据。",
			args:        objectSchema(map[string]any{"agentId": stringProperty("Agent id")}, "agentId"),
			output:      schemaRef("agent_detail"),
			permissions: []string{"agent.read"},
			danger:      pluginpkg.DangerRead,
			idempotent:  true,
			callableBy:  callerAll,
			examples:    []string{"keel agent show agent_design_lead"},
		},
		{
			path:    []string{"agent", "create"},
			summary: "创建 Agent",
			description: "创建 Agent：名字、描述、头像、默认模型、工具与系统提示词（inline 或 AGENTS.md 文件引用）；" +
				"id 与确定性 identicon 由系统生成。与 UI 使用同一校验与租户边界，配置里不允许内联明文凭据。",
			args: objectSchema(map[string]any{
				"name":         stringProperty("展示名"),
				"description":  stringProperty("一句话描述"),
				"avatar":       schemaRef("avatar"),
				"defaultModel": schemaRef("model_binding"),
				"tools":        arrayOf(schemaRef("tool_grant")),
				"systemPrompt": schemaRef("system_prompt"),
			}, "name"),
			output:      schemaRef("agent_detail"),
			permissions: []string{"agent.write"},
			danger:      pluginpkg.DangerWrite,
			idempotent:  false,
			callableBy:  callerUIAgent,
			examples:    []string{`keel agent create --name "设计主理人" --default-model prov_ds/deepseek-v4.1`},
		},
		{
			path:    []string{"agent", "update"},
			summary: "更新 Agent 配置",
			description: "按 id 更新 Agent 配置（名字、描述、头像、默认模型、工具、系统提示词、团队 CLI 开关）；" +
				"同一组值重复提交结果相同。写操作与 UI 使用同一 schema 校验；凭据仍只显示引用。",
			args: objectSchema(map[string]any{
				"agentId":      stringProperty("Agent id"),
				"name":         stringProperty("展示名"),
				"description":  stringProperty("一句话描述"),
				"avatar":       schemaRef("avatar"),
				"defaultModel": schemaRef("model_binding"),
				"tools":        arrayOf(schemaRef("tool_grant")),
				"systemPrompt": schemaRef("system_prompt"),
				"teamCli":      schemaRef("team_cli"),
			}, "agentId"),
			output:      schemaRef("agent_detail"),
			permissions: []string{"agent.write"},
			danger:      pluginpkg.DangerWrite,
			idempotent:  true,
			callableBy:  callerUIAgent,
			examples:    []string{"keel agent update agent_design_lead --name 设计主理人"},
		},
		{
			path:    []string{"agent", "delete"},
			summary: "删除 Agent（被引用时拒绝）",
			description: "删除 Agent。被团队或会话引用时拒绝并列出引用方：必须先移出团队再删除，或改为归档；" +
				"归档保留历史快照，不影响已启动的 run。属破坏性操作，只允许人工面调用。",
			args:        objectSchema(map[string]any{"agentId": stringProperty("Agent id")}, "agentId"),
			output:      objectSchema(map[string]any{"removed": stringProperty("被删除的 Agent id")}, "removed"),
			permissions: []string{"agent.write"},
			danger:      pluginpkg.DangerDestructive,
			idempotent:  false,
			callableBy:  callerUIOnly,
			examples:    []string{"keel agent delete agent_retired"},
		},
		{
			path:    []string{"agent", "start"},
			summary: "启动 Agent",
			description: "用 Agent 的配置启动一个 session/run，并把解析后的模型绑定冻结进 run manifest 快照。" +
				"启动是状态转移：重复调用不是幂等空操作，已在运行时返回“已运行”。",
			args: objectSchema(map[string]any{
				"agentId": stringProperty("Agent id"),
				"input":   stringProperty("首轮输入；留空则等待后续消息"),
			}, "agentId"),
			output:      schemaRef("agent_run_ref"),
			permissions: []string{"agent.write"},
			danger:      pluginpkg.DangerWrite,
			idempotent:  false,
			callableBy:  callerAll,
			examples:    []string{"keel agent start agent_design_lead"},
		},
		{
			path:    []string{"agent", "stop"},
			summary: "停止 Agent",
			description: "停止该 Agent 当前运行的 session/run；不重写已冻结的历史快照，下一次启动使用最新配置。" +
				"停止是状态转移：重复调用返回“未在运行”而不是静默成功。",
			args:        objectSchema(map[string]any{"agentId": stringProperty("Agent id")}, "agentId"),
			output:      objectSchema(map[string]any{"stopped": booleanProperty("是否有正在运行的 run 被停止")}, "stopped"),
			permissions: []string{"agent.write"},
			danger:      pluginpkg.DangerWrite,
			idempotent:  false,
			callableBy:  callerAll,
			examples:    []string{"keel agent stop agent_design_lead"},
		},
	}
	return PluginCLISpec{
		PluginID:     "keel.agent",
		Version:      "1.0.0",
		Description:  "Agent 插件：Agent 库、配置编辑与启停的 CLI 管理面。",
		Capabilities: capabilities(commands),
		Required:     agentRequiredPaths(),
	}
}

// TeamPluginCLISpec returns the agent-team plugin's capability tree
// (`keel team ...`): list/show/create/update/delete/member add/member
// remove/save/start.
func TeamPluginCLISpec() PluginCLISpec {
	commands := []cliCommand{
		{
			path:    []string{"team", "list"},
			summary: "列出团队",
			description: "列出当前租户的团队（id、名称、captain、成员数、状态），是设置页团队卡片与会话启动页团队选择器的同一数据面。" +
				"状态由配置派生（缺 captain 或已归档即 invalid），不读取陈旧行。",
			args:        objectSchema(map[string]any{"includeArchived": booleanProperty("是否包含已归档团队")}),
			output:      arrayOf(schemaRef("team_summary")),
			permissions: []string{"team.read"},
			danger:      pluginpkg.DangerRead,
			idempotent:  true,
			callableBy:  callerAll,
			examples:    []string{"keel team list"},
		},
		{
			path:    []string{"team", "show"},
			summary: "查看团队配置",
			description: "输出团队配置：captain、成员、团队级默认、能力开关（含 teamCli 允许的子命令）与 revision/hash。" +
				"运行中的 run 用的是启动时快照，本命令读到的是最新配置；不含凭据明文。",
			args:        objectSchema(map[string]any{"teamId": stringProperty("团队 id")}, "teamId"),
			output:      schemaRef("team_detail"),
			permissions: []string{"team.read"},
			danger:      pluginpkg.DangerRead,
			idempotent:  true,
			callableBy:  callerAll,
			examples:    []string{"keel team show team_growth"},
		},
		{
			path:    []string{"team", "create"},
			summary: "创建团队",
			description: "创建团队配置：captain 必选且唯一，成员 0..N；团队是配置空间，不定义第二套运行拓扑。" +
				"只有人工面可以创建团队，队长 CLI 不含此命令。",
			args: objectSchema(map[string]any{
				"name":           stringProperty("团队名"),
				"captainAgentId": stringProperty("captain 的 Agent id"),
				"memberAgentIds": arrayOf(stringProperty("成员 Agent id")),
			}, "name", "captainAgentId"),
			output:      schemaRef("team_detail"),
			permissions: []string{"team.write"},
			danger:      pluginpkg.DangerWrite,
			idempotent:  false,
			callableBy:  callerUIOnly,
			examples:    []string{"keel team create --name 增长组 --captain agent_growth_captain"},
		},
		{
			path:    []string{"team", "update"},
			summary: "更新团队配置",
			description: "按 id 更新团队名称、描述、团队级默认与能力开关；captain 的更换同样走这里（换人后仍必须唯一）。" +
				"只改 team 配置，不改写 model/agent provider 共享配置；同一组值重复提交结果相同。",
			args: objectSchema(map[string]any{
				"teamId":         stringProperty("团队 id"),
				"name":           stringProperty("团队名"),
				"description":    stringProperty("团队描述"),
				"captainAgentId": stringProperty("新的 captain Agent id"),
				"defaults":       schemaRef("team_defaults"),
				"capabilities":   schemaRef("team_capabilities"),
			}, "teamId"),
			output:      schemaRef("team_detail"),
			permissions: []string{"team.write"},
			danger:      pluginpkg.DangerWrite,
			idempotent:  true,
			callableBy:  callerUIOnly,
			examples:    []string{"keel team update team_growth --name 增长组v2"},
		},
		{
			path:    []string{"team", "delete"},
			summary: "删除团队（被会话引用时拒绝）",
			description: "删除团队配置。被会话或运行引用时拒绝并列出引用方，可以改为归档；归档团队视为 invalid，不得启动。" +
				"属破坏性操作，只允许人工面调用。",
			args:        objectSchema(map[string]any{"teamId": stringProperty("团队 id")}, "teamId"),
			output:      objectSchema(map[string]any{"removed": stringProperty("被删除的团队 id")}, "removed"),
			permissions: []string{"team.write"},
			danger:      pluginpkg.DangerDestructive,
			idempotent:  false,
			callableBy:  callerUIOnly,
			examples:    []string{"keel team delete team_retired"},
		},
		{
			path:    []string{"team", "member", "add"},
			summary: "向团队添加成员",
			description: "把 Agent（引用 Agent 库或 inline 定义）加入团队配置并纳入当前团队实例；默认不持久化，除非显式 team save。" +
				"只修改 team 配置，不改写 model/agent provider 的共享配置。需要 teamCli.enabled 且允许 member.add，关闭后本命令从队长工具/CLI 面消失。",
			args: objectSchema(map[string]any{
				"teamId":      stringProperty("团队 id"),
				"agentId":     stringProperty("成员 Agent id"),
				"label":       stringProperty("团队内显示名"),
				"description": stringProperty("团队内角色说明"),
			}, "teamId", "agentId"),
			output:      schemaRef("team_member"),
			permissions: []string{"team.write"},
			danger:      pluginpkg.DangerWrite,
			idempotent:  false,
			callableBy:  callerUICaptain,
			examples:    []string{"keel team member add team_growth agent_design_lead"},
		},
		{
			path:    []string{"team", "member", "remove"},
			summary: "从团队移除成员",
			description: "从团队配置移除一个成员席位；captain 不能被移除，只能换 captain。只修改 team 配置，不改写 Agent 库条目；" +
				"运行中的 run 保留启动时快照。需要 teamCli.enabled 且允许 member.remove。",
			args: objectSchema(map[string]any{
				"teamId":  stringProperty("团队 id"),
				"agentId": stringProperty("成员 Agent id"),
			}, "teamId", "agentId"),
			output:      objectSchema(map[string]any{"removed": stringProperty("被移除的成员 Agent id")}, "removed"),
			permissions: []string{"team.write"},
			danger:      pluginpkg.DangerDestructive,
			idempotent:  false,
			callableBy:  callerUICaptain,
			examples:    []string{"keel team member remove team_growth agent_design_lead"},
		},
		{
			path:    []string{"team", "member", "list"},
			summary: "列出团队成员",
			description: "输出团队配置里的成员席位（agentId、团队内 label/description、是否 inline 定义、是否为 captain），" +
				"是团队画布、成员抽屉与队长自组装前的现状读取面。只读，不改写任何配置；不含凭据明文。",
			args:        objectSchema(map[string]any{"teamId": stringProperty("团队 id")}, "teamId"),
			output:      arrayOf(schemaRef("team_member")),
			permissions: []string{"team.read"},
			danger:      pluginpkg.DangerRead,
			idempotent:  true,
			callableBy:  callerAll,
			examples:    []string{"keel team member list team_growth"},
		},
		{
			path:    []string{"team", "member", "save"},
			summary: "保存团队成员变更",
			description: "把队长的成员增删与成员 label/description 修改持久化为 team 配置文件，并前移 revision/hash；" +
				"只改成员席位，不改 captain/团队名/默认/能力（整份保存用 keel team save），也不改写 model/agent provider 共享配置。" +
				"同一份内容重复保存结果相同。需要 teamCli.enabled 且允许 member.save。",
			args: objectSchema(map[string]any{
				"teamId":  stringProperty("团队 id"),
				"message": stringProperty("变更说明，写入审计事实"),
			}, "teamId"),
			output: objectSchema(map[string]any{
				"revision": integerProperty("保存后的配置 revision"),
				"hash":     stringProperty("内容 hash"),
			}, "revision", "hash"),
			permissions: []string{"team.write"},
			danger:      pluginpkg.DangerWrite,
			idempotent:  true,
			callableBy:  callerUICaptain,
			examples:    []string{"keel team member save team_growth --message 新增设计主理人"},
		},
		{
			path:    []string{"team", "save"},
			summary: "保存团队配置",
			description: "把队长自组装产生的成员变更持久化为 team 配置文件，并前移 revision/hash；同一份内容重复保存结果相同。" +
				"只写 team 配置，不触碰 model/agent provider 共享配置。需要 teamCli.enabled 且允许 save。",
			args: objectSchema(map[string]any{
				"teamId":  stringProperty("团队 id"),
				"message": stringProperty("变更说明，写入审计事实"),
			}, "teamId"),
			output: objectSchema(map[string]any{
				"revision": integerProperty("保存后的配置 revision"),
				"hash":     stringProperty("内容 hash"),
			}, "revision", "hash"),
			permissions: []string{"team.write"},
			danger:      pluginpkg.DangerWrite,
			idempotent:  true,
			callableBy:  callerUICaptain,
			examples:    []string{"keel team save team_growth --message 新增设计主理人"},
		},
		{
			path:    []string{"team", "start"},
			summary: "启动团队",
			description: "以 captain 为主 Agent 启动团队 run：captain 必选且有效，缺 captain 或已归档时拒绝启动；" +
				"成员按既有 Agent/run 机制各自启动，不共享 captain 的模型上下文。启动时冻结团队快照，运行中修改只影响下一次 run。",
			args: objectSchema(map[string]any{
				"teamId": stringProperty("团队 id"),
				"input":  stringProperty("首轮输入；留空则等待后续消息"),
			}, "teamId"),
			output:      schemaRef("team_run_ref"),
			permissions: []string{"team.write"},
			danger:      pluginpkg.DangerWrite,
			idempotent:  false,
			callableBy:  callerUICaptain,
			examples:    []string{"keel team start team_growth"},
		},
	}
	return PluginCLISpec{
		PluginID:     "keel.agent-team",
		Version:      "1.0.0",
		Description:  "AgentTeam 插件：团队配置、成员自组装与团队启动的 CLI 管理面。",
		Capabilities: capabilities(commands),
		Required:     teamRequiredPaths(),
	}
}

// PluginCLISpecs returns the three plugin specs in declaration order (model,
// agent, team). The order is presentation order for the settings page sections;
// command order inside a tree is the declaration order above.
func PluginCLISpecs() []PluginCLISpec {
	return []PluginCLISpec{ModelPluginCLISpec(), AgentPluginCLISpec(), TeamPluginCLISpec()}
}

// CLICapabilities returns every capability of the three trees in the canonical
// command order that --help and the settings page render (sorted by path, the
// pluginpkg order). The result is a copy: editing it does not touch any spec.
func CLICapabilities() []pluginpkg.CLICapability {
	specs := PluginCLISpecs()
	combined := make([]pluginpkg.CLICapability, 0, 32)
	for _, spec := range specs {
		combined = append(combined, spec.CLI()...)
	}
	// Sorting through pluginpkg keeps one ordering rule in the system: the same
	// lessPath that CLICapabilities() uses on a manifest.
	manifest := &pluginpkg.Manifest{Provides: pluginpkg.Provides{CLI: combined}}
	return manifest.CLICapabilities()
}

// TeamSubcommand returns the TeamCLI allowlist name of a declared team command:
// [team member add] → "member.add", [team list] → "list". It is the one mapping
// between the capability tree and the allowlist vocabulary, so a tree command
// and an allowlist entry cannot name different things.
func TeamSubcommand(path []string) (string, bool) {
	if len(path) < 2 || path[0] != "team" {
		return "", false
	}
	return strings.Join(path[1:], "."), true
}

// capabilityByPath looks a capability up by exact path in a declared tree.
func capabilityByPath(capabilities []pluginpkg.CLICapability, path []string) (pluginpkg.CLICapability, bool) {
	for _, capability := range capabilities {
		if slices.Equal(capability.Path, path) {
			return capability, true
		}
	}
	return pluginpkg.CLICapability{}, false
}

// CaptainTeamCapabilities returns the team commands a captain may actually run:
// the tree's team_captain callable set filtered by the EFFECTIVE team CLI switch
// (EffectiveTeamCLI of the team and the captain agent). A command absent from
// the result does not exist for the captain — this is the "关闭后命令从工具/CLI
// 面消失" half of the spec, computed from one function instead of a prompt.
func CaptainTeamCapabilities(spec PluginCLISpec, team, agent TeamCLI) []pluginpkg.CLICapability {
	effective := EffectiveTeamCLI(team, agent)
	capabilities := make([]pluginpkg.CLICapability, 0, len(spec.Capabilities))
	for _, capability := range spec.CLI() {
		if !capability.CallableByCaller(pluginpkg.CallerTeamCaptain) {
			continue
		}
		subcommand, ok := TeamSubcommand(capability.Path)
		if !ok || !effective.Allows(subcommand) {
			continue
		}
		capabilities = append(capabilities, capability)
	}
	return capabilities
}

// AuthorizeCaptainCommand is the dispatch-side check for a captain call: it
// reports a typed *PermissionError when the command is not declared for the
// captain, the switch is off, or the subcommand is not on the effective
// allowlist. It and CaptainTeamCapabilities share EffectiveTeamCLI, so a command
// that is missing from the face is exactly a command that fails here.
func AuthorizeCaptainCommand(spec PluginCLISpec, team, agent TeamCLI, path ...string) error {
	subcommand, ok := TeamSubcommand(path)
	if !ok {
		return &PermissionError{Subcommand: strings.Join(path, " "), Reason: PermissionNotDeclared}
	}
	capability, declared := capabilityByPath(spec.Capabilities, path)
	if !declared || !capability.CallableByCaller(pluginpkg.CallerTeamCaptain) {
		return &PermissionError{Subcommand: subcommand, Reason: PermissionNotDeclared}
	}
	effective := EffectiveTeamCLI(team, agent)
	switch {
	case !effective.Enabled:
		return &PermissionError{Subcommand: subcommand, Reason: PermissionDisabled}
	case !effective.Allows(subcommand):
		return &PermissionError{Subcommand: subcommand, Reason: PermissionNotAllowed}
	}
	return nil
}

// CapabilityTree is one plugin's served capability tree: its manifest identity
// plus every declared command. It is what the settings page renders and what a
// model/captain discovers.
type CapabilityTree struct {
	// PluginID is the manifest id that owns the tree.
	PluginID string `json:"pluginId"`
	// Version is the plugin manifest version.
	Version string `json:"version"`
	// Description is the one-line plugin description.
	Description string `json:"description"`
	// Commands are the declared capabilities in declaration order.
	Commands []pluginpkg.CLICapability `json:"commands"`
}

// CapabilityCatalog is the machine-readable capability document that
// `keel plugin capabilities --json` and GET /v1/plugins/capabilities serve. It
// is built from the same specs the CLI trees and the manifest validation use,
// so no surface can serve a command the others do not declare.
type CapabilityCatalog struct {
	// Plugins are the three plugin trees in declaration order (model, agent,
	// team).
	Plugins []CapabilityTree `json:"plugins"`
}

// ServeCapabilityCatalog builds the catalog from the three plugin specs. The
// result is a fresh value: the trees are copies.
func ServeCapabilityCatalog() CapabilityCatalog {
	specs := PluginCLISpecs()
	plugins := make([]CapabilityTree, 0, len(specs))
	for _, spec := range specs {
		plugins = append(plugins, CapabilityTree{
			PluginID:    spec.PluginID,
			Version:     spec.Version,
			Description: spec.Description,
			Commands:    spec.CLI(),
		})
	}
	return CapabilityCatalog{Plugins: plugins}
}

// JSON renders the catalog as the served document. The output is stable
// (indented, declaration order), so a client can diff it and an audit can hash
// it.
func (c CapabilityCatalog) JSON() ([]byte, error) {
	return json.MarshalIndent(c, "", "  ")
}

// Validate checks the catalog the way an install checks a plugin: every tree
// must pass pluginpkg's manifest validation and freeze the command set its spec
// requires. An unknown plugin id has no required set and therefore fails
// closed.
func (c CapabilityCatalog) Validate() error {
	specs := make([]PluginCLISpec, 0, len(c.Plugins))
	for _, plugin := range c.Plugins {
		specs = append(specs, PluginCLISpec{
			PluginID:     plugin.PluginID,
			Version:      plugin.Version,
			Description:  plugin.Description,
			Capabilities: plugin.Commands,
			Required:     requiredPathsForPlugin(plugin.PluginID),
		})
	}
	return ValidatePluginCLISpecs(specs)
}

// requiredPathsForPlugin returns the frozen command set of a known plugin id.
// An unknown id gets nil, which PluginCLISpec.Validate rejects ("coverage cannot
// be checked"), so a catalog cannot smuggle in a plugin nobody declared.
func requiredPathsForPlugin(pluginID string) [][]string {
	switch pluginID {
	case "keel.model":
		return modelRequiredPaths()
	case "keel.agent":
		return agentRequiredPaths()
	case "keel.agent-team":
		return teamRequiredPaths()
	}
	return nil
}

// CLI returns a copy of the declared capabilities in declaration order.
func (s PluginCLISpec) CLI() []pluginpkg.CLICapability {
	return capabilitiesCopy(s.Capabilities)
}

// Manifest returns the plugin manifest that carries this tree. It is a fresh
// value on every call: the manifest copies the capabilities, so a caller that
// edits the returned manifest cannot edit the spec.
func (s PluginCLISpec) Manifest() *pluginpkg.Manifest {
	return &pluginpkg.Manifest{
		ID:          s.PluginID,
		Version:     s.Version,
		Kind:        pluginpkg.KindInProcess,
		Description: s.Description,
		Engines:     pluginpkg.Engines{Keel: ">=0.1.0 <1.0.0"},
		Provides:    pluginpkg.Provides{CLI: s.CLI()},
		Source:      pluginpkg.Source{Type: pluginpkg.SourceBuiltin},
	}
}

// Validate checks one tree the way the host checks a plugin package:
//
//   - the manifest carrying it must pass pluginpkg's manifest validation, which
//     is where summary/description/danger/permissions/callable_by and path
//     uniqueness are enforced (there is no second validator);
//   - every path in Required must be declared, so a dropped sub-command is an
//     error instead of a smaller tree;
//   - an empty Required set is an error: a tree that claims no frozen command
//     set cannot be checked for coverage at all.
//
// Failures come back as *pluginpkg.ValidationError with field-level entries, so
// the API can answer 400 with the exact field and the settings page can
// highlight it.
func (s PluginCLISpec) Validate() error {
	problems := append([]pluginpkg.FieldError(nil), pluginpkg.FieldErrors(s.Manifest().Validate(pluginpkg.Environment{}))...)
	if len(s.Required) == 0 {
		problems = append(problems, pluginpkg.FieldError{
			Field:   "provides.cli",
			Message: fmt.Sprintf("plugin %s declares no required command set; command coverage cannot be checked", s.PluginID),
		})
	}
	declared := make(map[string]bool, len(s.Capabilities))
	for _, capability := range s.Capabilities {
		declared[capability.Key()] = true
	}
	for _, path := range s.Required {
		if declared[strings.Join(path, " ")] {
			continue
		}
		problems = append(problems, pluginpkg.FieldError{
			Field:   "provides.cli",
			Message: fmt.Sprintf("required command %q is not declared", commandHead(path)),
		})
	}
	if len(problems) == 0 {
		return nil
	}
	return &pluginpkg.ValidationError{Errors: problems}
}

// ValidatePluginCLISpecs checks every spec and the cross-plugin rule that one
// command path belongs to exactly one plugin: the three trees share one `keel`
// namespace, so a path declared twice would give one command two owners (two
// different danger classes, two permission sets). Intra-plugin repeats are
// reported by pluginpkg through spec.Validate; this function adds the
// inter-plugin check pluginpkg cannot see.
func ValidatePluginCLISpecs(specs []PluginCLISpec) error {
	problems := make([]pluginpkg.FieldError, 0)
	owner := make(map[string]string, 32)
	for _, spec := range specs {
		problems = append(problems, pluginpkg.FieldErrors(spec.Validate())...)
		for _, capability := range spec.Capabilities {
			key := capability.Key()
			previous, exists := owner[key]
			switch {
			case !exists:
				owner[key] = spec.PluginID
			case previous != spec.PluginID:
				problems = append(problems, pluginpkg.FieldError{
					Field: "provides.cli",
					Message: fmt.Sprintf("command %q is declared by both %s and %s",
						commandHead(capability.Path), previous, spec.PluginID),
				})
			}
		}
	}
	if len(problems) == 0 {
		return nil
	}
	return &pluginpkg.ValidationError{Errors: problems}
}

// cliCommand is one capability before it becomes a pluginpkg.CLICapability: the
// fields every command must declare, kept as a literal so a new command cannot
// be added without its summary, schemas, permissions, danger, idempotency and
// callers.
type cliCommand struct {
	path        []string
	summary     string
	description string
	args        map[string]any
	output      map[string]any
	permissions []string
	danger      pluginpkg.Danger
	idempotent  bool
	callableBy  []pluginpkg.Caller
	examples    []string
}

// capability converts the declaration into the contract type, copying the
// slices so the returned node never aliases a package-level caller set.
func (c cliCommand) capability() pluginpkg.CLICapability {
	return pluginpkg.CLICapability{
		Path:         append([]string(nil), c.path...),
		Summary:      c.summary,
		Description:  c.description,
		ArgsSchema:   c.args,
		OutputSchema: c.output,
		Permissions:  append([]string(nil), c.permissions...),
		Danger:       c.danger,
		Idempotent:   c.idempotent,
		CallableBy:   append([]pluginpkg.Caller(nil), c.callableBy...),
		Examples:     append([]string(nil), c.examples...),
	}
}

// capabilities converts a declaration table into contract nodes.
func capabilities(commands []cliCommand) []pluginpkg.CLICapability {
	out := make([]pluginpkg.CLICapability, len(commands))
	for i, command := range commands {
		out[i] = command.capability()
	}
	return out
}

// capabilitiesCopy deep-copies declared capabilities, so a returned tree shares
// no mutable state (paths, permission lists, caller lists, schemas) with the
// spec it came from.
func capabilitiesCopy(capabilities []pluginpkg.CLICapability) []pluginpkg.CLICapability {
	copied := make([]pluginpkg.CLICapability, len(capabilities))
	for i, capability := range capabilities {
		capability.Path = append([]string(nil), capability.Path...)
		capability.Permissions = append([]string(nil), capability.Permissions...)
		capability.CallableBy = append([]pluginpkg.Caller(nil), capability.CallableBy...)
		capability.Examples = append([]string(nil), capability.Examples...)
		capability.ArgsSchema = copySchema(capability.ArgsSchema)
		capability.OutputSchema = copySchema(capability.OutputSchema)
		copied[i] = capability
	}
	return copied
}

// copySchema deep-copies one JSON Schema document.
func copySchema(schema map[string]any) map[string]any {
	if schema == nil {
		return nil
	}
	copied := make(map[string]any, len(schema))
	for key, value := range schema {
		copied[key] = copySchemaValue(value)
	}
	return copied
}

// copySchemaValue deep-copies one decoded schema value.
func copySchemaValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return copySchema(typed)
	case []any:
		copied := make([]any, len(typed))
		for i, item := range typed {
			copied[i] = copySchemaValue(item)
		}
		return copied
	case []string:
		return append([]string(nil), typed...)
	default:
		return value
	}
}

// callerAll is every caller face: the human CLI/settings page, an agent's own
// tool face and a team captain's team CLI.
var callerAll = []pluginpkg.Caller{pluginpkg.CallerUI, pluginpkg.CallerAgent, pluginpkg.CallerTeamCaptain}

// callerUIAgent is the human face plus an agent's own tool face: configuration
// writes an agent may perform on its own behalf.
var callerUIAgent = []pluginpkg.Caller{pluginpkg.CallerUI, pluginpkg.CallerAgent}

// callerUICaptain is the human face plus a captain's team CLI: the team
// self-assembly commands, which the team's teamCli switch can take away.
var callerUICaptain = []pluginpkg.Caller{pluginpkg.CallerUI, pluginpkg.CallerTeamCaptain}

// callerUIOnly is the human face alone: destructive commands stay out of every
// agent surface, so no model can delete a provider, an agent or a team.
var callerUIOnly = []pluginpkg.Caller{pluginpkg.CallerUI}

// modelProviderTypes is the first-batch provider type vocabulary
// (specs/provider-registry: llm/image/video/decision/tts/stt). It is a
// documentation hint, never an enum: the type vocabulary is open, so a plugin
// may declare "embedding" and the CLI must accept it without a code change.
var modelProviderTypes = modelprovider.BuiltinTypeIDs()

// providerStatuses is the provider enablement vocabulary used by list/update,
// derived from the domain so the CLI cannot drift from the stored axis.
var providerStatuses = providerStatusStrings()

// providerStatusStrings renders the domain enablement vocabulary as strings.
func providerStatusStrings() []string {
	statuses := modelprovider.ProviderStatuses()
	names := make([]string, len(statuses))
	for index, status := range statuses {
		names[index] = string(status)
	}
	return names
}

// modelProviderVerbs is the frozen provider management set from
// specs/provider-registry: list|show|add|update|remove|test.
var modelProviderVerbs = []string{"list", "show", "add", "update", "remove", "test"}

// modelRequiredPaths freezes the model plugin's command coverage without
// deriving it from the declared tree.
func modelRequiredPaths() [][]string {
	paths := make([][]string, 0, len(modelProviderVerbs)+1)
	for _, verb := range modelProviderVerbs {
		paths = append(paths, []string{"model", "provider", verb})
	}
	return append(paths, []string{"model", "catalog"})
}

// agentRequiredPaths freezes the agent plugin's command coverage:
// list|show|create|update|delete|start|stop (specs/agent-registry).
func agentRequiredPaths() [][]string {
	return [][]string{
		{"agent", "list"},
		{"agent", "show"},
		{"agent", "create"},
		{"agent", "update"},
		{"agent", "delete"},
		{"agent", "start"},
		{"agent", "stop"},
	}
}

// teamRequiredPaths freezes the agent-team plugin's command coverage:
// list|show|create|update|delete|member add|remove|list|save|save|start
// (specs/agent-team + design.md §5: the captain needs `member list` to read the
// current seats and `member save` to persist its assembly).
func teamRequiredPaths() [][]string {
	return [][]string{
		{"team", "list"},
		{"team", "show"},
		{"team", "create"},
		{"team", "update"},
		{"team", "delete"},
		{"team", "member", "add"},
		{"team", "member", "remove"},
		{"team", "member", "list"},
		{"team", "member", "save"},
		{"team", "save"},
		{"team", "start"},
	}
}

// commandHead renders a path as the command a person runs ("keel team member
// add"), which is how a coverage error names the missing command.
func commandHead(path []string) string {
	if len(path) == 0 {
		return "keel"
	}
	return "keel " + strings.Join(path, " ")
}

// objectSchema builds a JSON Schema object document.
func objectSchema(properties map[string]any, required ...string) map[string]any {
	schema := map[string]any{"type": "object"}
	if len(properties) > 0 {
		schema["properties"] = properties
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

// arrayOf builds a JSON Schema array document over items.
func arrayOf(items map[string]any) map[string]any {
	return map[string]any{"type": "array", "items": items}
}

// schemaRef references a definition declared by the plugin's extensibility
// document.
func schemaRef(name string) map[string]any {
	return map[string]any{"$ref": "#/definitions/" + name}
}

// stringProperty builds a JSON Schema string property.
func stringProperty(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

// booleanProperty builds a JSON Schema boolean property.
func booleanProperty(description string) map[string]any {
	return map[string]any{"type": "boolean", "description": description}
}

// integerProperty builds a JSON Schema integer property.
func integerProperty(description string) map[string]any {
	return map[string]any{"type": "integer", "description": description}
}

// enumProperty builds a JSON Schema string property restricted to values.
func enumProperty(values []string, description string) map[string]any {
	options := make([]any, 0, len(values))
	for _, value := range values {
		options = append(options, value)
	}
	return map[string]any{"type": "string", "enum": options, "description": description}
}
