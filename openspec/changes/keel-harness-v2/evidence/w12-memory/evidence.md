# W12 记忆卫生证据（2026-11-23）

范围：`openspec/changes/keel-harness-v2/tasks.md` 的 W12「记忆卫生（W1–W2）」：
`keel.spill`、`keel.compaction` 扩展（tool-result-pruner + image-offload）、
`keel.prompt-cache`。`keel.checkpoint` 未在本切片内（仍为未完成）。

## 交付内容

### 1. `keel.compaction` 扩展（完整）

| 能力 | 实现 | 位置 |
|---|---|---|
| tool-result-pruner | 保留窗口之外、文本型且 ≥ 阈值的工具结果序列化为 `ToolResultMessage` JSON，写入 `spill.Store`，上下文替换为有界引用（ref、sha256、bytes、预览） | `internal/compaction/hygiene.go` `PruneToolResults`/`ApplyHygiene` |
| image-offload | 图片窗口之外的 `ImageRefContent` 替换为命名 `blobId`/MIME/size 的文本引用；provider 不再上传/内联旧图 | `internal/compaction/hygiene.go` `OffloadImages`/`RenderImageReference` |
| 两个接入点 | runner 在 **image resolve 之前** 对 rebuild transcript 执行一次（防 resume 重传旧图）；loop 在每个 turn 边界、auto-compaction 之前执行一次（拦截 run 内新增的大结果） | `internal/agent/hygiene.go`、`internal/loop/loop.go` `maybeHygiene` |
| 失败语义 | 存储错误/不完整引用/无 blobId 的图片 → 原文保留 + `HygieneReport.Failures`（runner/loop 以 warn 记录），run 不失败、不静默丢 | 同上 |
| 账本不变 | 只改 in-memory 请求视图；已提交消息与 run result 保持原文；测试断言 caller transcript 未被修改 | `internal/loop/hygiene_test.go`、`internal/agent/hygiene_test.go` |
| token 账 | 复用 `EstimateContextTokens`/`EstimateTokens`；报告 `TokensBefore/TokensAfter`，不新增计量口径 | `internal/compaction/hygiene.go` |
| 无第二存储 | 生产实现 `internal/spillblob` 复用租户 blob 表 + 共享 CAS，与 attachment 相同 uploader/reader | `internal/spillblob/blobstore.go` |

### 2. `keel.spill`（完整，含真实插件实体）

- 契约/策略：`internal/spill`（`Store`、`Scope`、`Ref`、`Policy`、`RenderReference`、
  `SpillText`、UTF-8 有界预览）。
- 生产后端：`internal/spillblob`（租户 blob；内容去重；Load 校验 sha256/size）。
- 内建插件：`internal/harnessext.NewSpillPlugin` + `internal/pluginpkg/builtin/keel.spill.yaml`
  （`in-process`，capability ids `keel.spill.text`/`keel.spill.load`，模型工具面仍为固定 11）。
- 恢复方式：引用中的 blob id 通过既有 blob 读取路径（HTTP / `blob.ImageSource`）取回；
  测试按引用逐字节恢复原始 `ToolResultMessage`。

### 3. `keel.prompt-cache`（完整于本仓能力面）

- `internal/promptcache`：`CanonicalTools`（按 name 排序）+ `CanonicalSchema`（对象键排序、
  数字 lexeme 保留）+ `Build`（system/tool/prefix digest）+ `Tracker`（system 变化、tool set
  增删、tool 声明变化、model 变化四类失效原因）。
- loop 在 `PromptCache` 开启时以规范化工具清单发请求；Anthropic 编码器在
  `StreamConfig.PromptCache` 时把 `cache_control: {"type":"ephemeral"}` 放在最后一个 tool
  （无 tool 时放 system content block）。OpenAI 无显式断点，但同样受益于字节稳定前缀。
- 缓存边界刻意位于 transcript 之前：修剪/外置/压缩只改边界之后的消息，不会击穿前缀。
- 失效清单写进包文档与 spec delta（`agent-loop-contract`）。

## 测试（全部串行 `-p=1`）

本地 PostgreSQL 18：`KEEL_TEST_DSN=postgres://kzf@127.0.0.1:5432/postgres?sslmode=disable`，
无 Docker。

```text
go test -count=1 -p=1 ./internal/spill/... ./internal/spillblob/... ./internal/compaction/... \
  ./internal/promptcache/... ./internal/provider/... ./internal/loop/... ./internal/agentloop/...
ok  internal/spill
ok  internal/spillblob       (真 PG：Spill→Load 逐字节、去重、租户隔离、空载荷拒绝)
ok  internal/compaction
ok  internal/promptcache
ok  internal/provider
ok  internal/loop            (两 turn 集成：第二次请求只带引用；caller transcript 不变)
ok  internal/agentloop
```

```text
go test -count=1 -p=1 ./internal/agent/...        ok  (60.5s)
go test -count=1 -p=1 ./internal/servicehost/... ./internal/harnessext/...  ok  (4.9s / 18.0s)
go test -count=1 -p=1 ./cmd/keel/...              ok  (13.6s)
go test -count=1 -p=1 ./cmd/runtime/...           ok  (0.4s)
```

关键测试：

- `internal/compaction/hygiene_test.go`
  - `TestPruneToolResultsReplacesOldLargeResultWithDurableReference`（引用身份、bounded 预览、
    按引用恢复逐字节原文、token 下降）
  - `TestPruneIsIdempotent`（每 turn 重跑不重复 spill）
  - `TestPruneFailureKeepsOriginal` / `TestPruneIncompleteReferenceKeepsOriginal`（失败保留原文）
  - `TestOffloadImagesOutsideRecentWindow` / `TestOffloadImageWithoutBlobIDIsKept`
- `internal/loop/hygiene_test.go`
  - `TestLoopAppliesMemoryHygieneAtTurnBoundary`（真实 loop 两请求，第二请求带引用；
    `OnHygiene` 报告；store 可恢复；输入 transcript 未被改写）
  - `TestLoopHygieneRequiresCompaction`（compaction 关闭时 hygiene 不运行）
- `internal/agent/hygiene_test.go`
  - `TestRunnerHygienePrePassOffloadsBeforeImageResolution`（旧图在 resolve 前变文本引用）
- `internal/spillblob/blobstore_test.go`（真 PG）
- `internal/provider/prompt_cache_test.go`（断点在最后 tool、opt-in、无 tool 时 system block、
  工具顺序不影响请求字节）
- `internal/promptcache/promptcache_test.go`（顺序无关、schema 键序无关、失效原因、边界）

## Spec / 任务 / 覆盖表

- `openspec/changes/keel-harness-v2/specs/agent-loop-contract/spec.md`：新增
  「记忆卫生只派生请求视图，不重写账本」Requirement + 4 个 Scenario。
- `openspec/changes/keel-harness-v2/tasks.md`：`keel.spill`、`keel.compaction` 扩展、
  `keel.prompt-cache` 勾选并附实现/测试位置。
- `plugins/COVERAGE.md`：compaction / spill 状态从 planned 更新为已交付（capability）。

## 已知边界（不假装完成）

- `keel.checkpoint` 不在本切片；仍无独立「何时落 / 保留多少」策略插件。
- 工具结果修剪只处理**纯文本** content；带图片/其他块的混合结果不部分溢出（避免引用
  与内容不完整）。图片本身走 image-offload。
- 模型没有新的“读取 spill 引用”工具：恢复走既有 blob 读取路径（HTTP/宿主能力），
  模型工具面保持固定 11 个。
- spill 载荷上限沿用 blob uploader 的 64MiB；超限时保留原文并记录失败，不截断。
- 生产 wiring 需要宿主传入 blob seam；`cmd/keel serve` 与 `cmd/runtime` 已接，
  测试/嵌入宿主未接时 pruner 自动关闭（图片外置仍工作）。
