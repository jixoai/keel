# Proposal: keel-provider-config

> 立项：2026-11-09。参考：zcode `docs/working-memory/provider-refactor/design-v2/design.md`
> （Provider/Model 配置、解析、Registry、ModelFactory、选择与执行边界）。keel 只取结构，
> 不搬应用层机制；内核 loop 与 PI 契约不动。

## Why

keel 现在的模型配置是一层 **CLI 式解析器**（`internal/provider` 从 pigo 移植）：按 model 字符串
猜 provider、按 env 取 key、协议靠 flag，模型能力/选项/上下文窗口没有事实源。由此产生三类真问题：

1. **身份不成立**：只按 modelId 路由，同一模型经不同网关（协议、baseURL、能力）会解析错；
   真实模型 E2E 已经撞过一次「把 provider 名当 model id 发上线」。
2. **能力散落**：上下文窗口只在租户文档里有一个全局值；thinking 档位在 runner 里硬编码
   `ThinkingLow`；附件能否进模型没有模型能力可查（当前只能整体降级 queue）；压缩/子代理/
   后续所有执行分支都在各自猜。
3. **配置更新会改到在途请求**：没有「解析一次、冻结执行」的边界。

## What Changes

在**产品层**新增 `internal/modelcfg`（纯数据，零 IO、零内核依赖）：

- Provider Config（访问方式/协议/端点/静态 header）与 Model Config（属性/选项）分离，
  **身份永远是 `providerId/modelId`**；
- 内置 catalog（go:embed JSON）→ 租户 overlay（稀疏：缺省继承、null 清除）→ 选择级精确覆盖，
  解析出完整 Effective 配置；不完整/禁用的条目保留在设置视图但**不可执行**；
- 有序规则匹配（模型族 → 模型+协议 → 站点 URL → 精确 provider+model），数组顺序即优先级；
- 模型属性（contextWindow / inputFormat / outputFormat / tool / jsonSchema / nativeWebSearch）
  与选项规格（reasoningLevel、maxOutputTokens）成为唯一事实源；
- `ModelFactory`：`Selection{providerId, modelId, options.reasoningLevel}` → 不可变 ActiveModel，
  run 在 claim 时创建并随 loadout 冻结；
- 设置视图与可执行 Registry 来自同一次解析（`GET /v1/providers` / `GET /v1/models`）。

**两处刻意简化**（记录为 v1 范围，非永久裁决）：
- 不引入 CEL 表达式引擎：`optionSpecs.*.map` 用闭集补丁模板（值 → 请求字段补丁），
  未知 pid 的映射 fail-closed；
- 不做账号/权益 overlay（没有账号体系）：保留 overlay 层形状，但不实现 Account Source。

## Impact

- `internal/modelcfg`（新）：类型、规则、解析、工厂、校验、内置 catalog；
- `internal/servicehost`：启动时装配 resolver/registry，按 run 注入 ActiveModel；
- `internal/agent`：runner 接受解析后的 thinking 档位/上下文窗口/最大输出，不再硬编码；
- `internal/agentapi`：`GET /v1/providers`、`GET /v1/models`；selection 作为 user.message 的账本事实；
- 租户文档的 flat `agent.model/baseURL/protocol/provider` 归一为一条 selection（兼容旧部署）；
- 内核零改动（`internal/kernel`、`internal/loop` 不感知 provider 配置）。
