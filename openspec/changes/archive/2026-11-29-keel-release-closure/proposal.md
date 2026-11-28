# Proposal: keel-release-closure

## Why

keel 是 jixoai 的独立开源产品，不属于 fde2e 产品线；发布只应使用 keel 自己的 GitHub Release 与官网下载页。当前 release workflow 仍带可选的 `jixoai/fde2e-releases` 镜像步骤和 `FDE2E_RELEASES_TOKEN` 文档，容易把 keel 与 fde2e 发布链混在一起。v0.5.0 已按独立 Release 发布，但收尾记录与 spec placeholder 仍需清理。

## What Changes

- 删除 keel `release.yml` 中的 fde2e 聚合仓 mirror 步骤与相关 secret 引用；keel 发布不依赖 fde2e。
- 更新 release 文档：只描述 keel 自己的 Release、SHA256SUMS、官网下载页与 `keel update`。
- 清理 keel 自身 specs 的 placeholder Purpose（仅 keel，不碰其它产品线）。
- 记录 v0.5.0 发布/更新验收：release page、SHA、官网下载页、二进制版本。
- 不重新发布 v0.5.0；已公布 SHA 不改。
