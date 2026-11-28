# distribution

## ADDED Requirements

### Requirement: 发布只产出于本仓 GitHub Release

`v*` tag（或显式 dispatch 的 tag）触发的发布 MUST 只把该版本的产物发布到
`jixoai/keel` 自己的 GitHub Release（`gh release create/upload --repo "$GITHUB_REPOSITORY"`）。
发布流程 MUST NOT 镜像或复制资产到 `jixoai/fde2e-releases`（或任何聚合仓），MUST NOT
读取 `FDE2E_RELEASES_TOKEN` 或新增任何跨仓发布 token；配置缺失不构成发布阻塞，配置存在
也不得产生第二份发布页。文档 MUST 把本仓 Release 页描述为唯一发布出口。

已公布的 tag 与其 `SHA256SUMS.txt` MUST 视为不可变：补发/修复只能通过新版本号完成，
MUST NOT 用重建产物或改写已公布 SHA 的方式让页面匹配。

#### Scenario: tag 发布只有本仓一个出口

- **WHEN** 推送 `v0.5.1` tag（且 `go run ./cmd/keel version` 与 tag 一致）
- **THEN** 工作流在本仓创建 Release 并上传五平台二进制、`keel-web-<ver>.zip` 与
  `SHA256SUMS.txt`
- **AND** 工作流没有聚合仓步骤、没有 `FDE2E_RELEASES_TOKEN` 引用，且不需要任何跨仓 secret

#### Scenario: 聚合仓不产生第二份发布

- **WHEN** 发布成功后检查 `jixoai/fde2e-releases`
- **THEN** 不存在本次 `keel-v<ver>` tag 的新发布；资产只存在于 `jixoai/keel`

#### Scenario: 文档只指向唯一出口

- **WHEN** 阅读 README、RUNBOOK 与官网发布/下载页
- **THEN** 下载与校验命令只指向 `https://github.com/jixoai/keel/releases/...`；
  GitHub 代理镜像仅做 host 替换，不指向任何聚合仓
