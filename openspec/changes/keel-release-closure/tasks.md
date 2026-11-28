# Tasks: keel-release-closure

## 1. Remove fde2e coupling

- [ ] 删除 `.github/workflows/release.yml` 的 aggregate mirror 步骤和 `FDE2E_RELEASES_TOKEN`
- [ ] 清理 `docs/harness/release.md` / README / 站点中 fde2e 聚合仓与镜像 secret 的引用
- [ ] `git grep -n 'FDE2E_RELEASES_TOKEN\|fde2e-releases'` 在 keel 中无残留

## 2. Spec and release acceptance

- [ ] 修复 keel 自身 spec 的 placeholder Purpose（只动 keel）
- [ ] `openspec validate --specs --strict` 中 keel 相关 spec 通过
- [ ] 记录 v0.5.0 release page、SHA256SUMS、官网下载页、二进制 `keel version` 验收
- [ ] 更新发布/升级文档，明确 keel 独立 Release
- [ ] 最终 `openspec archive keel-release-closure --yes` + spec strict
