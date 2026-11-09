# Tasks: keel-provider-config

> P=解析核心 · C=catalog · F=工厂/运行期 · S=设置视图/API · W=接线 · Q=验收 · R=复核。
> 纪律：内核零改动；本包纯数据（零 IO/零内核依赖）；旧 flat 配置必须零改动可用。

## P. 解析核心（internal/modelcfg）

- [ ] P1 类型：ProviderConfig / ModelConfig / properties / optionSpecs / 规则集合 / Selection /
      Effective / Issues（JSON tag 与本 spec 字段逐字一致）
- [ ] P2 稀疏 overlay：缺省继承、`null` 清除、深合并；实现三层（builtin → tenant → selection）
- [ ] P3 有序规则匹配：modelRules → modelApiRules → providerSiteRules → providerModelRules；
      模型名大小写不敏感整串匹配；URL 规范化；数组顺序即优先级
- [ ] P4 完整性校验：Effective 必填项、option map 非空、reasoning values 有序非空；输出 issues
      而不是 panic；issues 带字段路径
- [ ] P5 单测：同名模型跨 provider、族→具体覆盖、site 规则、null 清除、未知模型、缺字段 issue

## C. 内置 catalog

- [ ] C1 `catalog/builtin.json`（go:embed）：至少覆盖本机在用的入口（s2api / a6api / deepseek /
      anthropic / openai / openrouter / ollama），每条带协议、baseURL 模板、能力与选项规格
- [ ] C2 校验器：catalog 载入即校验（规则引用存在的 provider、model 字段合法、无密钥）
- [ ] C3 单测：catalog 可载入；已知模型解析出预期能力；未知模型落到基线并标记

## F. ModelFactory 与运行期冻结

- [ ] F1 `ActiveModel`：provider/model/协议/端点/凭据引用/properties/options/请求补丁，只读
- [ ] F2 `Factory.Resolve(selection)`：由 Registry 完整配置创建；不可执行时返回结构化错误
- [ ] F3 请求补丁：reasoningLevel 值→补丁（openai reasoning_effort / anthropic thinking budget /
      其它协议按 catalog 声明）；maxOutputTokens→max_tokens；未知值 fail-closed
- [ ] F4 run 冻结：worker/servicehost 在 claim 时注入 ActiveModel；配置热更只影响新 run

## S. 设置视图与 API

- [ ] S1 `Resolution` 同时产出 settings（Effective + issues + executable/selectable）与 registry
- [ ] S2 `GET /v1/providers`（设置视图）、`GET /v1/models`（可选择的扁平列表，含能力摘要）
      → 端点已实现（agentapi/models.go：`Config.Models`／`ModelCatalog` 缝；隐藏 provider 剔除、
      空解析带 `no provider configuration is loaded`、未接线 501；models_test.go）；serve.go 已装配
      `rt.Models()`（modelSeam 适配器）＋`--models-config`／`KEEL_MODELS_CONFIG`（`host.LoadModelsDocument`
      → `tenantCfg.Models`）；真机冒烟通过（8 provider／10 模型、hidden 不泄、未接线 501）
- [ ] S3 selection 作为 user.message 的账本事实：校验 + 写入 payload；运行取最新已提交 selection
      → 边缘侧完成（httpapi：`Config.ValidateSelection` 缝；present 且未接线/校验失败/非对象一律 400，
      对象逐字进 payload；selection_test.go 覆盖五类）；运行侧已由 b8c4a2b 落地；serve.go 已接
      `rt.Models().ValidateSelection`（真机冒烟：未知模型/未知 provider/非对象 = 400 且零写入）；
      残留：`/v1/sessions/{id}/events` 裸边不校验词汇（同 delivery 现状）
- [ ] S4 租户 flat agent.model/baseURL/protocol/provider 归一为匿名 provider + selection（兼容）

## W. 接线

- [ ] W1 servicehost：启动装配 resolver/registry；把 ActiveModel 映射到 Runner
      （Model/Provider/APIKey/Stream/ThinkingLevel/ContextWindow/MaxOutputTokens）
- [ ] W2 runner：移除硬编码 `ThinkingLow`；思考档位与最大输出从 ActiveModel 来；压缩用模型
      contextWindow（租户值仅作覆盖）
- [ ] W3 agentapi/客户端契约：selection 形状进入 `GET /v1/models` 响应与 input 请求体；
      wire 类型零改动（chat-surface 不感知）
      → 服务端侧完成：`/v1/models` 以 ModelView（properties/options/issues）出模型形状；input 接收
      可选 `selection` 对象（缺省字节不变、逐字入 payload）；客户端零改动（增量可选字段）
- [ ] W4 内核纯度：`go list -deps` 断言 kernel/loop/agent 不 import modelcfg

## Q. 验收

- [ ] Q1 真实模型 E2E：`providerId/modelId` 到达线路；reasoningLevel=low/high 两档都成功；
      maxOutputTokens 生效（请求体断言）
- [ ] Q2 兼容：旧 flat 配置启动行为不变；R1 与既有 16 门不退化
- [ ] Q3 设置视图：不完整 provider 在 `/v1/providers` 可见且不可执行，`/v1/models` 不含
- [ ] Q4 冻结：运行中切换配置，当前 run 不受影响（进程内测试 + 说明）

## R. 复核

- [ ] R1 子代理对抗复核（解析优先级/overlay 语义/凭据不落日志/内核纯度），阻塞修完再复审
- [ ] R2 全量验收、报告、archive
