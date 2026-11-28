# Tasks: keel-windows-verification

> 验收对象：`keel-0.6.0-windows-amd64.zip`（发现 P0/P1，v0.6.0 不再宣称已验证）
> 与修复后的 `keel-0.6.1-windows-amd64.zip`（最终验收）。
> 机器：Windows 11 家庭版中文版 10.0.26300，AMD64。

## 1. v0.6.0 资产与版本（PASS）

- [x] 1.1 下载 zip + `SHA256SUMS.txt`；核对 GitHub Release asset digest
- [x] 1.2 zip 59,275,550 字节，SHA-256 `44d377...6274`，`sha256sum -c` OK
- [x] 1.3 `keel.exe version` → `keel 0.6.0`，`go1.27.3 windows/amd64`
- [x] 1.4 证据：`evidence/commands/01-assets.txt`、`02-extract-version.txt`、`03-version-signature.txt`

## 2. v0.6.0 首启（FAIL → 两个缺陷）

- [x] 2.1 无参启动：PG ready，但 13750 永不监听；180s 超时 → P0
- [x] 2.2 `keel db stop` + `keel db start`：20s 超时（exit 124），PG 已启动 → P0 复现
- [x] 2.3 根因：`pg_ctl start` 的 `CombinedOutput` 管道被 postmaster 继承
- [x] 2.4 修复 P0 后：health 200 但 `/v1/ready` 持续 503 缺 handoff 表 → P1
- [x] 2.5 记录 `evidence/defects.md`；修复进入 `fix-windows-pg-start` 与 `fix-serve-ready-schema`
- [x] 2.6 **v0.6.0 结论：不得称为 Windows 已验证**；资产不改

## 3. v0.6.1 修复验证（预览构建）

- [x] 3.1 两个修复的 Linux/Windows 回归测试 PASS
- [x] 3.2 0.6.1 测试二进制：`keel db start` 3s 返回；无参启动 health 1.6s / ready 0.1s
- [x] 3.3 会话 + 消息 API 201；完整停机重启后会话仍在、用户消息仍在
- [x] 3.4 `KEEL_BOARD_ENABLED=1` + tenant `board.enabled=true`：任务创建并在重启后仍在
- [x] 3.5 含空格/中文可执行路径与 `KEEL_HOME`：health/ready 200
- [x] 3.6 预览证据：`evidence/commands/07..16`、`evidence/screenshots/01..02`

## 4. v0.6.1 发布与真实资产复验

- [ ] 4.1 版本 0.6.1；OpenSpec strict 通过；独立复审 P0/P1=0
- [ ] 4.2 提交并推送 main；tag `v0.6.1`；release workflow 成功
- [ ] 4.3 下载 `keel-0.6.1-windows-amd64.zip`；SHA-256 与 `SHA256SUMS.txt` 一致
- [ ] 4.4 真实资产无参首启：health/ready/capabilities/Web/静态资源 200
- [ ] 4.5 真实资产会话/消息 + 完整重启持久化
- [ ] 4.6 真实资产 `KEEL_BOARD_ENABLED=1` 看板任务 + 重启持久化
- [ ] 4.7 真实资产含空格/中文路径（含内嵌运行时解压）启动
- [ ] 4.8 系统集成：SmartScreen/Defender、防火墙/回环、UAC、控制台/关闭、install 目标
- [ ] 4.9 `keel update --check` 解析 release 并判断版本
- [ ] 4.10 证据：`evidence/windows-verification.md`、`evidence/release-v0.6.1.md`、截图/命令输出
- [ ] 4.11 发布后切 `site/docs/{downloads,zh/downloads,index}.md` 与 `site/docs/harness/release.md` 到 v0.6.1；site workflow 绿

## 5. 收口

- [x] 5.1 更新 RUNBOOK 的 Windows 验收/排障
- [ ] 5.2 归档三个 change；三个 change strict 通过；全量 specs 的 7 个既有长 Requirement 警告记录在 evidence（非本次引入）
- [ ] 5.3 提交/推送归档与最终证据；输出 macOS 工作报告

## Evidence

- `evidence/defects.md` — P0/P1、非阻塞观察、v0.6.0 不可宣称已验证
- `evidence/windows-verification.md` — 最终 PASS/FAIL 表与截图索引（v0.6.1 发布后填）
- `evidence/release-v0.6.1.md` — tag/Release/SHA/CI/下载页
- `evidence/commands/` — 资产、版本、探针、会话、重启、看板、中文路径、系统集成
- `evidence/screenshots/` — Web 首页、会话详情/失败态；v0.6.1 截图补充
