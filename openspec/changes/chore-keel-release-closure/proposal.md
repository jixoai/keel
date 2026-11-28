# Proposal: chore-keel-release-closure

> 立项：2026-11-28（v0.5.0 已发布：tag `v0.5.0` → commit `d0184d02`，GitHub Release
> `https://github.com/jixoai/keel/releases/tag/v0.5.0`）。定位：发布收尾，不改运行时行为、
> 不改已发布 tag/资产。

## Why

1. **keel 的发布链仍带着 fde2e 聚合仓分支**：`.github/workflows/release.yml` 末尾有
   `FDE2E_RELEASES_TOKEN` 门控的 `jixoai/fde2e-releases` 镜像步骤；EN/ZH 官网
   `harness/release.md` 与 `openspec/changes/MAINLINE.md` 仍把它描述为发布路径的一部分。
   keel 只应有自己的 GitHub Release；该 token 未配置时步骤空转，配置后又会把同一份字节
   复制到另一个仓，两个发布页天然会漂移，且让 keel 的发布依赖父项目 secret。
2. **28 个 keel 主 spec 的 Purpose 仍是归档占位**（`TBD - created by archiving change …`），
   `openspec validate --specs --strict` 因此 44 项里 28 项失败，主规格库不可作为可信入口。
3. **v0.5.0 的发布事实没有在 change 轨迹里收口**：tag/commit、Release URL、官网下载页、
   SHA-256 清单、未验证项（Windows 仅构建未实机验证）散落在对话与发布页，需要一条可复核
   的收尾记录，供父仓集成与下一版发版沿用。

## What Changes

1. **删除聚合仓镜像**：`.github/workflows/release.yml` 删除 `FDE2E_RELEASES_TOKEN` 与
   `jixoai/fde2e-releases` 镜像步骤及其注释；发布只走本仓 Release
   （`gh release create/upload --repo "$GITHUB_REPOSITORY"`），不再要求任何跨仓 token。
2. **文档对齐**：`site/docs/harness/release.md`、`site/docs/zh/harness/release.md` 删除
   “聚合仓/镜像到 fde2e-releases”条目；`openspec/changes/MAINLINE.md` 的运维债不再把
   `FDE2E_RELEASES_TOKEN` 未配置列为缺失。保留 `gh.fde2e.tweb.xin` 这类 **GitHub 代理镜像**
   （它只是换 host，仍指向 `jixoai/keel` 的同一 Release 路径，不属于聚合仓）。
3. **补齐 spec Purpose**：只改 `openspec/specs/*/spec.md` 的 `## Purpose` 段，按各 capability
   的真实需求写实；不动 Requirements、不动 `openspec/changes/archive/**` 历史。
4. **记录 v0.5.0 收尾**：在 `evidence/release-v0.5.0.md` 记录 tag/commit、Release URL、
   五平台资产、web zip、`SHA256SUMS.txt`（逐资产 SHA-256）、官网下载页、校验命令、镜像状态
   与已知外部项；Windows 明确写“仅 CI 构建，未实机验证”。
5. **验证**：workflow YAML 解析；`openspec validate --specs --strict` 与
   `openspec validate chore-keel-release-closure --strict`；站点链接/引用门禁；
   `go build ./...` / `go vet`（不跑全量 TUI，留最终集成）。

## Capabilities

### New Requirements

- `distribution`: “发布只产出于本仓 GitHub Release”——tag/dispatch 触发、本仓单一出口、
  禁止聚合仓镜像与跨仓 token、已公布 tag/SHA 不重建。

### Modified Capabilities

<!-- 无。v0.5.0 运行时行为、HarnessAPI、插件契约均不变；本 change 只收口发布链与规格库
     Purpose，不新增/修改任何运行时 Requirement。 -->

## Impact

- `.github/workflows/release.yml`（仅删除尾部镜像步骤与相关注释）。
- `site/docs/harness/release.md`、`site/docs/zh/harness/release.md`（删除聚合仓描述）。
- `openspec/changes/MAINLINE.md`（运维债一条）。
- `openspec/specs/{agent-loop-contract,app-shell,checkpoint-policy,client-shell,
  collaboration-core,collaboration-subagent,creation-mode,deliverables,demo-provider,
  distribution,harness-core,layerfs,model-catalog,naming,pi-hooks,plugin-architecture,
  plugin-system,plugin-ui-extension,provider-credentials,session-query,session-title,
  site,supervision,task-log,tool-surface,trace-hygiene,ui-craft,ui-kit}/spec.md`
  （只改 Purpose）。
- 不改：`v0.5.0` tag、Release 资产、`SHA256SUMS.txt`、版本常量、Go 源码、web 源码、插件
  清单；不删除 `jixoai/fde2e-releases` 仓本身（那是父项目资产，不在本仓权限内）。

## Out of scope

- 不重新构建/重新上传 v0.5.0，不修改已公布 SHA。
- 不验证 Windows 实机启动；Windows 仍为“CI 构建成功、未实机验证”。
- 不跑全量 TUI/E2E（留最终集成）。
- 不清理父仓或其他子仓中的 keel 发布记录。
- 不引入新的发布目标（Homebrew、npm、Docker Hub 等）。
