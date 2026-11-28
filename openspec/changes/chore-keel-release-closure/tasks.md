# Tasks: chore-keel-release-closure

## 0. 规格与基线

- [x] 0.1 proposal / specs 增量与验收口径对齐（distribution 新增“发布只产出于本仓”
      Requirement，三个 Scenario 覆盖工作流、聚合仓与文档）
- [x] 0.2 受影响文件盘点（before/after 清单）：
      `.github/workflows/release.yml`、`site/docs/harness/release.md`、
      `site/docs/zh/harness/release.md`、`openspec/changes/MAINLINE.md`、
      28 个 `openspec/specs/*/spec.md` 的 Purpose；不含 Go/web/插件源码。
      证据：`git diff --stat` = 32 个已跟踪文件 + 本 change 目录；28 个 spec 均为单行替换。

## 1. 删除聚合仓发布分支

- [x] 1.1 `.github/workflows/release.yml`：删除 `FDE2E_RELEASES_TOKEN` 与
      `jixoai/fde2e-releases` mirror 步骤；header 注释改为“本仓唯一出口”；保持版本门/
      构建/校验和/本仓 publish 步骤不变。
- [x] 1.2 复核工作流无 `FDE2E_RELEASES_TOKEN`、`fde2e-releases`、`AGG_TOKEN` 残留；
      `permissions` 仍只有 `contents: write`。
      证据：`evidence/verification.txt` §Active aggregate-repo references。

## 2. 文档对齐

- [x] 2.1 `site/docs/harness/release.md`：删除“Mirror — FDE2E_RELEASES_TOKEN →
      jixoai/fde2e-releases”条目与正文聚合仓 bullet；保留 GitHub 代理镜像说明。
- [x] 2.2 `site/docs/zh/harness/release.md`：同 §2.1 的中文面。
- [x] 2.3 `openspec/changes/MAINLINE.md` 运维债：删除“聚合仓 token 未配置”作为缺失项，
      改为记录 2026-11-28 起只发本仓。
- [x] 2.4 README / RUNBOOK / docs 全量检索：无聚合仓/token 指向（历史归档与父仓内容除外）。

## 3. 补齐主 spec Purpose

- [x] 3.1 28 个占位 Purpose（`TBD - created by archiving change …`）逐个改写为真实 Purpose，
      每段 1–3 句覆盖该 capability 的职责与关键不变量。
- [x] 3.2 只改 `## Purpose` 段：Requirements、Scenario、archived change 均不动；
      `git diff --stat` 显示 28 个 spec 各 1 行替换，`git diff -U0` 无 Requirement 行变化。

## 4. v0.5.0 发布收尾记录

- [x] 4.1 `evidence/release-v0.5.0.md`：tag/commit、Release URL、workflow run、五平台资产
      与 web zip、`SHA256SUMS.txt` 逐资产 SHA-256、官网下载页、校验命令。
- [x] 4.2 已知外部项：Windows 仅 CI 构建未实机验证；无代码签名；镜像探测结果；
      聚合仓无本次发布。

## 5. 验证

- [x] 5.1 workflow YAML 解析（Ruby Psych）通过；`actionlint -ignore SC2010` 通过。
      说明：裸 `actionlint` 的唯一告警是 `origin/main` 已存在的 SC2010（checksums 步骤
      `ls | grep`），与本次改动无关，未顺手改写以免影响发布资产排序。
- [x] 5.2 `openspec validate chore-keel-release-closure --strict` 通过。
- [x] 5.3 `openspec validate --specs --strict`：44 passed / 0 failed（残余项 0）。
- [x] 5.4 站点门禁：`check-citations.mjs` = `CITATIONS OK (1465)`；
      `cd site && pnpm build` = `SITE BUILD OK — 37 page pairs, 3314 links, 1465 citations verified`
      （采用 content-addressable store 安装固定 lockfile，未改 lockfile）。
- [x] 5.5 `go build ./...`、`go vet ./...`、`go test -run '^$' ./cmd/keel/...` 全绿。

## 审查门禁（发布收尾版；三轴按适用范围执行）

- [x] **A 安全红队（实现方自检）**：workflow 不含跨仓 token/secret 分支，
      `permissions: contents: write` 只服务本仓 release；无明文凭据。
  - 证据：`evidence/verification.txt` §Active aggregate-repo references。
  - 独立子代理复核：未在本 lane 执行；留给父仓最终集成（本 lane 只交付可 cherry-pick 分支）。
- [x] **B 功能与链路红队**：tag → version gate → 构建 → checksums → 本仓 Release
      链路在 v0.5.0 已真实跑通（run 46103711132，success）；文档下载/校验命令指向同一资产。
  - 证据：`evidence/release-v0.5.0.md`。
- [x] **C 视觉与设备红队**：N/A——本 change 无 UI/客户端改动；不改 web/TUI 代码与样式。
  - 证据：`git diff --stat` 只含 workflow/docs/openspec。
- [x] **P0/P1 清零**：发布链无 P0/P1；Windows 实机验证为已知外部项（P2）。
- [x] 缺陷分级表：P0=0，P1=0，P2=1（Windows 实机验证缺失，留发布验证 backlog）。

## 验收

- [x] 相关验证命令与退出码已贴 `evidence/verification.txt`
- [x] 规格变更已回写 change（distribution 新 Requirement）；主 spec Purpose 已写实
- [x] 不修改 v0.5.0 tag/资产/SHA；不推 main、不 force-push
