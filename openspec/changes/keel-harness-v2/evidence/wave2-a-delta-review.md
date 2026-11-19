# Wave 2 A 轴 seam 增量复核与处置（2026-11-20）

对象：冻结快照 `597ad7e`（增量 `06e3747..597ad7e`，核心为 seam `de680cf`）。复核：独立 reviewer
子代理（只读；自跑 build/race/真库 DSN 测试 + 四个原创探针，探针在 /tmp 且已删除）。
**结论：8.5/10，P0=0、P1=0，P2×7。**

## 七个审查面结论

| 面 | 结论 | 关键证据 |
|---|---|---|
| 跨会话/跨租户隔离 | 通过 | 身份只由 `serve.go:103-118` 从 `RunScope` 盖章；file scope 与 bash identity 同源；`TestSessionsGetIndependentWorkspaces` |
| 路径逃逸 | 通过 | `WriteFileFor` 锚定根 + gbashfs `path.Clean` 内存图；探针：`..`/绝对路径零宿主残留；symlink 目标在图内不可解析 |
| capture 正确性 | 通过 | taskId 经 task gate 会话内卡校验；nohup key 与父 key 不撞；无 lease 时 0 capture/1 capture_failed 且 Close 报错（探针实证） |
| 沙箱作业命令 | 通过 | `jobs` 会话过滤、`ps` 租户视图；`joinNohupArgs` 对 `$(...)`/`;`/引号零展开（探针）；上限拒新不杀旧；`fg/bg` 127 |
| serve 装配 | 通过 | store→sandbox→manager→service 顺序与惰性 getter；服务监听前无 run；双桥无漂移 |
| 证据完整性 | 通过 | 测试全真断言；SKIP 项在真库下亲测 PASS；E2E 边界与作者标注一致，无虚报 |
| 凭据/日志 | 通过 | 新日志仅 tenant/session/call_id/tree/count/seq；证据无连接串；nohup 无 env 回显 |

## P2 与处置

| # | 问题 | 处置 |
|---|---|---|
| 1 | UI 桥无 run scope 时静默回落 TempDirWorkspace，无告警/结果标注 | **W2.jobcap 补丁**：Mount 回落 Warn + 测试 |
| 2 | nohup 语义偏离未入 known items：默认 cwd 启动、重定向被父 shell 消耗 | **W2.jobcap 补丁**：w2.seam README known items |
| 3 | `ps` 暴露同租户他 session 命令文本（租户边界内） | **W2.jobcap 补丁**：注释明示「租户视图」；spec 允许 |
| 4 | 并发 capture 的 seq/快照时序：父/子全量快照可能使 `session_snapshot` latest 落后 manifest | **任务板立条目**（fork 接线前定序） |
| 5 | `session.Close()` 错误被 `_ =` 丢弃（service-nil / 双失败残余） | **W2.jobcap 补丁**：runSync/runJob 记日志 |
| 6 | `WriteFile` 覆盖 symlink 不跟随目标（与 POSIX 不同，无逃逸） | 文档注记；记 backlog |
| 7 | `test-full-dsn.txt` 非 verbose，单看无法确认 DSN 测试执行 | **W2.jobcap 补丁**：verbose/`-run` 转录 |

## spec-MUST 缺口裁定（记账，不阻塞本轮）

- **bash 删除对账（upsert-only）**：spec「执行空间产生的变更 MUST 通过 fs.capture 进入 layerfs」
  字面覆盖删除，但删除对账语义未定义、实现未做 → **任务板立条目**，不假装已满足。
- **capture 策略覆写（`config.toml` 的 checkpoint/manual）**：layerfs spec 的 MUST，当前只实现
  per-tool-call，覆写通道未接 → **任务板立条目**，注明依赖配置面切片。

其余已知项（workspace 无逐出钩子、后台作业 capture）已由 W2.jobcap 解决或属资源生命周期 backlog。

## 复核实证

- 自跑 `go build ./...`、`go test -race ./internal/bashbridge/`、`./internal/layerfs/gbashfs/`、
  `cmd/keel` wrapper 测试均 PASS。
- `KEEL_TEST_DSN_W2_RVA` 下 `TestBashJobCommandsAdapterMatchesTheToolSurfaceJobFacts` PASS，
  证明 `test-serve-wrapper.txt` 的 SKIP 在真库下确实执行。
- 探针：路径逃逸矩阵、symlink、`joinNohupArgs` 注入串、lease 过期 Close，全部收敛；宿主零残留。
