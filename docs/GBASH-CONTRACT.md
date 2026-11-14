# GBASH 契约（W0 冻结）

本文件是 keel `bash` 主桥对上游 **gbash** 的唯一契约来源。它把依赖钉死到「版本 / API 边界 /
进程与取消模型 / 许可证 / 注入面 / 风险与升级策略」。**本文件未冻结前，`bash` 的实现任务保持
阻塞**（`openspec/changes/keel-harness-v2/specs/tool-surface/spec.md`「契约 pin 后才可开工」）。

- 冻结日期：**2026-11-15**
- 上游：`github.com/ewhauser/gbash`（Go）
- 状态：**已冻结（pin 与 §3 依赖面可签收），带 3 条提请 Owner 裁决项**（见 §10）。这 3 条不改变
  pin 的选择，但直接决定 `keel.shell` / `keel.jobs` 的实现形态：**gbash 侧不承担作业托管**，
  全部由 keel JobManager 承担。

引用约定：外部 URL 直接给出；本地源码路径相对 v0.0.41 检出（
`git clone --depth 1 --branch v0.0.41 https://github.com/ewhauser/gbash.git`）。
本文件中所有「实测」标注的结论均由一个针对 v0.0.41 的真实 Go 探针跑出（探针不随文档提交）。

---

## 1. Pin

| 项 | 值 |
|---|---|
| Go module path | `github.com/ewhauser/gbash` |
| **建议 pin 的 tag** | **`v0.0.41`** |
| tag commit（dereference 后） | `7cc3528b7139d063e1ebc0edb19540c44ed32d40` |
| tag 上 commit 的时间 | `2026-10-20T11:38:48-06:00` |
| release 发布时间 | `2026-10-20T17:48:26Z` |
| 获取日期 | `2026-11-15` |
| 默认分支 HEAD（参考，**不 pin**） | `64eb59678c116708f96c9172da15a0ed52dd8daf`（`2026-10-24`，仅 next.js 依赖安全升级） |
| 仓库创建时间 | `2026-03-10T16:47:05Z` |
| 最近 push | `2026-11-11T20:15:12Z` |
| Stars / Forks | **约 56 stars** / 2 forks（量级：小众早期项目） |
| 语言 | Go |
| 许可证 | `Apache-2.0`（GitHub API `license.spdx_id`） |
| 仓库描述（原文） | "Pure Go bash implementation for agent sandboxes" |
| 归档状态 | 未归档 |
| 基线构建要求 | `go 1.26.0`（`go.mod`）；本仓 `go 1.27.3` 满足 |

**上游状态原文短引**（`README.md:7-8`）：

> `> [!WARNING]`
> `> This is alpha software. It is likely that additional security hardening is needed. Use with care.`

以及 `README.md:410`（Security Model 末段）：

> "This is not a hardened sandbox. If you need stronger containment against denial-of-service or
> runtime bugs, use OS- or process-level isolation around it."

> **注意**：tag 号停在 `v0.0.x`，且 alpha 警告在 README 顶部（不是历史残留）。把它当
> 「**早期 alpha、会破 API、不承诺沙箱强度**」对待，而不是「准 1.0」。

来源：
- <https://github.com/ewhauser/gbash>
- <https://github.com/ewhauser/gbash/releases>（最新 release = `v0.0.41`，`v0.0.40` 同日发布）
- <https://github.com/ewhauser/gbash/blob/v0.0.41/go.mod>
- <https://github.com/ewhauser/gbash/blob/v0.0.41/README.md>

发布节奏（供升级策略参考，`CHANGELOG.md` + releases）：`v0.0.40`/`v0.0.41` 同日（2026-10-20）
连发，内容几乎全是 `fix` 类安全硬化（`builtins` 尺寸边界、`fs` 后端硬化、`syntax` 指数级解析、
`interp` 递归线性化、`server` 加固、`network` 密钥重加密）。**结论：`v0.0.x` 之间修的是安全
bug，不是加特性——不能「等它稳定再升」，必须主动跟安全 fix。**
- <https://github.com/ewhauser/gbash/blob/v0.0.41/CHANGELOG.md>

---

## 2. 许可证

- gbash 本体：**Apache-2.0**（`LICENSE` 为 Apache License 2.0 全文；`README.md:521` 声明
  "This project is licensed under the Apache License 2.0. Copyright 2026 Eric Hauser."）。
- **合规一句话**：Apache-2.0 允许闭源分发与商业使用；keel 以 Go module 依赖方式链接（非 vendored
  修改）时，义务是**保留上游版权与许可声明、附上 Apache-2.0 文本、标注对上游文件的任何修改**，
  无需开源 keel 自身代码。
- **本仓已满足该义务的形态**：作为普通 `require` 依赖引入，在 `THIRD_PARTY_NOTICES`（或等价文件，
  实现时定名）登记 `github.com/ewhauser/gbash v0.0.41 — Apache-2.0 — Copyright 2026 Eric Hauser`。
  **不得**把它 vendor 进本仓再声称是自有代码。
- **传递依赖许可需注意**：上游 `internal/shell` 是 `mvdan/sh`（`mvdan.cc/sh`）的**in-tree fork**，
  该部分为 **BSD-3-Clause**（`internal/shell/UPSTREAM_LICENSE`：`Copyright (c) 2016, Daniel Martí`，
  含 "Neither the name" 条款）。BSD-3 的「不得用作者名背书」条款会随二进制分发传递，登记时必须
  一并列出。**注意**：上游**没有** `NOTICE` 文件（已确认 `ls NOTICE` 不存在），因此不引入
  Apache-2.0 §4(d) 的 NOTICE 传递义务。
- 上游的 `go.mod` 直接依赖仅 `golang.org/x/{crypto,term,text}` 与 `golang.org/x/sys`（+ 测试用
  `go-quicktest/qt`、`google/go-cmp`），依赖面收敛，供应链风险低。

来源：
- <https://github.com/ewhauser/gbash/blob/v0.0.41/LICENSE>
- <https://github.com/ewhauser/gbash/blob/v0.0.41/README.md>（License 段）
- <https://github.com/ewhauser/gbash/blob/v0.0.41/internal/shell/UPSTREAM_LICENSE>
- <https://github.com/ewhauser/gbash/blob/v0.0.41/go.mod>

---

## 3. API 边界（冻结依赖面）

**这是 keel 被允许依赖的全部上游 surface。** 不在此清单内的上游符号，**不构成 keel 的兼容
承诺**；升级时它们可以任意改名/删除而不算破坏契约。

### 3.1 允许依赖的类型与函数

**根包 `github.com/ewhauser/gbash`**

| 类别 | 精确符号 |
|---|---|
| 构造 | `gbash.New(opts ...Option) (*Runtime, error)` |
| 一次性执行 | `(*Runtime).Run(ctx, *ExecutionRequest) (*ExecutionResult, error)` |
| 会话 | `(*Runtime).NewSession(ctx) (*Session, error)` |
| 会话执行 | `(*Session).Exec(ctx, *ExecutionRequest) (*ExecutionResult, error)` |
| 会话文件系统逃生口 | `(*Session).FileSystem() gbfs.FileSystem` |
| 生效限额查询 | `(*Session).Limits() policy.Limits` |
| 请求 | `gbash.ExecutionRequest`（字段：`Script`、`ScriptPath`、`Args`、`Env`、`WorkDir`、`Timeout`、`ReplaceEnv`、`Stdin`、`Stdout`、`Stderr`、`SpillDir`、`MaxSpillBytes`、`Name`、`ShellVariant`） |
| 结果 | `gbash.ExecutionResult`（字段：`ExitCode`、`ShellExited`、`Stdout`、`Stderr`、`ControlStderr`、`FinalEnv`、`StartedAt`、`FinishedAt`、`Duration`、`Events`、`StdoutTruncated`、`StderrTruncated`、`TimedOut`、`StdoutSpillPath`、`StderrSpillPath`、`StdoutSpillTruncated`、`StderrSpillTruncated`） |
| 选项 | `WithRegistry`、`WithFileSystem`、`WithWorkingDir`、`WithPolicy`、`WithLimitOverrides`、`WithBaseEnv`、`WithHost`、`WithTracing`、`WithLogger`、`WithNetwork`、`WithHTTPAccess`、`WithNetworkClient` |
| 注册表入口 | `gbash.DefaultRegistry() *commands.Registry` |
| 文件系统配置 | `InMemoryFileSystem()`、`SeededInMemoryFileSystem(gbfs.InitialFiles)`、`CustomFileSystem(gbfs.Factory, workingDir string)`、`MountableFileSystem(MountableFileSystemOptions)`、`HostDirectoryFileSystem`、`ReadWriteDirectoryFileSystem` |
| 结构体 | `FileSystemConfig{Factory, WorkingDir}`、`MountableFileSystemOptions{Base, Mounts, WorkingDir}` |
| 取消 | **标准 `context.Context`**（见 §6） |

**`github.com/ewhauser/gbash/commands`**（命令注入）

| 类别 | 精确符号 |
|---|---|
| 接口 | `commands.Command`：`Name() string`；`Run(ctx context.Context, inv *Invocation) error` |
| 适配器 | `commands.DefineCommand(name string, fn CommandFunc) Command`；`commands.CommandFunc` |
| 注册表接口 | `commands.CommandRegistry`：`Register(Command) error`、`RegisterLazy(name string, LazyCommandLoader) error`、`Lookup(name string) (Command, bool)`、`Names() []string` |
| 注册表实现 | `commands.Registry`、`commands.NewRegistry(cmds ...Command) *Registry`、`commands.LazyCommandLoader` |
| 调用上下文 | `commands.Invocation`（字段：`Args`、`Env`、`Cwd`、`Stdin`、`Stdout`、`Stderr`、`FS *CommandFS`、`Fetch`、`Exec`、`Interact`、`Limits`、`GetRegisteredCommands`） |
| 文件系统视图 | `commands.CommandFS`：`Open`、`OpenFile`、`ReadFile`、`Stat`、`StatQuiet`、`Lstat`、`ReadDir`、`Readlink`、`Realpath`、`Symlink`、`Link`、`Chown`、`Chmod`、`Chtimes`、`MkdirAll`、`Remove`、`Rename`、`Getwd`、`Chdir`、`Resolve`、`ResolveAt` |
| 读辅助 | `commands.ReadAll(ctx, inv, reader)`、`commands.ReadAllStdin(ctx, inv)` |
| 退出/诊断 | `commands.ExitError{Code, Err}`、`commands.ExitCode(err) (int, bool)`、`commands.Exitf(inv, code, format, ...)`、`commands.Diagnosticf`、`commands.DiagnosticMessage` |

**`github.com/ewhauser/gbash/fs`**（文件系统注入）

| 类别 | 精确符号 |
|---|---|
| 接口 | `fs.FileSystem`（17 个方法，见 §4.2）、`fs.Factory`：`New(ctx) (FileSystem, error)` |
| 适配器 | `fs.FactoryFunc` |
| 工厂 | `fs.Memory()`、`fs.SeededMemory(InitialFiles)`、`fs.Overlay(lower Factory)`、`fs.Snapshot(source FileSystem)`、`fs.Reusable(source Factory)`、`fs.Mountable(MountableOptions)`、`fs.Host(HostOptions)` |
| 挂载 | `fs.MountConfig{MountPoint, Factory}`、`fs.MountableOptions{Base, Mounts}`、`fs.NewMountable(base FileSystem) *MountableFS`、`(*MountableFS).Mount/Unmount/Mounts` |
| 种子 | `fs.InitialFiles`、`fs.InitialFile`、`fs.LazyFileProvider` |
| 具体实现（**仅用于测试替身**） | `fs.NewMemory() *MemoryFS` |
| 路径工具 | `fs.Clean`、`fs.Resolve`、`fs.ResolvePhysical` |

**`github.com/ewhauser/gbash/policy`**（预算与访问控制）

| 类别 | 精确符号 |
|---|---|
| 接口 | `policy.Policy`：`AllowCommand`、`AllowBuiltin`、`AllowPath`、`Limits()`、`SymlinkMode()` |
| 实现 | `policy.Static`、`policy.NewStatic(*Config)`、`policy.Config{AllowedCommands, AllowedBuiltins, ReadRoots, WriteRoots, Limits, SymlinkMode}` |
| 限额 | `policy.Limits{MaxCommandCount, MaxGlobOperations, MaxBraceExpansionWords, MaxLoopIterations, MaxSubstitutionDepth, MaxFunctionDepth, MaxStdoutBytes, MaxStderrBytes, MaxFileBytes, MaxVariableBytes, MaxTotalVariableBytes}`、`policy.Unlimited` |
| 错误 | `policy.DeniedError{Subject, Reason}` |

**`github.com/ewhauser/gbash/host`**（仅当需要真实进程元数据时）

| 类别 | 精确符号 |
|---|---|
| 接口 | `host.Adapter`：`Defaults(ctx)`、`Platform()`、`ExecutionMeta(ctx)`、`NewPipe()` |
| 实现 | `host.NewSystem()`；`host.ExecutionMeta{PID, PPID, ProcessGroup}` |

**`github.com/ewhauser/gbash/trace`**（仅观测）

`trace.Event`、`trace.Kind` 常量（`EventCallExpanded`、`EventCommandStart`、`EventCommandExit`、
`EventFileAccess`、`EventFileMutation`、`EventPolicyDenied`）、`trace.SchemaVersion = "gbash.trace.v1"`。

### 3.2 明确**不**依赖的上游面

1. **`github.com/ewhauser/gbash/internal/...` 全部**（`internal/runtime`、`internal/shell`、
   `internal/builtins`、`internal/shellstate`）。Go 的 internal 规则已阻止跨模块导入；这里额外声明：
   **keel 不得用 `replace`/`vendor`/`copy` 绕过**。需要 internal 能力（作业控制、进程组、
   信号派发）就自己在 keel 侧实现，或走本文件 §4 的注入面。
2. **`github.com/ewhauser/gbash/cli`**（`cli.Run`/`cli.Config`）：那是「起一个 gbash 二进制」的路
   径；keel 是进程内嵌，**不用**。同理不用 `gbash/server`（JSON-RPC + Unix socket，README 明确
   "negotiation is not configured over the wire" 且无鉴权）。
3. **`contrib/*`**（独立 module：`awk`/`jq`/`yq`/`sqlite3`/`python`/`nodejs`/`bashtool`/`codingtools`）：
   默认不注册；keel 若需要 `jq`/`yq` 等，**在 `keel.shell` 里显式决策**，不在本契约内承诺。
4. **`packages/gbash-wasm` / `@ewhauser/gbash-wasm`**：只记录（见 §8 备注），keel 不做浏览器执行面。
5. **具体后端类型断言**：上游自己就说 `Session.FileSystem()` "only promises the `gbfs.FileSystem`
   interface. Do not rely on concrete backend types or type assertions"（
   `website/content/configuration/filesystem.mdx:129`）。同理 `fs.*FS` 具体类型不得出现在 keel
   的非测试代码里。

### 3.3 兼容承诺的边界

> **本清单之外的上游 API 不构成 keel 的兼容承诺。**
>
> keel 的 `keel.shell`（gbash 适配层）是**唯一**允许 `import "github.com/ewhauser/gbash/..."`
> 的包。仓内其余代码只依赖 `keel.shell` 暴露的 keel 自有类型。这样上游升 breaking 时，改动面
> 被限制在一个包内（§7 的薄适配层策略）。

**`gbash` 是一等依赖而非稳定 ABI**：`v0.0.x` 语义 = 随时可破 API。keel 不承诺「上游小版本自动
兼容」，只承诺「本文件重 pin + conformance 快照通过」。

---

## 4. 注入面（keel → gbash）

三条注入路径，全部有实测结论。

### 4.1 自定义命令注册

**机制**：`gbash.DefaultRegistry()` 拿默认注册表 → `Register(...)` / `RegisterLazy(...)` → `gbash.New(gbash.WithRegistry(reg))`。

**关键实证**：`Registry.Register` **覆盖同名 shell builtin**。实测（v0.0.41）：

```
注册 jobs/nohup/ps/kill/timeout/echo 后再执行：
  jobs; nohup x; ps; kill -TERM 1; timeout 1 true; echo plain
  → KEEL[jobs] args=[] cwd=/home/agent
    KEEL[nohup] args=[x] cwd=/home/agent
    KEEL[ps] args=[] cwd=/home/agent
    KEEL[kill] args=[-TERM 1] cwd=/home/agent
    KEEL[timeout] args=[1 true] cwd=/home/agent
    KEEL[echo] args=[plain] cwd=/home/agent
```

```go
// 覆盖语义的源码依据（不依赖实测，可复核）：
// commands/registry.go:46-48
//   "// Register stores cmd by name, replacing any existing command with the same name."
// 分派顺序：registry 命中优先于 builtin 分派（internal/shell/command_dispatch.go）
```

**（a）`mcp` CLI 子命令（shim）** — ✅ 可注入

```go
registry := gbash.DefaultRegistry()
registry.Register(commands.DefineCommand("mcp", func(ctx context.Context, inv *commands.Invocation) error {
    // keel.mcp 的 registry 是唯一事实源；这里只做 argv → action 的翻译，不复制实现
    switch invoke := firstArg(inv.Args); invoke {
    case "list", "describe", "call":
        return keelMCPHost.Dispatch(ctx, invoke, inv.Args[1:], inv.Stdout, inv.Stderr)
    default:
        return commands.Exitf(inv, 2, "mcp: unknown subcommand %q", invoke)
    }
}))
```

约束（来自 `tool-surface/spec.md`「MCP 只经聚合工具与 CLI 暴露」）：
- 与聚合工具**共用同一个 registry**；`mcp` 命令是 shim，不持有自己的 server 表。
- 二进制内容 **不得**经 stdout 传输，须走 keel content block。因此 `mcp` 命令的
  `inv.Stdout` 只允许文本结果；二进制走「落 layerfs + 打印引用」。
- **`mcp` 不在 gbash 默认注册表里**（全仓 `grep -i mcp` 只命中 `examples/bench-fs/main.go` 的
  `NumCPU` 误匹配），所以 `mcp` 是 keel 纯新增命令，无覆盖冲突。

**（b）作业控制命令（`nohup` / `jobs` / `ps` 视图）** — ⚠️ 可注入，但**语义必须是全新实现**

**实测事实（v0.0.41）**：

| 名称 | `command -v` | `type` | 直接执行 |
|---|---|---|---|
| `jobs` | 命中（`jobs`） | "jobs is a shell builtin" | **`jobs: command not found`，exit 127** |
| `fg` / `bg` / `disown` | 命中（同上，builtin 命名表登记） | shell builtin | **exit 127** |
| `nohup` | 未命中 | — | **`nohup: command not found`，exit 127** |
| `ps` | 未命中 | — | **`ps: command not found`，exit 127** |
| `kill` | 未命中 | — | **可用**（真实实现，走 shellstate 信号派发） |
| `wait` | 未命中 | — | **可用**（真实实现） |
| `timeout` | 未命中 | — | **可用**（作为普通注册命令存在） |

即：`jobs`/`fg`/`bg`/`disown` 只出现在 **`help` 帮助主题表**
（`internal/builtins/help.go:141-165`）与 **interp builtin 名称表**
（`internal/shell/interp/builtin.go` 的 `runtimeBuiltinNames`）里，**没有实现体**
（`internal/builtins/` 下无 `jobs.go`/`fg.go`/`nohup.go`/`ps.go`）。这是一个**会骗人**的表面：
`command -v jobs` 说存在，执行返回 127。

**结论**：keel 的作业视图必须**整段自研**为 gbash 命令，不能依赖上游任何作业语义：

- `nohup`、`ps`：纯新增命令，无冲突。
- `jobs`：必须用 `Register` **覆盖**（否则脚本里 `jobs` 会 127；覆盖后行为由 keel 定义）。
- `fg`/`bg`：**不要注册**。gbash 的 SPEC 明确 job control 是非目标（见 §5），注册成「假前台
  切换」只会制造错误期望。若模型调用，让它 127 并给 `unknown` 语义。
- 这些命令的**数据源**是 keel JobManager（Postgres 表），不是 gbash 进程内状态——见 §5。

**（c）命令实现的硬约束**（来自 `Invocation` 契约）

- 文件访问**必须**经 `inv.FS`（`*commands.CommandFS`，自带 policy 检查与 trace），
  **禁止**用 `os.Open`/`os.ReadFile` 直接碰宿主。
- 网络**必须**经 `inv.Fetch`；子执行**必须**经 `inv.Exec`/`inv.Interact`（这两个回调在
  `Session.Exec` 路径上是**重入**的：`internal/runtime/session.go:384` 的 `subexecCallback` →
  `s.exec`，`isReentrantSessionCall` 跳过 `mu` 锁，所以嵌套执行不死锁）。
- `inv.FS.Chdir` **只改该次调用的视图**，不改 shell 的 `PWD`（实测：`cdx; pwd` → 命令内
  `after=/tmp`，之后 shell `pwd` 仍是 `/home/agent`）。要改 shell cwd，得走 gbash 的 `cd`
  builtin，或由 keel 在脚本层前置 `cd`。

### 4.2 自定义文件系统后端（layerfs 写穿适配）

**接口**（`fs/fs.go:18-36`）——**这是唯一必须实现的 17 个方法**：

```go
type FileSystem interface {
	Open(ctx context.Context, name string) (File, error)
	OpenFile(ctx context.Context, name string, flag int, perm stdfs.FileMode) (File, error)
	Stat(ctx context.Context, name string) (stdfs.FileInfo, error)
	Lstat(ctx context.Context, name string) (stdfs.FileInfo, error)
	ReadDir(ctx context.Context, name string) ([]stdfs.DirEntry, error)
	Readlink(ctx context.Context, name string) (string, error)
	Realpath(ctx context.Context, name string) (string, error)
	Symlink(ctx context.Context, target, linkName string) error
	Link(ctx context.Context, oldName, newName string) error
	Chown(ctx context.Context, name string, uid, gid uint32, follow bool) error
	Chmod(ctx context.Context, name string, mode stdfs.FileMode) error
	Chtimes(ctx context.Context, name string, atime, mtime time.Time) error
	MkdirAll(ctx context.Context, name string, perm stdfs.FileMode) error
	Remove(ctx context.Context, name string, recursive bool) error
	Rename(ctx context.Context, oldName, newName string) error
	Getwd() string
	Chdir(name string) error
}

type File interface { io.ReadWriteCloser; Stat() (stdfs.FileInfo, error) }
type Factory interface { New(ctx context.Context) (FileSystem, error) }
```

安装方式：`gbash.WithFileSystem(gbash.CustomFileSystem(myFactory, workingDir))`
（`api.go:368` 签名：`CustomFileSystem(factory gbfs.Factory, workingDir string) FileSystemConfig`）。

**「write 后 bash 立即可读」如何成立——实测两条链路都通**：

1. **keel 写 → bash 读**：在**会话已存在**时，直接对注入的同一后端写文件，随后
   `Session.Exec("cat ...")` 立即读到新内容。
   实测：后端写 `v1` → bash `cat` → `v1`；后端改 `v2` → bash `cat` → `v2`（`W live-edit`）。
   无需重新 materialize、无需重建 session。
2. **bash 写 → keel 读**：`Session.Exec("echo from-bash > /path")` 之后，直接对注入后端的
   `Open("/path")` 读到 `from-bash`（`W2`）。

原因：`Session.fs` 就是注入的实例本身（`internal/runtime/runtime.go:199`：
`fsys, err := r.sessionFactory.New(ctx)`，随后 `s.fs = fsys`），`Session.FileSystem()` 也返回它
（`session.go:394`）。**适配层因此是零拷贝、同进程、同一对象**——这就是 keel 要求「写穿」的机制。
没有缓存层需要失效。

**根路径与挂载策略（强制）**：

**实测反例（必须避免）**：把 layerfs 后端直接作为 `CustomFileSystem` 的根工厂，gbash 会向它写入
**258 个虚拟命令桩**（`/bin/*`、`/usr/bin/*`，内容为
`"# gbash virtual command stub: <name>\n"`），外加 `mkdir /bin /usr/bin /home/agent /tmp` 与
`chmod /tmp`。总计 **263 次写**（`P1`）。这会把 keel 的内容寻址存储污染成一堆 `0755` 的假
可执行文件，并在每次 `Session.Exec` 的 layout 校验里被反复 `Stat`。

**推荐形态：`MountableFileSystem`，把 layerfs 挂在业务路径，把 gbash 的沙箱布局隔离到内存挂载**

```go
gbash.New(
    gbash.WithFileSystem(gbash.MountableFileSystem(gbash.MountableFileSystemOptions{
        Base:  gbfs.Memory(),                       // 沙箱根：/bin /usr/bin /tmp /home/agent
        Mounts: []gbfs.MountConfig{
            {MountPoint: "/workspace", Factory: layerfsFactory}, // ← layerfs 只挂这里
        },
        WorkingDir: "/workspace",
    })),
)
```

实测效果（`M1`–`M4`）：**Base 与 workspace 挂载的后端收到的布局写入 = 0**。因为
`initializeSandboxLayout` 的 `MkdirAll`/命令桩全部落在 `Base`（内存），而 `/bin`、`/usr/bin`、
`/tmp` 各自显式挂内存 FS（`fs/mountable.go:505-535` 的 `route` 按最长前缀路由）。layerfs 只在
`/workspace` 前缀内收到真实业务 I/O。随后「keel 写 `/notes.txt` → bash `cat /workspace/notes.txt`」
读到 `tool-wrote-this`（`M4`）。

**必须写进实现的三条**：

1. **layerfs 必须挂在非根挂载点**（不能是 `/`）。`fs/mountable.go:117`（`Mount`）与
   `fs/mountable.go:632`（`validateMountPath`）两处都明确 `cannot mount at root "/"`。
2. **`/bin`、`/usr/bin`、`/tmp` 显式挂内存**。只挂 `/bin`+`/usr/bin` 会让 `chmod /tmp`（sticky
   位）落回 Base；Base 是内存时无害，但显式挂载让「哪条路径属于谁」不依赖上游实现细节。
3. **`fs.Factory.New` 每个 session 调一次**。若要让多个 keel 会话（子代理）共享同一份 layerfs，
   工厂必须返回**同一实例**或同一底层存储的视图——实测这是安全的：两个 session 共用一个后端时
   并发 `Exec` 互不阻塞（各自 500ms，`S1`=真并行）、跨 session 的写入立即可见（`S2`）、
   并发 append 同一文件得到 2 行（`S3`）。**但** `Session.Exec` 在**单 session 内是串行的**
   （见 §5.3）。

**不要用的后端**：`HostDirectoryFileSystem` / `WithWorkspace`（宿主只读 + 内存 overlay，写不落
layerfs）、`ReadWriteDirectoryFileSystem`（**直接写宿主**，上游威胁模型把它列为高危选项：
"Whether production embeddings ever use `ReadWriteDirectoryFileSystem(...)` outside short-lived temp
roots"）。keel 的两个后端都不得使用。

### 4.3 host 二进制「不落穿」的验证方式

**要求**：命令永不落到宿主二进制，网络默认关。

**机制（上游承诺 + 实测）**：

- 注册表驱动：未注册命令 → `internal/shell/command_dispatch.go:66`
  `"%s: command not found"`，exit **127**。不存在「找不到就 exec 宿主」的分支。
- 全仓非测试代码**无 `os/exec`、无 `syscall.ForkExec`、无 `os.StartProcess`**（已 grep 确认；
  唯一的 `os/exec` 出现在 `cmd/gbash-gnu/`、`internal/conformance/`、`internal/testutil/`、
  `contrib/awk/goawk/interp/`——都不是运行时路径）。
- 网络：默认关。`curl` **不进默认注册表**，只在 `WithHTTPAccess`/`WithNetwork`/自定义
  `NetworkClient` 时由 `internal/builtins.EnsureNetworkCommands` 注册。

**实测（两条 host 变体各跑一遍）**：

```
[default-virtual-host] curl=absent wget=absent nc=absent ssh=absent git=absent python3=absent node=absent
                       cat /etc/passwd → "No such file or directory"
[system-host]          同样的结果
```

即：**即使换 `host.NewSystem()`（真实进程/OS 视图），宿主二进制也不可达**——`host` 适配器只影响
`uname` 等平台元数据与 PID/进程组种子，不影响命令解析。

**keel 的验证方式（写成 conformance 快照，不是一次性人工检查）**：

| 断言 | 方法 | 期望 |
|---|---|---|
| 宿主二进制不可达 | 脚本 `command -v git python3 node docker kubectl` 逐个检查 | 全部 absent |
| 宿主路径不可读 | `cat /etc/passwd`、`ls /Users`、`cat ~/.ssh/id_rsa` | 全部 `No such file or directory` |
| 网络默认关 | `curl -sS https://example.com` | `curl: command not found`（127） |
| 未注册命令 | `definitely-not-a-command` | 127 + `command not found` |
| `PATH` 不指向宿主 | `echo $PATH` | `/usr/bin:/bin`（虚拟桩路径） |
| 无宿主进程派生 | 构建期断言：keel 的 gbash 适配层包不得 `import "os/exec"`；`go list -deps` 中不出现宿主 exec 路径 | 编译期失败 |
| `host` 适配器选择 | 生产用默认虚拟 host（`PID=1`，不泄漏宿主 PID） | `echo $$` → `1` |

**说明 `exec /bin/sh -c 'echo host-shell'` 会成功**——那是 gbash **自己**的 `sh` 实现（嵌套
同沙箱执行，`internal/builtins/bash.go` 走 `inv.Exec` 回调），不是宿主 `/bin/sh`。验证时不要把它
误判为「落穿」。

---

## 5. 进程与作业模型

### 5.1 gbash 侧的进程模型：**没有进程，只有 goroutine**

- 全部执行发生在 keel 进程内（in-process），gbash 不 fork 宿主进程（§4.3 已验证）。
- 默认虚拟 host：`$$`=**1**、`PPID`=**0**、`ProcessGroup`=**1**（`internal/runtime/host.go:161-168`，
  注释："The virtual shell is PID 1 and leads its own group, so no host process-group id leaks into
  the sandbox"）。**所以 sandbox 内的 PID 与 keel 的 OS PID 是两套命名空间，不能互相对应。**

### 5.2 gbash 的「作业」语义：**execution-scoped，出 Exec 即死**

上游 SPEC 明确（`SPEC.md:681`、`SPEC.md:724`）：

> "job control remains unsupported"
>
> "background jobs do not outlive the execution: when the top-level run ends they are canceled, the
> shell side of every process-substitution pipe is closed, and all jobs and process substitutions
> are joined before the call returns"

实现上：`Runner.StopJobs`（`internal/shell/interp/jobs.go:66`）在 `internal/shell/core.go:183`
`defer` 执行 → `jobs.cancel()` + `wg.Wait()`。背景任务上限 256/execution
（`maxLiveBackgroundJobs`，超限报 `fork: background job limit reached (256)` + exit 1）。

**实测（`P4`）**：`( sleep 0.7; echo alive > /tmp/alive.txt ) & echo launched` 返回后等 1.3s，
再 `cat /tmp/alive.txt` → **`MISSING`**。后台任务在 `Exec` 返回前就被 cancel+join 了。

**`$!` 也不是真 PID**（实测 `bg pid` → `pid=g1`）：`internal/shell/interp/vars.go:1275-1277`
把 `$!` 展开成 `"g" + len(bgProcs)`，即**虚拟作业标签**。

### 5.3 单 session 内 `Exec` 是**串行**的

`internal/runtime/session.go:31-38`：`Session.Exec` 首先 `s.mu.Lock()`。实测（`P5`）：同一 session
上两个并发 `Exec("sleep 0.4")` → 分别 **800ms / 400ms**，完全串行。

**这直接杀死一个看似可行的设计**：「超时后让原 `Exec` 继续跑、同时给模型返回」——不行，因为原
`Exec` 持锁期间该 session 的任何新 `Exec` 都阻塞。

### 5.4 keel 的作业模型（gbash 侧只提供原语）

**边界划分（冻结）**：

| 能力 | 归属 | gbash 提供什么 |
|---|---|---|
| 作业登记、上限、查询、输出引用、退出状态 | **keel JobManager**（Postgres 表） | 无 |
| 作业的跨 `Exec` 存活 | **keel** | 无（出 Exec 即 kill，§5.2） |
| 后台执行的实际载体 | **keel**（独立 goroutine + 独立 gbash `Session`） | `Session` + `context` |
| `nohup`/`jobs`/`ps` 的**视图** | **keel**（自研命令，读 JobManager） | `Register` 覆盖命名（§4.1b） |
| `kill` | gbash（真实实现，但只作用于沙箱内虚拟 PID） | `kill` 命令 |
| 重启收养 | **keel** | 无 |

**「kid 转后台 / 收养由 keel JobManager 承担」的具体形态**：

- 一个 keel 作业 = **一个独立的 gbash `Session`**（不是同一 session 里的 `&`）。因为：
  (a) 同 session 串行（§5.3），作业会堵住交互；
  (b) 后台任务出 Exec 即死（§5.2）。
- 作业 goroutine 用自己的 `context.WithCancel` + 自己的 `Session`，输出经
  `ExecutionRequest.Stdout/Stderr` **流式**写入 layerfs（不是攒在内存里，§6.3）。
- 作业的「pid」是 keel 自己的作业 id（`job_<uuid>` 之类），**不是** gbash 的 `$!` 或 `$$`。
  `ps` 视图展示 keel 作业表；`$$` 在沙箱内永远是 1，不要暴露给模型当作业 id。

**重启收养的可行性结论：❌ 在 gbash 侧不可行，且不必要**

- gbash 侧没有可收养的对象：作业状态在 goroutine 与 `Runner.jobs`（内存）里，进程退出即全丢。
  `server` 模式的 session 也只是内存对象 + TTL（`server/server.go:25` 默认 30min），
  SPEC 把 "restart-persistent sessions" 列为**非目标**（`SPEC.md:281` 附近）。
- **keel 侧可行为什么成立**：作业的**持久化事实**（id、命令、cwd、环境、创建时间、输出 blob 引用、
  退出码）在 Postgres 里。keel 重启后：
  - 若作业已死（进程不在了）：从 blob 引用恢复输出，把状态定为 `interrupted`/`exited`，
    **不需要「重新登记一个活的作业」**。
  - 若作业仍活着：**不可能**——它是 keel 进程内的 goroutine，进程死则 goroutine 死。
- **因此 `tool-surface` spec 的「重启后收养」场景必须按「恢复事实 + 判定终态」实现，不能按
  「重新挂上正在运行的进程」实现。** 这是一条对 spec 的**实现级收窄**，需 Owner 知情：
  真正的跨重启存活要么把作业改成宿主进程（违背 in-process 原则），要么接受「重启即中断」。
  **建议判据**：作业状态在 keel 重启后一律收敛为终态（`exited`/`interrupted`），UI 显示
  "interrupted by harness restart"，输出保留（blob 引用在，可读）。

---

## 6. 超时与取消

### 6.1 上游的 `Timeout` 语义：**超时 = 取消 + 杀，不是转后台**

`ExecutionRequest.Timeout`（`execution_types.go:28`）→
`executionContext` 用 `context.WithTimeout`（`internal/runtime/session.go:433-438`）→
超时后 `classifyExecutionControlError`（`session.go:546-571`）把结果归一化成：

| 触发 | `ExitCode` | `TimedOut` | `ControlStderr` | `err` 返回 |
|---|---|---|---|---|
| `Timeout` 到期 / ctx `DeadlineExceeded` | **124** | **true** | `execution timed out after <d>` | **nil**（已处理，不返回 err） |
| ctx `Canceled` | **130** | **false** | `execution canceled` | **nil** |

实测（`B1`）：`sleep 3; echo SHOULD-NOT-PRINT` + `Timeout=200ms` → **200ms 返回**，
`exit=124`、`timedOut=true`、`stdout=""`。3.2s 后该 session 仍可用（`B2`："probe-done"），但
**`SHOULD-NOT-PRINT` 永远没打印** —— 工作被 cancel 了，**没有被转到后台**。

**结论：gbash 的 `Timeout` 与 keel 的 `timeout` 参数语义不兼容。**
`tool-surface/spec.md`：「`timeout` MUST 是工具参数……超时 MUST NOT 直接 kill 命令，而是转入后台并
返回超时信息、终端信息与后台 pid」。**`ExecutionRequest.Timeout` 字段因此禁止使用**。

### 6.2 keel 的 `timeout` 参数实现形态（冻结）

```
bash { action:"run", command, cwd, timeout, env }

1. 若 timeout == 0（未给）：走普通 Exec(ctx)，同步等结束。
2. 若 timeout  > 0：
   a. 为这次执行**单独开一个 gbash Session**（作业载体），在后台 goroutine 里跑
      Exec(ctxBackground, ...)，**不设 ExecutionRequest.Timeout**（保持 0）。
   b. keel 自己起 time.After(timeout)：到期时**不 cancel** ctxBackground，
      只把「已流式写入 layerfs 的输出」+ head/tail 摘要 + 作业 id 返回给模型。
   c. 返回：`{timedOut:true, jobId, pid(keel job id), 当前 head/tail, blob 引用, 退出码:未定}`
   d. 作业继续跑；`bash {action:"jobs", op:"status"}` 查；`op:"kill"` 时**才** cancel ctxBackground。
3. 用户取消 run（模型面 cancel / 会话删除）：cancel ctxBackground → 130 语义，作业收敛为 canceled。
```

为什么必须这样：

- **不能用 `ExecutionRequest.Timeout`**：它 cancel（§6.1），而 keel 要「转后台继续跑」。
- **不能靠 `&`**：出 Exec 即死（§5.2）。
- **不能在同 session 里留着跑**：串行锁（§5.3）会堵死交互。
- **只能用「独立 Session + keel 自己的 deadline」**。实测（`A1`–`A3`）：用 goroutine + `Stdout`
  流式写，模型侧 600ms 时已看到 `tick1 tick2 tick3`，而作业最终跑到 `FINISHED`——**「先返回、
  后台继续」在 gbash 上成立**，前提是作业有自己的 Session。

**取消传播**：keel 的 ctx 必须一路传到 `Session.Exec`（`context.Context` 是上游唯一取消通道）；
`ctx` 取消 → 130 + `ControlStderr="execution canceled"` + err=nil。**注意**：ctx 取消后
`Session` **仍可复用**（内部 `executionContext` 只包装本次调用，不动 session 状态）。

### 6.3 输出有界与流式

- **流式**：`ExecutionRequest.Stdout/Stderr` 是 `io.Writer`，实现是
  `capturePassthroughWriter{Write: capture→passthrough}`（`internal/runtime/passthrough_writer.go:42-47`）
  ——**先写 captureBuffer（有界），再写你的 writer**。实测（`A1`）：`Stdout: &buf` 能实时看到
  `tick1/tick2/...`，不是等结束。
- **有界**：`MaxStdoutBytes`/`MaxStderrBytes` 默认 **1 MiB**（`internal/runtime/runtime.go:113-137`）；
  超限后 captureBuffer 只保留前 `limit` 字节并置 `StdoutTruncated=true`，**但 passthrough 仍收到
  全量**（实测：`captured=64B, streamed=711B`，`truncated=true`）。所以：
  **keel 的流式 writer 是唯一能拿到全量输出的地方**；`result.Stdout` 只是有界摘要。
- **spill**：`SpillDir` + `MaxSpillBytes` 把溢出写**宿主**临时文件，路径在
  `StdoutSpillPath`（实测 `C1`：`/tmp/gbash-spill-probe/gbash-output-4051681798`，`truncated=true`，
  `spillTruncated=false`）。**keel 不用 spill**（那是宿主文件，绕过 layerfs）；keel 用自己的
  streaming writer 直接落 layerfs blob。
- **内存档位**：默认 policy 上限（`internal/runtime/runtime.go:113-137`）：
  `MaxCommandCount=10000`、`MaxLoopIterations=10000`、`MaxGlobOperations=100000`、
  `MaxBraceExpansionWords=100000`、`MaxSubstitutionDepth=50`、`MaxFunctionDepth=1000`、
  `MaxStdoutBytes=1<<20`、`MaxStderrBytes=1<<20`、`MaxFileBytes=8<<20`、
  `MaxVariableBytes=32<<20`、`MaxTotalVariableBytes=256<<20`。
  **`MaxStdoutBytes`/`MaxStderrBytes` 必须调小**（keel 只要 head/tail，1 MiB 驻留没必要）——
  但注意别调到 0（0 = 继承默认）。

---

## 7. 风险与升级策略

### 7.1 风险清单（按严重度）

| # | 风险 | 证据 | 缓解 |
|---|---|---|---|
| **R1** | **alpha + `v0.0.x`**：任意小版本可破 API，且上游把安全硬化当 patch 发 | README 顶部 WARNING；`CHANGELOG` v0.0.40/41 全是 `fix(harden)` | 薄适配层（§7.2）+ 重 pin 流程（§7.3）+ conformance 快照（§8） |
| **R2** | **不是硬化沙箱**，上游自己这么说 | README:410；`THREAT_MODEL.md` 明列 `WithWorkspace`/`ReadWriteDirectoryFileSystem`/`TraceRaw`/server 模式为改变安全姿态的选项 | keel **不用**这些选项；`TraceRaw` 禁用（只用 `TraceRedacted` 或关）；服务模式不用；多租户隔离由 keel 自身进程/容器边界承担，**不把 gbash 当安全边界** |
| **R3** | **作业控制是假的**：`jobs`/`fg`/`bg`/`disown` 有名字无实现（127），`nohup`/`ps` 不存在；`$!` 是 `g1` 不是 PID；background 出 Exec 即死 | §4.1b、§5.2 实测 + `SPEC.md:681,724` | 作业语义**全在 keel**；`jobs` 用 `Register` 覆盖；`fg`/`bg` 不注册 |
| **R4** | **`ExecutionRequest.Timeout` 语义与 keel 冲突**（kill vs 转后台） | §6.1 实测 | 禁用该字段；keel 自管 deadline（§6.2） |
| **R5** | **沙箱布局写入污染注入后端**（258 个 `/bin/*` 桩 + mkdir + chmod） | §4.2 实测 `P1`（263 次写） | 用 `MountableFileSystem`，layerfs 只挂在非根挂载点（§4.2，实测归零） |
| **R6** | **单 session `Exec` 串行**，一个长作业会堵死交互 | §5.3 实测（800ms/400ms） | 作业用独立 Session（§5.4） |
| **R7** | **重启不可收养**：gbash 无跨进程作业状态；spec 的「重新登记仍运行的作业」物理上不成立 | §5.4 | spec 收窄为「恢复事实 + 收敛终态」；需 Owner 确认 |
| **R8** | **`kill` 只作用于虚拟 PID**：`kill` 走 `shellstate.SignalDispatcherFromContext`，无 shell 上下文时报 `signaling is only available from shell contexts`；`SIGKILL`/`SIGSTOP` 被上游拒绝（`SPEC.md:727`） | `internal/builtins/kill.go:119-125`；`SPEC.md:727` | keel 的作业 kill 走 `context.CancelFunc`，**不靠** sandbox `kill`；若模型要 `kill`，映射到作业 cancel |
| **R9** | **大小写/`MaxFileBytes=8MiB` 对 layerfs 可能太紧**：大文件读会被截断 | `policy.Limits.MaxFileBytes` | 用 `WithLimitOverrides` 按 keel 需要调；但**不得**设 `Unlimited`（保留 DOS 防护） |
| **R10** | **`ShellVariant` 面**：`auto`/`bash`/`sh`/`mksh`/`zsh`/`bats` 六种；语义差异会漏到模型 | `shell_variant.go` | keel 固定 `ShellVariantBash`，不让模型选；写进 conformance |
| **R11** | **`TraceRaw` 会泄漏 secrets**：上游自己警告 raw tracing 不安全 | README:408 | 只用 `TraceRedacted` 或不开 trace；`OnEvent` 落 keel 账本时再脱敏一次 |
| **R12** | **WASM/`contrib` 生态诱惑**：`contrib/python`、`contrib/nodejs`、`gbash-wasm` 看着能扩展命令面 | `contrib/*/go.mod`；`packages/gbash-wasm` | 明确不在本契约内（§3.2）。加命令 = 走 keel 自己的 `Register` + 审批，不默默挂 contrib |

### 7.2 不稳定面的隔离策略：**薄适配层（`keel.shell`）**

**唯一导入点**：只有 `keel.shell`（及其子包 `keel.shell.gbash`）可以 `import "github.com/ewhauser/gbash/..."`。
其他包只见 keel 自有类型。落地约束：

1. **类型不出包**：`gbash.ExecutionResult`、`commands.Invocation`、`gbfs.FileSystem` 等**不得**
   出现在非 `keel.shell` 包的任何导出签名里。对外只暴露 keel 的 `ShellResult`/`JobHandle`/`FSView`。
2. **适配层自带语义**：`timeout→后台`、`作业 id`、`输出落 blob`、`env 白名单`、`cwd 语义`
   都在适配层实现；上游换了也不影响上层的工具面。
3. **单点收口**：上游 breaking 时，改动期望是「改 `keel.shell.gbash` 一个包 + 重跑 conformance」。
   若某次升级要改到别的包，说明契约泄漏了，先修泄漏。
4. **不 copy 上游代码**：不进 `internal/`、不 vendor、不 fork。需要 internal 能力（进程组、
   信号派发、作业表）→ 在 keel 侧写。

### 7.3 升级流程（重 pin 是一个有仪式感的动作）

```
1. 读上游 CHANGELOG + diff `api.go`/`execution_types.go`/`commands/`/`fs/fs.go`/`policy/`
   → 与本文件 §3.1 清单逐条对照；任何清单内符号的签名/语义变化 = 本文件必须改。
2. 在 keel 分支上把 `go.mod` 里的 gbash 版本改为目标版本（不自动 `go get -u`）。
3. 跑 conformance 快照（§8）。**任何一条从绿变红 = 升级阻断**。
4. 更新本文件的：§1 Pin 表（tag / commit / 日期 / stars / 上游状态原文）、§7.1 风险（新增项）、
   §3.1 清单（若符号有增减）。
5. PR 里必须带：旧→新 tag、conformance 结果、本文件 diff。
```

**升级节奏建议**：`v0.0.x` 的 patch 修的是**安全 bug**（§1），所以**不要停在原地**——每个
安全类 release 都应评估。但**不追随主干 HEAD**（`main` 随时可能不 green）；
只跟 tag，且 tag 之间差异一次性吞下。

**禁止的升级方式**：`go get github.com/ewhauser/gbash@main` / `@master`（无 tag 可 pin，
conformance 无基线）；`replace` 到本地 fork（绕过本契约）。

### 7.4 替代方案（备选记录，不采用）

**`github.com/mark3labs/go-bash`** — "Just Bash in Go"，MIT，约 14 stars，最新 tag `v0.1.0`
（2026-10-05），Go 1.25。README 自述 **beta**："The public API is stable, but byte-level output
of some builtins may shift as we close gaps against real bash."（相比之下 gbash 是 alpha，
go-bash 自称 API 更稳）。也有 registry 化自定义命令（`command.Define`）与可注入 `fs.FileSystem`。

**为什么不选它（一句话）**：gbash 的默认命令面（90+ / GNU coreutils 对齐）、内置 policy 与
execution budgets、结构化 trace、`MountableFileSystem` 多挂载、以及无宿主 `os/exec` 的显式承诺
更贴合「恒定工具面 + 输出有界 + 沙箱」的 keel 要求；且本仓已按 gbash 的三条注入路径（命令 /
文件系统 / 作业视图）验证过可行性，换库等于重做 §4 全部实证。
**保留为 mental fallback**：若 gbash 后续出现「必须 fork 才能用」的破坏（例如封闭 `fs.Factory`、
移除 `Register` 覆盖语义），go-bash 是第一个重评估对象——它的 `fs.FileSystem` 接口
（`io/fs` 嵌入 + `OpenFile`/`MkdirAll`/`Rename`/`Symlink`/`Realpath`/`AllPaths` 等）与 gbash 的
17 方法接口可机械对译，适配层可平移。

来源：
- <https://github.com/mark3labs/go-bash>
- <https://github.com/mark3labs/go-bash/blob/master/README.md>
- <https://github.com/mark3labs/go-bash/blob/master/fs/fs.go>

---

## 8. 待验清单（实现首日必须用 conformance 快照验证）

下列每一条都是**「文档/源码这么写，但 keel 的真实用法可能不同」**的点。它们不是「可以晚点再说」，
而是**首日就要用真实 keel 用例跑通**，否则 §4–§6 的结论可能在集成时翻车。

| # | 待验项 | 为什么可疑 | 建议断言 |
|---|---|---|---|
| **V1** | **流式输出接口的真实形状**：`Stdout` writer 是否在**每个 `Write` 调用**边界立即到达（还是被 `bufio` 包了一层） | 实测已看到 tick 实时到达（`A1`），但那是短命令；长命令 + 大块输出时行为未验 | 命令 `for i in $(seq 1 1000); do echo $i; sleep 0.01; done` → 流式 writer 收到的**首个字节时间** << 总时长；且**字节数**与 `seq 1 1000` 的总字节数一致（验证 passthrough 不受 `MaxStdoutBytes` 截断） |
| **V2** | **FS 注入的真实接口**：`Session.FileSystem()` 返回的到底是注入的实例，还是包了一层 `virtualDeviceFS` | 源码显示 `NewSession` 会 `wrapSandboxFileSystem(fsys)`（`internal/runtime/virtual_devices.go:36`）——`/dev/*` 被包住。**keel 拿到的不是裸 layerfs** | `Session.FileSystem()` 对 `/dev/null`、`/dev/urandom`、`/dev/full` 的读写走 gbash 虚拟设备，**不落** layerfs；其余路径落 layerfs。keel 的工具层要能区分 |
| **V3** | **退出码 / 信号语义**：`kill -TERM $$`、`kill -KILL $$` 的实际行为 | **实测异常**：两者都 **exit=0** 且继续执行后续命令（`X sig-term-self` / `X sig-kill-self` 都打印 "after"）。而 `SPEC.md:727` 说 `kill` 非 catchable 信号应 **fail with an error** | 明确：gbash 的 sandbox 内自杀信号**不终止脚本**。keel 的取消**不能**用脚本内 `kill` 表达，只能靠 ctx cancel；模型若写 `kill -9 $$` 得到的行为要在工具说明里写清 |
| **V4** | **`&` 后台在 `timeout→后台` 方案里的地位** | 实测出 Exec 即死（`P4`）。但如果 keel 用「独立 Session + 长 Exec」，模型脚本内部的 `&` 依然是 execution-scoped | 文档化：`bash {action:"run"}` 内的 `cmd &` **不构成** keel 作业；只有 **整条 `run` 超时**才产生作业。两者不要混 |
| **V5** | **`MountableFileSystem` 的 layerfs 挂载点在 `cwd` 语义下是否稳定** | `MountableFS.Chdir` 有 4 个分支（mounted/synthetic/default），`WorkingDir` 落在挂载点时 `resolve` 走 `namespacedFS` 包装 | keel 用 `WorkingDir` = layerfs 挂载点（如 `/workspace`）跑：`pwd`、`cd` 进出、`cd ..` 跨挂载边界、`realpath`；期望 cwd 稳定且不逃出挂载点 |
| **V6** | **`/dev/*` 与 layerfs 的持久性**：写 `/dev/shm`、`/tmp` 是否真的不落 layerfs | `/tmp` 若未显式挂内存，会落 Base（内存），**不落 layerfs**——但这是「恰好」还是「保证」需测 | 在 layerfs 挂 `/workspace` 的前提下，`echo x > /tmp/y; echo x > /dev/shm/z` → 断言 layerfs **零写入** |
| **V7** | **`CommandFS` 的 policy 边界**：`inv.FS` 是否真的挡住 `ReadRoots`/`WriteRoots` 外的路径 | `CommandFS` 每个方法都先 `prepare→check`（`commands/invocation_capabilities.go:125-200`），理论上挡得住；但 keel 的 policy 配置（`ReadRoots`/`WriteRoots`）与 layerfs 挂载点的**交集**未验 | 配 `WriteRoots: ["/workspace"]` → `inv.FS.OpenFile("/etc/x", O_CREATE)` 必须被拒（`policy.DeniedError`），且**不落** layerfs |
| **V8** | **`Register` 覆盖 builtin 的稳定性**：`echo`/`printf` 被覆盖后是否影响 shell 内建语义（重定向、`set -e`） | 实测 `KEEL[echo]` 覆盖成功且 `echo plain` 走 keel 实现——但这会**破坏 POSIX 语义**（builtin `echo` 有特殊展开规则） | **keel 不覆盖 `echo`/`printf`/`test`/`cd`/`pwd`/`export` 等 shell 内建**。只覆盖 `jobs`，新增 `nohup`/`ps`/`mcp`。写进 conformance 的黑名单 |
| **V9** | **`Source`/`.`/`eval` 是否绕过 registry** | `eval`/`source` 走同一 compile pipeline（`SPEC.md` 的 loop instrumentation 段），但**运行时动态命令名**是否仍查 registry | `eval "jobs"`、`comm=$(echo jobs); $comm` → 必须命中 keel 的 `jobs`，不能 127 也不能落到上游 |
| **V10** | **大文件读上限**：`MaxFileBytes=8MiB` 默认值 vs keel 的 `read` 工具 | `read` 是 keel 自己的工具（不走 gbash），但 bash 里 `cat bigfile` 会受 8MiB 限制 | 若 keel 允许 >8MiB 文件，必须 `WithLimitOverrides{MaxFileBytes: N}`；N 要有上界（不得 `Unlimited`） |
| **V11** | **`ExecutionResult.Events` 的可用性**：未开 trace 时是空切片；开了 `TraceRedacted` 才有 | 若 keel 想把 gbash 事件当账本事实，必须开 trace；但 trace 的 schema 是 `gbash.trace.v1`，**不属于本契约冻结面** | 决策点：keel 是**自己**在 `bash` 工具层记账（推荐），还是吃 `result.Events`。若吃，需把 `trace.Event` 加进 §3.1 清单并接受其 schema 漂移 |
| **V12** | **`bash` 工具与 gbash 的 `bash` 命令撞名** | gbash 有内建 `bash`/`sh`/`mksh`/`zsh`/`bats` 命令（嵌套同沙箱执行）。keel 的工具也叫 `bash`。模型写 `bash -c '...'` 时命中哪个？ | 实测：bash 命令可用（嵌套执行，非宿主）。keel **不要**覆盖它；工具名与 sandbox 命令名不同层，不冲突，但工具描述要写清 |

### 8.1 「未获取到」清单

| 项 | 原因 |
|---|---|
| 文档站页面的**线上渲染版**（`https://ewhauser.github.io/gbash/docs/guides/agent-integration/` 等） | `curl`/WebFetch 到 `ewhauser.github.io:443` 均 TLS 握手失败（`SSL_ERROR_SYSCALL` / `ECONNRESET`）。**改用同仓库的 MDX 源文件**（`website/content/**`，就是站点渲染的来源），URL 见下方抓取列表。结论无损，只是没读到线上 HTML |
| 文档站 `docs/api/server/` 之外的 server 细节 | 只读了 `website/content/api/server.mdx` 与 `server/*.go` 的 TTL/持久性相关片段；keel 不用 server 模式，未深挖 |
| `pkg.go.dev` 的 API 文档索引 | Go proxy 的 `@v/list` 与 `sum.golang.org` 在本环境不可达（`EOF`），无法核对 pkg.go.dev 的符号渲染。**改用 v0.0.41 源码直接读符号**（更权威，但少了 pkg.go.dev 的「导出 / 非导出」自动区分——已人工按 `^func |^type ` 提取大写导出符号） |
| 上游**精确的** `stars` 时间序列 / 使用方统计 | GitHub API 只给当前值（56）。未取 `stargazers` 历史（无关紧要） |
| `mark3labs/go-bash` 的**精确**命令面与 conformance 状态 | 只读了 README + `fs/fs.go` 片段；作为备选记录，未做完整调研（不必要） |
| `CHANGELOG` 里 `v0.0.1`–`v0.0.39` 的逐版本要点 | 只精读了 `v0.0.40`/`v0.0.41`（最近、最相关）。更早版本无升级价值（不跨越） |

---

## 9. 抓取来源（实际访问）

**一手仓库 / API**
- <https://github.com/ewhauser/gbash>
- <https://api.github.com/repos/ewhauser/gbash>（`gh api repos/ewhauser/gbash`：stars/license/created/pushed）
- <https://github.com/ewhauser/gbash/releases>（`gh api repos/ewhauser/gbash/releases`）
- <https://github.com/ewhauser/gbash/tags>（`gh api repos/ewhauser/gbash/tags`，最新 `v0.0.41`）
- `git clone --depth 1 --branch v0.0.41 https://github.com/ewhauser/gbash.git`（本文件全部源码结论的基线）

**v0.0.41 源码文件（本地检出路径）**
- `README.md`、`CHANGELOG.md`、`SPEC.md`、`THREAT_MODEL.md`、`SECURITY.md`、`LICENSE`、`go.mod`
- `api.go`、`options.go`、`execution_types.go`、`shell_variant.go`
- `commands/command.go`、`commands/registry.go`、`commands/execution.go`、`commands/invocation_capabilities.go`、`commands/doc.go`
- `fs/fs.go`、`fs/memory.go`、`fs/overlay.go`、`fs/mountable.go`、`fs/initial_files.go`、`fs/trie.go`、`fs/readwrite.go`
- `policy/policy.go`
- `host/host.go`、`host/system.go`、`host/process_unix.go`
- `trace/trace.go`
- `internal/runtime/session.go`、`runtime.go`、`execution_types.go`、`layout.go`、`layout_state.go`、`session_factory.go`、`buffer.go`、`passthrough_writer.go`、`host.go`、`virtual_devices.go`
- `internal/shell/core.go`、`command_dispatch.go`、`interp/jobs.go`、`interp/builtin.go`、`interp/vars.go`、`interp/runner.go`、`UPSTREAM_LICENSE`
- `internal/builtins/registry.go`、`help.go`、`kill.go`、`bash.go`、`bash_jobcontrol_linux.go`
- `server/server.go`、`server/session.go`

**文档站内容源（= 线上站点的 MDX 原文；站点 HTML 因 TLS 不可达）**
- <https://raw.githubusercontent.com/ewhauser/gbash/main/website/content/api/custom-commands.mdx>（对应站点 `/docs/api/custom-commands`）
- <https://raw.githubusercontent.com/ewhauser/gbash/main/website/content/api/index.mdx>（`/docs/api`）
- <https://raw.githubusercontent.com/ewhauser/gbash/main/website/content/api/sessions.mdx>（`/docs/api/sessions`）
- <https://raw.githubusercontent.com/ewhauser/gbash/main/website/content/api/runtime.mdx>（`/docs/api/runtime`）
- <https://raw.githubusercontent.com/ewhauser/gbash/main/website/content/api/wasm.mdx>（`/docs/api/wasm`）
- <https://raw.githubusercontent.com/ewhauser/gbash/main/website/content/api/server.mdx>（`/docs/api/server`）
- <https://raw.githubusercontent.com/ewhauser/gbash/main/website/content/configuration/filesystem.mdx>（`/docs/configuration/filesystem`）
- <https://raw.githubusercontent.com/ewhauser/gbash/main/website/content/guides/agent-integration.mdx>（`/docs/guides/agent-integration`）
- <https://raw.githubusercontent.com/ewhauser/gbash/main/website/content/commands/index.mdx>（`/docs/commands`）
- <https://raw.githubusercontent.com/ewhauser/gbash/main/website/content/security/index.mdx>（`/docs/security`）
- <https://raw.githubusercontent.com/ewhauser/gbash/main/website/app/lib/docs-navigation.ts>（导航结构 / URL 对照）

**备选方案**
- <https://github.com/mark3labs/go-bash>
- <https://api.github.com/repos/mark3labs/go-bash>（`gh api`：MIT / 14 stars / `v0.1.0`）
- <https://raw.githubusercontent.com/mark3labs/go-bash/master/README.md>
- `https://api.github.com/repos/mark3labs/go-bash/contents/fs/fs.go`

---

## 10. 冻结签收

| 检查项 | 状态 |
|---|---|
| 模块路径 / tag / commit / 日期 | ✅ §1 |
| 许可证 + 合规义务（含 BSD-3 传递） | ✅ §2 |
| 冻结依赖面（类型/函数清单 + 排除项） | ✅ §3 |
| 命令注入（mcp / nohup / jobs / ps） | ✅ §4.1（含 `Register` 覆盖语义实证） |
| 文件系统注入（layerfs 写穿 + 挂载策略） | ✅ §4.2（含 258 桩污染实测与规避） |
| host 二进制不落穿验证 | ✅ §4.3（含快照断言表） |
| 进程与作业模型边界 | ✅ §5（含重启收养的**否定**结论） |
| 超时与取消 | ✅ §6（含「禁止使用 `ExecutionRequest.Timeout`」） |
| 风险 + 升级策略 + 替代方案 | ✅ §7 |
| 待验清单 | ✅ §8（12 项 + 未获取清单） |

**需 Owner 裁决（3 条）**：

> 这 3 条不影响本文件的 pin 与 §3 依赖面冻结（契约本身可以签收），但会影响 `keel.jobs` 的
> 验收标准写法，必须在 `keel.jobs` 开工前定稿。

1. **R7 需要裁决**：`tool-surface/spec.md` 的「重启后收养」在 in-process 模型下无法按字面实现
   （§5.4）。建议改为「恢复事实 + 收敛终态」。这不是实现懒，是物理限制。
2. **V3 需要裁决**：gbash 沙箱内 `kill -TERM/-KILL $$` **不终止脚本**（实测 exit=0，继续执行）。
   模型若用它表达「中止」会静默失败。要在工具描述里明说。
3. **R3 需要裁决**：`jobs` 由 keel 覆盖，`fg`/`bg` 不提供。若产品侧期望「真 job control」，
   需明确这是**非目标**（上游同样列为非目标）。

未达成上述 3 条裁决不影响本文件的 pin 与依赖面冻结，但影响 `keel.jobs` 的验收标准写法。
