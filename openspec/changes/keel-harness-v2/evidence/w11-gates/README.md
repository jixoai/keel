# W11 收口门证据（2026-11-23）

Change: `keel-harness-v2` · 门判据: `acceptance.md` · 任务: `tasks.md` W11
Worktree: `.worktrees/keel-w11` · Branch: `task/keel-w11-gates` · Base: `a549e87`
环境: macOS arm64 / Go 1.27.4 / PostgreSQL 18.7（本机单实例，**无 Docker**）
测试库: `KEEL_TEST_DSN=postgres://kzf@127.0.0.1:5432/postgres?sslmode=disable`
（每个用例自建独立 DB，跑完删除；见 §10 清理证据）

> **Rerun 2026-11-23 on the rebased base `a549e87`（新机，PG 已起）。**
> 本目录下 `vertical-integration.txt`、`e0-r1-live.txt`、`e0-r1-negative-control.txt`、
> `e0-r1-report.json`、`g0-*.txt`、`g5-*.txt` 均为本次 rerun 的实测输出；旧机
> `8d6252b` 的旧输出已替换（叶子路径 141→152、工具面/路由已经变化，旧输出不再代表本分支）。
> 本轮 rerun 暴露并修复了两个 rebase 后回归：① `PUT /v1/settings/providers/{id}/credential`
> 缺 accessgate 权限声明 → `keel serve` 启动即拒（§9）；② `plugins/COVERAGE.md` 的 compaction
> 行状态词不可解析 → catalog 门红（§2.2）。另把 sidecar 残留检查的 needle 从源目录改为脚本名，
> 并增加「卸载前必须先看到进程」的正控（§1.7）。

## 0. 结论表

| 门 | 判定 | 一句话 |
|---|---|---|
| W11 垂直集成（任务→bash→capture→面板→fork→合并→卸载插件） | **PASS** | 真 `keel serve` + 真 PG + HTTP：全链路一次跑通（§1） |
| E0 既有门零回归 | **BLOCKED**（R1 子门 PASS） | R1 differential `unexpected=0` + 两类负控拒绝；`acceptance-all.sh` 的 DB reset 已走本地 psql，但 drain/inflight/perf/handoff/rss 仍依赖 docker exec/cgroup；core e2e 7/8（1 条旧夹具缺陷，§2） |
| G0 契约门 | **PASS** | 修复了 kernel→layerfs 越界；11 工具快照/生成名负控/插件同层句柄负控/catalog 全绿（§3） |
| G1 状态机门 | **FAIL** | 无逐状态机转移矩阵测试、无「拒绝事实」矩阵、无状→事实映射脚本（§4） |
| G2 恢复门 | **FAIL**（kill -9 子项 PASS） | 进程级 kill -9 续跑、子代理原任务续跑、产品级 SIGKILL 三格绿；容器重启 E2E 无 Docker 不可测；capture 崩溃格缺失（§5） |
| G3 幂等与冲突门 | **FAIL**（4/5 判据 PASS） | provider 额度耗尽且未配 fallback 时仍重试同一 provider，违反 supervision spec「立即停止重试该 provider，按 fallback 切换或升级」（§6） |
| G5 任务门 | **PASS** | taskId 强制、fork eventSeq 精确、资源 (path,hash)、write→bash 读、十万文件 1 条 capture 事件全绿（§7） |

> `acceptance.md` 的依赖顺序要求 E0→G0→G1 先绿，后续门才作为验收依据。本文件仍按主线要求
> 逐门给出可复跑证据与失败点，**G1/G2/G3 的未过项不作为后续门的依据**。

## 1. W11 垂直集成（PASS）

新增 `internal/producte2e/vertical_w11_test.go`：构建真 `cmd/keel`，对独立 PG 库启动
`keel serve -auth local-only`，用 web 客户端同款 HTTP 路由完成七步；每一步都以事件/接口读数断言，
不以自评文字代替。

```bash
KEEL_TEST_DSN='postgres://kzf@127.0.0.1:5432/postgres?sslmode=disable' \
  go test ./internal/producte2e -run TestVerticalW11 -count=1 -v
```

输出（`vertical-integration.txt`）：

```
--- PASS: TestVerticalW11 (3.52s)
PASS
ok  	github.com/jixoai/keel/internal/producte2e	4.284s
```

> **本轮 rerun 修的真实回归（rebase 后）**：`keel serve` 在 `a549e87` 上**启动即拒**：
> `route permission matrix: accessgate: 1 /v1 route(s) have no permission declaration:
> PUT /v1/settings/providers/{id}/credential`。该路由由 `4376a45` 引入但漏了 accessgate
> 声明，所以任何 `keel serve`（包括本测试）都起不来。已在 `internal/accessgate/matrix.go`
> 补 `ModeKey` + `secret.write`（它写的是秘密存储，models 文档另由 PUT /v1/settings/models 落）。
> 修复前本测试的失败形态是「health 40s 超时 + supervisor 刷 closed pool」，不是断言失败。

断言链（每条都在测试里，失败即红）：

1. **任务创建**：输入脚本 `tasks-create: write a report` 经 `tasks` 聚合工具落 `task.created`
   （`title=write a report`, `origin=tool`），面板 `cards[]` 可见。
2. **bash 执行**：同一轮的第二条指令经 `bash` 聚合工具真跑 `printf … > out.txt`，
   `tool.result{isError:false}`，命令退出码 0。
3. **capture**：`fs.capture` 恰好 **1** 条（一次 bash 一次结算），payload 带
   `taskId`/`count=1`/`treeHash=sha256:…`；`/files` 与 `/file-preview` 读回 `out.txt`
   内容与 hash。
4. **面板可见**：`GET /v1/sessions/{id}/task-panel` 返回该卡与 `task.created` 事件流。
5. **fork 子代理**：`POST /v1/sessions/{id}/fork` 返回 `origin=fork`、
   `originSeq == fork 时父账本最后 seq`、`parentTaskId == 活动卡`，并继承
   `configHash`/`toolManifestHash`；子会话只有 1 条自己的输入（前缀只读不复制）。
6. **合并结果**：父面板出现 `members[]`（`origin=fork`, `originSeq` 精确）且父账本追加
   `task.note`，正文含子会话 id 与子代理 summary。
7. **卸载插件零残留**：
   - sidecar 夹具 `w11.probe`（`provides.tools: [w11_probe]`）安装→启用后，发布名
     `w11_probe_w11_probe` 出现在工具面且 `run.finished.toolManifestHash` 改变；
   - UI 夹具 `keel.w12.fixture`（6 条 contributes）安装→启用；
   - disable→uninstall 后：inventory 无两条目、贡献消失、受管目录删除、无 sidecar 进程存活、
     generation 前进；**卸载后新 run 的 toolManifestHash 精确回到安装前基线**
     （Bot 白名单里故意保留那个悬空工具名，证明 registry 交集不再授予任何东西）。
   - sidecar 进程检查是**正控 + 负控**：夹具脚本名是唯一的 `w11-probe-sidecar.mjs`
     （entry command 是 `node <relative>`，cwd 才是包目录，所以「包目录」不在命令行上——
     旧 needle 指向源目录，`pgrep` 永远匹配不到，检查是空的）。启用后先 `waitForProcess`
     断言进程存在，卸载后再 `waitNoProcess` 断言消失；两步都失败即红。

为驱动模型工具调用，`internal/agent/fake.go` 增加行锚定的脚本指令
（`tasks-create:<title>` / `bash:<command>`，每个回合一条，工具结果后推进），
并有 `TestFakeScriptedDirectives` 钉住「散文不触发、脚本耗尽后回文本」。

## 2. E0 既有门零回归（BLOCKED；R1 子门 PASS）

### 2.1 R1 differential（PASS）

```bash
scripts/build-r1-baseline.sh          # git archive <pin.ref> + go build ./cmd/runtime
go build -o /tmp/keel-w11-candidate ./cmd/runtime
go run ./cmd/r1diff -old-bin /tmp/keel-r1-base -new-bin /tmp/keel-w11-candidate \
  -dsn 'postgres://kzf@127.0.0.1:5432/keel_w11_r1diff?sslmode=disable' -out /tmp/r1 \
  -pin cmd/r1diff/baseline.json -allow cmd/r1diff/allow-v2.json \
  -port-old 13770 -port-new 13773
```

> 端口用 13770/13773：本机 13771/13772 已被无关的 python 静态服务占用（`lsof` 可见），
> 不改他人进程。`r1diff` 默认的 13746/13747 也可用，跑前先 `lsof` 确认。

`e0-r1-live.txt`：

```
R1 differential: 152 leaf paths compared, 103 difference(s), 103 allowed, 0 unexpected
verdict: PASS (no difference outside the allowlist; every allow rule fired)
```

负控有两类（`e0-r1-negative-control.txt`），都必须 FAIL：

1. **同一份 dump 两边**（每条 allow 规则都会 stale）：
   `verdict: FAIL (stale allow rules: registered differences did not happen)`；
2. **注入一个不在 allowlist 的真实差异**（把 `runStatus.body.status` 从 `done` 改成
   `failed` 再 diff）：`152 paths, 1 difference, 1 unexpected` +
   `UNEXPECTED /runStatus/body/status`，`verdict: FAIL (unexpected differences and stale allow rules)`。
   第 2 类证明门能抓到真实漂移，而不只是「规则没命中」。

**修复的真实缺陷**：R1 在 `8d6252b` 本身即红（我用 `git archive HEAD` 构建未改动的
candidate 复现，见 §9）。根因：`8a5794e` 给 bash 增加了显式 `checkpoint` action，工具 schema
变化，但 `cmd/r1diff/allow-v2.json` 的两条 `toolManifestHash` 仍钉着 `d105fb5` 时的旧值，
于是报 `STALE RULE`。已把两条 allow 重钉到当前 manifest hash
`sha256:5afcad2e…`，理由写明 8a5794e 是有意的 schema 变更；每次 attempt 仍只接受这一个精确值。

修复前负证据（`HEAD:cmd/r1diff/allow-v2.json` 作为 `-allow`，其它不变）：
`e0-r1-stale-pin-before-fix.txt` 报同样两条 `STALE RULE` 并 `verdict: FAIL`。

### 2.2 工具面/catalog（PASS）

```bash
node scripts/plugin-catalog.mjs --check
# plugin-catalog: OK (103 entries, §A/§B/§C mapped, §H current)   （g0-catalog.txt）
```

**修复的真实缺陷**：`plugins/keel.oar/` 目录与清单存在但 `catalog.json` 无条目，catalog 门直接
失败（`installed official plugin keel.oar has no catalog entry`）。已补 `keel.oar` 条目：
`status=partial`、`keelPluginId=keel.oar`、note 指向 `evidence/w09-oar.md`（record 层已绿，
专用渲染器与任务图闭环待 W12），并 `--write` 重生成 COVERAGE §H。

**本轮 rerun 修的真实回归（rebase 后）**：`plugins/COVERAGE.md` §A 的 `compaction` 行状态格是
中文叙述（「压缩核心已交付；…」），不含闭集状态词，`plugin-catalog.mjs --check` 直接报
`COVERAGE §A row "compaction": status cell must name one of delivered|partial|planned|registered|merged|absent`。
已把该格改为 `delivered（压缩核心 + W12 pruner/offload 已交付；capability，无独立插件实体）`
并 `--write` 重生成 §H；现在输出是 `plugin-catalog: OK (103 entries, §A/§B/§C mapped, §H current)`。
注意口径：§A 的 `delivered` 指**能力面**；§H 的 `dsh.compaction`/`dsh.spill` 由 catalog 的
`coverage.status` 生成，仍是 `planned`（`covered:false`，无独立插件实体）——这是两套轴，
不是自相矛盾；旧 §H 手写的 `delivered` 才是越权声明，已被 `--write` 纠正。

### 2.3 全量 battery（BLOCKED）

`scripts/acceptance-all.sh` 无法在本环境完整运行，但**阻塞点已不是 DB reset**：

- `db_reset()` 已改为本地 `psql`（`scripts/acceptance-all.sh:97-102`，change `0c0138a`），
  不再依赖 `docker exec`；
- 仍依赖 Docker/cgroup 的脚本（按 `rg -c docker|cgroup` 实测）：
  `acceptance-drain.sh`（docker 6）、`acceptance-inflight.sh`（docker 5）、
  `acceptance-perf.sh`（docker 13 / cgroup 9）、`acceptance-handoff-rss.sh`（docker 17 / cgroup 9）、
  `acceptance-rss.sh`（docker 15 / cgroup 15）、`acceptance-g5-perf.sh`（docker 14 / cgroup 18）
  ——它们要 Dockerized PostgreSQL（`KEEL_PG_CTR`）与 Linux cgroup v2 / 2GB 内存上限，macOS 无；
- `acceptance-all.sh` 的 race-full/chaos/perf 也远超单机串行预算。

可独立复跑的分项（本机）：

> 本轮 `a549e87` rerun 实跑了：`go build/vet`、R1 + 两类负控、catalog、垂直集成、G0、
> G5（含 100k capture）。表中 conformance / core e2e / mcp-apps 三行沿用旧机 `8d6252b`
> 的记录，本轮未重跑（它们不在 W11 的 rerun 清单里）。

| 分项 | 结果 | 证据 |
|---|---|---|
| `go build ./...` + `go vet ./...` | PASS | `e0-build-vet.txt` |
| R1 differential + 负控 | PASS | `e0-r1-live.txt`, `e0-r1-negative-control.txt` |
| `cmd/r1diff` pin/allow 一致性 | PASS | 同上 |
| conformance（13 包并行） | **FAIL 5 tests**（串行全绿） | `e0-conformance.jsonl`, `e0-conformance-summary.txt`, `e0-conformance-serial-rerun.txt` |
| core e2e（8 cases） | 7 PASS / 1 FAIL | `e0-e2e-core.json`, `e0-e2e-core.log` |
| mcp-apps e2e（5 cases） | **5 PASS** | `e0-mcp-apps.json`, `e0-mcp-apps-assertions.json` |
| catalog | PASS | `g0-catalog.txt` |

**conformance 失败点（精确）**：5 条测试在并行跑包时失败、单独串行全部 PASS：
`internal/httpapi/TestInputRepairsBookkeepingAfterCrash` 与 `internal/agent` 的
`TestCheckpointCutInMovedUnreadInputFailSafe`、`TestRunnerResumesUnresolvedToolCall`、
`TestWorkerRetryThenSuccessCountsOnlyTerminalFinished`、`TestWorkerLeaseExpiredMetric`。
根因是这些包共用同一个 `KEEL_TEST_DSN`（`internal/agent/runner_test.go:testStore` 等直接
`store.Open(dsn)`），并行包之间的 worker/lease 事实互相可见；串行重跑证据在
`e0-conformance-serial-rerun.txt`（5/5 PASS）。这是**测试隔离缺陷**，不是产品路径失败；
`acceptance-all.sh` 的 `go test -race ./...` 同样会踩到。

**core e2e 失败点（精确）**：`tool result survives SIGKILL mid-execution`：
`cmd/runtime` 上创建的会话没有 Bot/Account 归属，A4.2b1 后工具授权 fail-closed：

```
Tool "mcp" is not authorized: this session has no resolvable account or bot identity.
(reason: no_session_principal)
```

同一条崩溃点现在由**产品面** mcp-apps 场景覆盖并通过（§5，`G2c SIGKILL during MCP tool
execution`），所以这是 core e2e 夹具过期，不是恢复逻辑回归。为定位新增了 `-cases` 选择器与
失败时的 marker 列表输出；同轮修复了 `run resumes after SIGKILL` 的读数竞态（见 §9）。

**mcp-apps 修复的真实缺陷**：该场景的会话也不带 Bot，A4.2b3a 后全部 run 直接 failed
（`session … is not owned by a bot and cannot run`）。已在场景启动时按租户
`account→role→profile→bot` 建 Bot + `mcp.use/mcp.read` 授权组，`POST /v1/sessions` 带
`botId`；5/5 恢复绿。

## 3. G0 契约门（PASS）

```bash
go test ./internal/architecture/... -count=1 -v
go test ./internal/toolsurface -run 'TestBuiltinSurfaceIsExactlyEleven|TestEveryGeneratedPerServerMCPNameIsRefused|TestApprovedPluginToolsChangeAndRestoreTheManifestHash|TestManifestHashStableAcrossMCPServers' -count=1 -v
go test ./internal/harnessext -run TestPluginTypesExposeNoSiblingStateHandle -count=1 -v
node scripts/plugin-catalog.mjs --check
```

| 判据 | 证据 |
|---|---|
| 内核包不得 import 产品层 | `g0-architecture.txt`（`TestKernelDoesNotImportProductLayers` / `TestKernelDoesNotImportV2Layers` 等全 PASS） |
| 工具注册名快照 = 11 内建 ∪ 已审批 `provides.tools` | `g0-toolsurface.txt`：`TestBuiltinSurfaceIsExactlyEleven`、`TestApprovedPluginToolsChangeAndRestoreTheManifestHash`、`TestManifestHashStableAcrossMCPServers` PASS |
| 负控：`spawn_subagent` / 具体 `mcp__fs__read` / **生成 N 个具体名** 均失败 | `TestBuiltinSurfaceIsExactlyEleven`（退役名 + `mcp__fs__read`）+ 新增 `TestEveryGeneratedPerServerMCPNameIsRefused`（5 servers × 5 tools = 25 个生成名逐个 `Get`/`Validate`，均 unknown tool） |
| 插件不得访问同层内部状态 | 新增 `internal/harnessext/plugin_boundary_test.go`：`Plugin`/`PluginEnv`/`RuntimeOptions` 反射断言不含 `*Manager`/`*Runtime`/`*SidecarLoader`/兄弟 `*Plugin` 句柄；往 `PluginEnv` 加一个 Manager 句柄即红（`g0-plugin-boundary.txt`） |
| 无第二套插件运行时 | `TestAssemblyPlaneStaysIndependent`（`g0-architecture.txt`）+ `internal/servicegraph` 套件 |
| catalog 唯一机读真源 | `g0-catalog.txt` + §2.2 |

**修复的真实缺陷**：`internal/agent` 直接 import `internal/layerfs`，违反
`TestKernelDoesNotImportV2Layers`（失败原文：
`boundary violated: github.com/jixoai/keel/internal/agent must not depend on …/layerfs`，
并传递到 `…/blob`）。根因是 capture policy 词汇（`CaptureMode`/`ParseCaptureMode`/
`WithCaptureMode`）错放在存储服务包。已迁到叶子包 `internal/capturepolicy`（纯数据契约），
agent/bashbridge/fileplugin/host/cmd/keel 改为引用它；layerfs 本身并不使用这些符号。
`internal/architecture` 由红转绿。

## 4. G1 状态机门（FAIL）

已跑绿的既有机器级测试（`g1-kernel-kinds.txt`、`g1-plugin-lifecycle.txt`、
`g1-g2-supervisor.txt`）：

- A5 词汇/分类/去重键：`TestEventKindsMatchStore`、`TestDedupeKeyNamespace`、
  `TestCommitClassesAndEdgeWhitelist`、`TestFinishStatusPolicy`；
- E1 插件双态机（包级/租户级）：install/enable/disable/uninstall 往返、依赖 `PENDING`、
  HMR 换代与回滚、坏包隔离、bundled 不可卸载、贡献随卸载消失；
- G 恢复/升级：supervisor 全套（auto/manual/never、backoff、升级阶梯、provider fallback、配置轮转）。

**未过项（按 `acceptance.md` G1 逐字）**：

1. `state-machines.md` 的每台状态机**没有转移矩阵测试**：现有测试只覆盖单条合法/拒绝转移，
   没有「矩阵全枚举 + 非法转移逐条被拒」的测试载体；A1/A2/A3/B*/C2–C4/D*/E2–E4/F*/H* 连单测都不齐。
2. **拒绝事实**：插件生命周期有 `install:refused` 等审计，但任务卡终态后 update、跨 epoch 双写等
   非法转移没有逐条的「拒绝并落账」断言；`tasklog.Close` 对已关闭卡是幂等返回，`Update`
   没有终态守卫（`internal/tasklog/service.go:277-295`、`:207-211`）。
3. **静态检查/状态→事实映射表**：仓库无此脚本；`state-machines.md` 的「权威事实」列没有机读化，
   也无 CI 断言每条状态都指到一个权威事实。
4. A5 的 `kindRegistry` 现有测试断言了 class 与词汇，但没有「每个 kind 的声明 key 与
   `state-machines.md` 表逐行相等」的矩阵。

结论：G1 需要新增门级测试/脚本，不能靠现有单测拼装成 PASS。

## 5. G2 恢复门（FAIL；kill -9 子项 PASS）

### 5.1 已绿的子项

```bash
KEEL_TEST_DSN=... go test ./internal/agent \
  -run 'TestCrashThenRestartResumesWithoutADuplicateFinal|TestCrashBeforeAssistantCommitReanswers|TestCrashAfterAssistantCommitDoesNotReanswer|TestCrashDuringToolTurnFillsResults|TestKillBetweenLoadAndFinishNeitherDuplicatesNorMisses' -count=1 -v
KEEL_TEST_DSN=... go test ./internal/harnessext -run TestResumeContinuesTheSameSession -count=1 -v
```

- **kill -9 + 重启**：真子进程 `SIGKILL`（precommit 已提交答案未 settle），重启 worker 接管过期
  lease → `run.status=done`、assistant **1** 条、`run.finished` **1** 条、`resume.attempt`
  (`lease_expired/adopted`) 带 configHash/configRevision、seq 连续无洞（`g2-kill9-recovery.txt`）。
- **子代理继续原任务**：`TestResumeContinuesTheSameSession`——同会话重排队、不新建卡、不重复输入、
  重复 requestId 幂等、恢复后完成原任务（`g2-subagent-resume.txt`）。
- **never/manual/auto 策略**：`TestScanNeverParksAndDoesNotResumeFailures`、
  `TestScanAutoResumesFailedRun`、`TestScanManualParksPendingRun`（`g1-g2-supervisor.txt`）。
- **产品级 SIGKILL 三格**（mcp-apps 场景）：SIGKILL after app document fetch、SIGKILL during
  listen reconnect、**SIGKILL during MCP tool execution**——重启后各恰好 1 条 `tool.result`
  且带 app binding（`e0-mcp-apps.json`）。

### 5.2 未过项

1. **容器重启 E2E BLOCKED**：`acceptance.md` 要求「容器重启后自动续跑」，本机无 Docker
   （`NO DOCKER`），无法构造容器重启；现有替代是进程 SIGKILL（5.1）。
2. **六格崩溃矩阵不齐**：模型流/工具/追加/压缩/SSE 有格；**capture 崩溃格缺失**
   （无「capture 提交前 SIGKILL → 恢复后仍只有一条 capture」的测试），
   compaction 格只有 checkpoint 切点测试，非完整六格矩阵。
3. 「恢复次数/原因/所用配置可查」在 `resume.attempt` payload 有断言；但「恢复不重复已提交副作用」
   只在 assistant/tool.result 计数上断言，capture 未断言。

## 6. G3 幂等与冲突门（FAIL；4/5 判据 PASS）

| 判据 | 测试 | 结果/证据 |
|---|---|---|
| 有副作用工具重试只一次效果 | `TestWriteIsIdempotentForTheSameToolCall`（同 toolCall 重放只一次写）、`TestFSWriteFollowsToolResultWithoutKeyCollision`、kernel 去重键命名空间 | PASS（`g3-file-write-idempotency.txt`, `g1-kernel-kinds.txt`） |
| 文件写前置条件不命中 `conflict` | `TestWriteReadPagingAndPreconditions` | PASS（`g3-file-write-idempotency.txt`） |
| webhook 重投不产生第二次会话 | `TestTriggerWebhookHMACAndInitialCardE2E`（重复投递 `deduped=true`、同 sessionId）、`TestTriggerFireCreatesBotOwnedSession` | PASS（`g3-webhook-decision-idempotency.txt`） |
| approve 重复作答 409 | `TestDecisionHTTPIdempotency`（同答案 `duplicate=true`；不同答案 **409**） | PASS（`g3-decision-409.txt`） |
| **provider 额度耗尽后停止对该 provider 重试** | `TestScanProviderFallbackSelection` 只覆盖「配置了 fallback → 切换」 | **FAIL** |

**失败依据**：`openspec/changes/keel-harness-v2/specs/supervision/spec.md:78-79`
要求「provider 报额度耗尽 → **立即停止重试该 provider**，按 `providerFallback` 切换或升级」。
现状（`internal/supervisor/supervisor.go:430-475`）在**未配置 fallback** 时仍
`RequeueRun` 同一 selection（继续重试同一 provider）；配置了 fallback 但校验失败时也照样重排
（`TestScanProviderFallbackInvalidStillResumes` 把这一行为钉成期望，`g3-provider-exhaustion.txt`）。
即 4/5 绿，第 5 条与 spec 相反；G3 不能判 PASS。

## 7. G5 任务门（PASS）

| 判据 | 测试 | 证据 |
|---|---|---|
| 每次工具调用有 `taskId` | `TestTaskGateStampsTheAmbientCard`、`TestTaskGateRejectsMissingOrForeignCards`、`TestTaskGatePassesAValidTaskIDThrough`、`TestTaskGateIsInvisibleToTheManifestHash`、`TestTaskGateDisabledPluginIsTyped` | `g5-taskid-gate.txt` |
| 任务树跨会话可拼 | `internal/tasklog` 全套（`TestSubagentCardLinksToParentActiveCard`、`TestForkCreatesANewCardPointingAtTheOrigin`、`TestNoteSubagentFinishedLandsOnTheParentCard`、panel 跨会话 members/ancestors） | `g5-tasklog.txt` |
| fork 原点 `eventSeq` 精确 | `TestForkCreatesAPrefixSessionCardAndInheritedBinding`（`OriginSeq == tip`、fork fact、前缀不复制）、`TestForkCapacityRetryConvergesToOneCard` | `g5-fork-resources.txt` |
| 资源只记 `(path, hash)` | tasklog attach/detach 事件（`TestAttachDetachNoteAndCloseAreEvents`、panel 资源折叠）、`TestSubagentReturnCarriesArtifactRefsAndParentNote`（file/tree/blob/tool 引用） | `g5-tasklog.txt`, `g5-fork-resources.txt` |
| `write` 后 `bash` 立即可读 | `TestWriteThroughReachesBash`、`TestFSWriteFollowsToolResultWithoutKeyCollision` | `g5-write-then-bash.txt` |
| 十万文件 capture 只一条批事件 | `TestCaptureOneHundredThousandFilesIsOneEvent`（100,000 文件、`count=100000`、`NewObjects=1`、`fs.capture` 事件 **1**、manifest 100,000 行、refcount 1） | `g5-100k-capture.txt`（本机 23.06s PASS；旧机 308.97s） |

> **G5 证据口径（不要过度解读）**：本节是 Go 包级/API 级断言（`tasklog`/`toolsurface`/
> `fileplugin`/`harnessext`），不是每条判据都经 `keel serve` HTTP 面复跑。产品 HTTP 面
> 的 task→bash→capture→panel→fork→merge 由 §1 的垂直集成覆盖；G5 的 PASS 口径是
> 「门判据在 API/存储契约层全部成立」，不是「全链路 HTTP 全覆盖」。

## 8. 负控与「不声称」

- R1 负控（两类）：同一 dump 两侧 → **FAIL（stale rules）**；注入一个未列入 allowlist 的
  差异 → **FAIL（unexpected difference + stale rules）**（§2.1）。
- Sidecar 残留检查（正控 + 负控）：启用后必须 `pgrep -f w11-probe-sidecar.mjs` 有进程，
  卸载后必须无进程；两步都在测试里，任一不成立即红（§1.7）。
- G0 负控：25 个生成的 `mcp__server__tool` 名逐个 unknown tool；往 `PluginEnv` 加 Manager 句柄
  → `TestPluginTypesExposeNoSiblingStateHandle` 红。
- G3 负控：同 webhook 两次 `deduped`；同审批答案两次 `duplicate`、不同答案 409。
- G5 负控：无/外星 card 的 tool call 被 `task_id_required`/`task_not_found` 拒绝并落 `tool.result`。
- 本文件**不声称** E0/G1/G2/G3 已过；tasks.md 只勾选实际通过的垂直集成项（G0/G5 因与失败门同
  行而未勾，行内注记判定）。

## 9. 本轮修复清单（代码/测试）

| 修复 | 文件 |
|---|---|
| kernel→layerfs 越界：capture policy 词汇迁到叶子包 | `internal/capturepolicy/capturemode.go`（新）、`internal/layerfs/capturemode.go`（删）、agent/bashbridge/fileplugin/host/cmd 引用 |
| R1 allowlist 重钉工具 manifest hash（8a5794e 的 bash checkpoint schema） | `cmd/r1diff/allow-v2.json` |
| catalog 补 `keel.oar` 条目并重生成 §H | `plugins/keel.plugin-market/catalog.json`、`plugins/COVERAGE.md` |
| core e2e `run resumes after SIGKILL` 读数竞态 → 轮询 run 行 | `cmd/e2e/main.go` |
| core e2e `-cases` 选择器 + MCP marker 失败诊断 | `cmd/e2e/main.go` |
| mcp-apps 场景 Bot 归属 + `mcp.use/mcp.read` 授权（A4.2b 后夹具过期） | `cmd/e2e/mcp_apps.go` |
| 垂直集成测试 + fake provider 脚本指令 | `internal/producte2e/vertical_w11_test.go`、`internal/agent/fake.go`/`fake_test.go` |
| G0 生成名负控 / 插件同层句柄负控 | `internal/toolsurface/toolsurface_test.go`、`internal/harnessext/plugin_boundary_test.go` |
| rebase 后回归：`PUT /v1/settings/providers/{id}/credential` 缺权限声明 → serve 启动即拒 | `internal/accessgate/matrix.go`（`ModeKey` + `secret.write`） |
| rebase 后回归：COVERAGE §A compaction 状态格不含闭集状态词 → catalog 门红 | `plugins/COVERAGE.md`（§A 状态格；`--write` 重生成 §H） |
| sidecar 残留检查 needle 指向源目录（命令行里没有）→ 改为脚本名并加正控 | `internal/producte2e/vertical_w11_test.go`（`w11-probe-sidecar.mjs`、`waitForProcess`） |
| 本轮 rerun 证据替换为 `a549e87` 实测（垂直/R1/G0/G5） | `evidence/w11-gates/{vertical-integration,e0-r1-*,g0-*,g5-*}` |

## 10. 清理证据

跑完后（2026-11-23 rerun 收尾实测）：

```text
$ psql "postgres://kzf@127.0.0.1:5432/postgres?sslmode=disable" -Atc \
    "select datname from pg_database where datname like 'keel_w11%' or datname like 'keel_agent_test_%'" 
(空；rerun 用的 keel_w11_manual / keel_w11_r1diff / keel_test 已 DROP ... WITH (FORCE))
$ pgrep -fl "keel-w11|keel-runtime|keel serve|w11-probe|keel-producte2e"
(空)
$ lsof -nP -iTCP:13770/13773 -sTCP:LISTEN
13770 free, 13773 free
$ ls -d /tmp/keel-w11-* /tmp/keel-r1-base
(无)
```

注意：13771/13772 是**本机既有的无关 python 静态服务**（2026-11-04 起就在），不是本轮的残留；
本轮 R1 因此改用 13770/13773。mcp-apps 场景自己的 `cleanup.json` 记录
`portFree=true/processAlive=false/workDirRemoved=true`（`e0-mcp-apps-cleanup.json`）。

## 11. 复跑入口（按门）

```bash
export KEEL_TEST_DSN='postgres://kzf@127.0.0.1:5432/postgres?sslmode=disable'
# 垂直集成（W11）
go test ./internal/producte2e -run TestVerticalW11 -count=1 -v
# G0
go test ./internal/architecture/... ./internal/toolsurface/... ./internal/pluginpkg/... -count=1
node scripts/plugin-catalog.mjs --check
# G1（现有机器级）
go test ./internal/kernel ./internal/harnessext ./internal/supervisor -count=1
# G2
go test ./internal/agent -run 'TestCrashThenRestartResumesWithoutADuplicateFinal' -count=1 -v
# G3
go test ./internal/fileplugin ./internal/harnessapi ./internal/supervisor -count=1
# G5
go test ./internal/tasklog ./internal/toolsurface ./internal/fileplugin ./internal/harnessext -count=1
# E0（可跑分项）
node scripts/plugin-catalog.mjs --check
go run ./cmd/r1diff …   # 见 §2.1
go run ./cmd/e2e -scenario core -bin <runtime> -dsn <dedicated> -port 13745 -reset -reset-confirm <db>
go run ./cmd/e2e -scenario mcp-apps -keel-bin <keel> -out <dir> -dsn <dedicated> -mcp-apps-port 13686
```

`scripts/acceptance-all.sh` 的 DB reset 已改为本地 psql（`0c0138a`），但完整 battery 仍会因为
`acceptance-drain/inflight/perf/handoff-rss/rss/g5-perf` 需要 Dockerized PostgreSQL
（`KEEL_PG_CTR`）+ Linux cgroup v2 + ≥2GB 容器预算而无法在 macOS/无 Docker 环境跑完；
只能跑上表分项（本轮已跑 build/vet、R1、catalog、垂直、G0、G5）。
