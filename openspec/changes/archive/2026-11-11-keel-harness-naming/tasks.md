# Tasks: keel-harness-naming

- [x] N1 术语扫描脚本（对外文件 → 命中清单；协议名白名单）
- [x] N2 `docs/AGENT.md` → `docs/HARNESS.md` 并按实现改写插件契约段
- [x] N3 `README.md`/`docs/*`/`web/README.md` 术语清扫
- [x] N4 用户可见串（默认系统提示、fixture 文案）
- [x] N5 术语表（site）与别名说明
- [x] Q1 扫描零残留（白名单除外）

## 已登记边界（交付时核实）

- 扫描白名单四类：`protocol`（AgentAPI/AgentBackend 协议名，81 处）、`code-ident`（仓库真实 Agent* 符号，16）、`alias`（改名说明行，8）、`upstream`（pi/pigo 的「Agent Skills 规范」，3）。新增命中会被脚本拒绝，负控已验。
- Go 包名/类型名（`agentext`、`agentapi`）按 Non-Goals 保留，未做改名。
