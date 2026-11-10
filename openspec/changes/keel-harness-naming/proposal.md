# Proposal: keel-harness-naming（对外统一 Harness）

## Why

Owner 裁决：keel 的产品层统一叫 **Harness**（原「Agent」），更准确专业。当前对外文件仍有大量
「Agent」字样（`README.md`、`docs/*`、`web/README.md` 等，侦察给出清单），且 `docs/AGENT.md:65`
的 `plugin.yaml` 说法与实际实现不符——官网会被这些不一致污染。

## What Changes

- 对外文档与用户可见串统一 **Harness**；协议术语 `AgentAPI` / `AgentBackend` **保留**并给
  「Harness（原 Agent）」别名说明（协议名是契约，不追改）。
- `docs/AGENT.md` 更名 `docs/HARNESS.md` 并按实现改写插件契约段（清单/生命周期以
  `keel-plugin-core` 为准）。
- 用户可见提示（`cmd/keel/serve.go` 默认系统提示、fixture 文案）改为 Harness。
- 术语表页（sites）与 `README` 术语一致；提供一次性扫描脚本（残留清单）。

## Non-Goals

- 不改 Go 包名/类型名（`agentext`、`agentapi` 等内部标识保留；改名成本高且非对外）。
- 不改协议字段/方法名。

## Impact

`README.md`、`docs/**`、`web/README.md`、`cmd/keel/serve.go`、`web/src/data/fixture.ts`；官网内容。
