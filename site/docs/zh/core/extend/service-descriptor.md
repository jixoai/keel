# 写一个服务描述符

本页就是规格里那条「照做即可」的路径：写一个服务描述符并接入 runtime，让服务达到 `ACTIVE`。以下内容
与仓库自身的蓝图和测试逐条对表。

## 1. 契约

一个服务由 `ServiceDescriptor` 描述（`internal/servicegraph/descriptor.go:26`），由工厂构造
（`internal/servicegraph/lifecycle.go:49`）：

```go
type Factory func(ctx context.Context, resolved Resolved) (Instance, error)

type Instance interface {
    Start(ctx context.Context, effects *EffectScope) error
    Stop(ctx context.Context) error
}

type ServiceDescriptor struct {
    Name     string   // loadout 内唯一，例如 "pi-loop"
    Version  string   // 实现身份；实现变了就升
    Provides []string // 能力，例如 "ctx.llm"
    Requires []string // 必须先被服务；缺 ⇒ PENDING
    Optional []string // 缺 ⇒ ACTIVE 但记录降级诊断
    Role     Role     // 能力链里 primary | fallback
    Effects  []string // 具名清理项，给运维看
    Factory  Factory  // 由 loadout 构建器挂上
}
```

服务图强制、新服务必须遵守的规则：

- **工厂是纯构造**。只做 `Stop` 能释放的获取；不要在那里注册监听器/定时器/注册项。
- **effects 在 `Start` 里通过 scope 注册**。`EffectScope.Register(kind, release)`
  （`internal/servicegraph/lifecycle.go:119`）收集本实例**在本代际**的清理项。释放按注册反序执行，
  单个释放失败不阻止其余。scope 已关闭时返回 `ErrScopeClosed`——调用方必须自行释放该 effect 并中止，
  否则 effect 会泄漏到代际之外。
- **依赖方通过能力绑定**。依赖方调用 `servicegraph.BindAs[你的接口](resolved, "ctx.你的能力")`，把
  provider 实例断言成本地声明的窄接口——绝不直接断言成具体 store 或内核句柄。
- **重装配身份是 `(Name, Version, Provides, Requires, Optional, Role)`**。其中任一项变了，服务图就会
  拆旧建新；只有配置变了则实例跨代共享。实现变了必须升 `Version`，否则重装配会静默保留陈旧实例。

## 2. 能力词汇

能力名来自 `internal/loadout/loadout.go:37`（`ctx.store`、`ctx.llm`、`ctx.prompt`、`ctx.mcp`、
`ctx.tools`、`ctx.loop`、`ctx.compaction`、`ctx.session-projection`、`ctx.telemetry`、`ctx.worker`、
`ctx.sandbox`、`ctx.external-agent`、`ctx.external-tools`）。要加能力就挨着加常量——诊断会说出这些
名字，而拼错的字符串是「缺依赖」，不是编译错误。

## 3. 步骤

**第 1 步——声明描述符。** 按蓝图风格（`internal/loadout/loadout.go:83`）写；声明顺序也被当作装配
顺序的依据。

```go
var defaultServices = []servicegraph.ServiceDescriptor{
    // …现有条目…
    {
        Name:     "audit-log",
        Version:  Version,             // loadout.Version（"v1"）
        Provides: []string{"ctx.audit"},
        Requires: []string{CapStore},  // 先要 store 接缝
        Optional: []string{CapTelemetry},
        Effects:  []string{"audit-writer"},
    },
}
```

**第 2 步——实现实例。**

```go
type auditService struct {
    ports  []string      // 解析后的配置
    writer *audit.Writer // 工厂里构建
}

func newAuditService(cfg AuditOptions) func(ctx context.Context, r servicegraph.Resolved) (servicegraph.Instance, error) {
    return func(ctx context.Context, r servicegraph.Resolved) (servicegraph.Instance, error) {
        // 纯构造：解析依赖、构建对象，Stop 负责释放
        mirror, err := servicegraph.BindAs[agent.RunMirror](r, loadout.CapStore)
        if err != nil { return nil, err }
        return &auditService{ /* … */ }, nil
    }
}

func (s *auditService) Start(ctx context.Context, effects *servicegraph.EffectScope) error {
    // 副作用写在这里，受本代际 scope 约束
    return effects.Register("audit-writer", func() error {
        s.writer.Close()
        return nil
    })
}

func (s *auditService) Stop(ctx context.Context) error { return nil } // effects 已被释放
```

**第 3 步——挂上工厂。** 宿主进程通过 `loadout.Config.Provide` 提供工厂；工厂为 `nil` 的服务会被
`loadout.Default(cfg)` 直接省略（`internal/loadout/loadout.go:226`）。注意服务「合法缺席」的两种方式：

```go
lo, err := loadout.Default(loadout.Config{
    ID:       "default",
    Revision: 2,
    Provide: func(service string) servicegraph.Factory {
        switch service {
        case loadout.ServiceStore: return storeFactory
        case "audit-log":          return newAuditService(auditCfg) // 你的服务
        default:                   return nil                        // 声明了但不开
        }
    },
    LoopConformance: []loadout.LoopConformance{{Service: loadout.ServicePiLoop, Manifest: agentloop.Manifest()}},
})
```

**第 4 步——应用并检查诊断。**

```go
eng := servicegraph.NewEngine(servicegraph.WithRetireTimeout(30*time.Second))
snap, err := eng.Apply(ctx, lo)      // internal/servicegraph/engine.go:139
if err != nil { … }                   // 启动失败：只回滚失败服务的 effects
lease, _ := eng.Acquire()             // run 由此 pin generation + revision（engine.go:270）
inst, ok := eng.Lookup("audit-log")   // 查看当前活着的是什么（engine.go:367）
```

`Snapshot()` 给出每个服务的状态与诊断——那里能看到「PENDING: 在等 ctx.store」或可选依赖的降级记录。
`servicehost.Boot` 会把运行路径上非 `ACTIVE` 的服务变成启动失败（`internal/servicehost/host.go:343`），
因此坏描述符在生产的装配里不会被静默忽略。

**第 5 步——退役路径。** reload 改动你的服务时，旧代进入 `RETIRING`（不接新 run），引用归零或
retire 超时后按反序卸载。`Engine.CollectExpired` 必须被周期调用——宿主 ticker 负责
（`engine.go:303`、`docs/LOADOUT.md:114`）。如果你的 effect 会活过代际，退役截止后它会以「泄漏的注册项」
出现在诊断里。

## 4. 刻意不完整的描述符

蓝图里有两种形状值得照抄，而不是「修好」：

- **`sandbox`：声明了，没有工厂。** `ServiceSandbox` 在蓝图里、`Provides: ["ctx.sandbox"]`，而全仓没有
  任何实现。它是 `prompt` 的可选依赖，因此缺席只记录降级诊断，什么都不阻塞（`docs/LOADOUT.md:48`）。
  要落地 sandbox 就加工厂，而不是加服务名。
- **`prompt` 与 `tools`：一个对象，两个能力。** `tools` requires `prompt`，因为 registry 必须是同一个
  host 对象的视图；两份注册表会在 `RefreshMCP` 时分叉（`docs/LOADOUT.md:44`）。

## 5. 替换 loop（特例）

`ctx.loop` 是唯一带额外门的的能力。任何提供 `ctx.loop` 的服务都必须声明 PI conformance manifest，
其规范哈希与自己的 pin 相等；没有声明、没有 pin 或哈希不符的 loop 会在 loadout 存在**之前**被拒绝
（`internal/loadout/conformance.go:55`）。默认蓝图声明 `agentloop.Manifest()` 并 pin
`loadout.PinnedPiLoopManifestHash`（`internal/loadout/conformance.go:18`）。

```go
// 默认 loop 通常够用；替换者必须做的是：
cases := agentloop.Manifest()             // 你的 loop 通过的套件（agentloop/manifest.go:72）
hash := agentloop.ManifestHash()          // 这些用例的规范哈希（manifest.go:79）
lo, err := loadout.Default(loadout.Config{
    Provide:         provideFactories,     // 你为 loadout.ServicePiLoop 提供的工厂
    LoopConformance: []loadout.LoopConformance{{Service: loadout.ServicePiLoop, Manifest: cases}},
    LoopPins:        map[string]string{loadout.ServicePiLoop: hash},
})
```

这个门是「声明检查器」，不是证明：它让「此 loop 通过了钉住的套件」成为强制、可哈希校验的声明。
不要为了过门而削弱套件——pin 是仓库里的字面量（`internal/loadout/conformance.go:18`），manifest 漂移
会有测试失败。

## 6. 新服务的验收

| 门 | 命令 |
|---|---|
| 单测 / race | `go test -race ./internal/servicegraph ./internal/loadout ./internal/servicehost` |
| 装配平价 | R1 差分：同输入、同库，白名单之外零差异（`cmd/r1diff`、`docs/RUNBOOK.md:162`） |
| 全量验收 | `KEEL_TEST_DSN='postgres://…' KEEL_EVIDENCE_DIR=/tmp/keel-evidence scripts/acceptance-all.sh` |
| 默认装配变更时 | R1 门是强制的——它是「默认 loadout 复现旧行为」的唯一证明 |

各门覆盖什么、耗时如何，见[测试与验收](/zh/core/extend/testing)。
