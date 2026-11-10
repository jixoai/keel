# 术语：Harness

## ADDED Requirements

### Requirement: 对外统一 Harness

所有面向用户的文档、页面与界面文案 MUST 使用 **Harness** 指代 keel 的产品层；MUST NOT 再出现
「Agent」作为产品名（协议名 `AgentAPI`/`AgentBackend` 例外，且 MUST 在首次出现处标注
「Harness（原 Agent）」别名）。

#### Scenario: 扫描零残留
- **WHEN** 运行术语扫描脚本
- **THEN** 对外文件中不再有产品名的 Agent 用法；协议名命中被列入白名单并在术语表说明

### Requirement: 文档与实现一致

`docs/HARNESS.md` MUST 按实现描述插件契约（清单 schema、三种 kind、生命周期、权限），
MUST NOT 保留 `plugin.yaml` 已实现一类与事实不符的说法，除非 `keel-plugin-core` 已交付。

#### Scenario: 契约对表
- **WHEN** 审查插件章节
- **THEN** 每条声明都能在 `keel-plugin-core` 的 spec/实现中找到对应
