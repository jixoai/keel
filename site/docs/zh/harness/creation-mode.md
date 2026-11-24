# 创造模式

> **已实现**：`keel.creation` 是内置插件，提供六个动作，与除 `keel.testkit` 外的所有内置插件
> 一样默认启用（`internal/harnessext/creation.go:91`、`cmd/keel/serve.go:1752`），并按
> webview + react + solid-glass 模板脚手架（`plugins/templates/webview-react-solidglass/`）。
> **Deferred**：端到端门 G8——「一句话 → 运行中的插件」——尚未通过
> （`openspec/changes/archive/2026-11-24-keel-harness-v2/DISPOSITION.md:20`）。本页描述已交付
> 的部分并明示缺口，不声称这是一个完全验收的特性。

创造模式回答一个简单的问题：*如果一切皆插件，那插件由谁来写？* 答案是智能体自己——在与一切
其余环节相同的审批与审计规则之下。

## 1. 它是什么

一个名为 `keel.creation` 的**内置插件**。它不是特权的内核特性：它可以像任何插件一样被停用，
并受制于它自己的权限集。清单在 `internal/pluginpkg/builtin/keel.creation.yaml`，实现位于
`internal/harnessext/creation.go`。它做的事：

```
用户用自然语言描述一个能力
        │
        ▼
keel.creation：脚手架一个插件包
        │   webview + react + solid-glass 模板
        │   plugin.yaml（id/version/kind/provides/permissions）
        │   ui/index.html + assets
        ▼
校验（清单 · 版本门 · 内容树 · 模板 lint）
        │
        ▼
安装 → 启用（需要 durable 审批）
        │
        ▼
返回 UI 深链：/plugins/<id>/
```

六个动作就是该插件的全部面（`internal/harnessext/creation.go:91`）：

| 动作 | 做什么 |
| --- | --- |
| `catalog` | 已安装插件及其面的只读聚合 |
| `scaffold` | 把模板渲染到 `<plugins-root>/.creation/<id>`，然后用**真实**包校验器校验产物 |
| `validate` | 暂存包的字段级问题，外加结构与宿主前置条件警告 |
| `install` | durable 审批 → manager `Install` → manager `Enable` → 深链 |
| `list` | 本会话生成了什么、处于什么状态 |
| `remove` | durable 审批 → 停用 → 卸载 → 删除暂存产物 |

模型经 harness 内建的 `creation` 聚合工具触达它们；`keel.creation.*` 名字是内部 handler，
永不发布给模型（`internal/harnessext/creation.go:88`）。创造模式**不**在插件内部调用模型：
主会话的 agent 写出包，这些工具是它在文件系统与插件管理器上的手。

## 2. 为什么它是插件，而不是内核模式

把创造模式做成插件会带来三个结果，而这三个都是目的所在：

1. **可停用。** 不想要自我扩展的部署停用 `keel.creation` 即可；其他一切不变。
2. **统一复核。** 生成的插件与手写插件走完全相同的管线：清单校验、审批、权限强制、审计
   （[权限](/zh/harness/plugins/permissions)、[生命周期](/zh/harness/plugins/lifecycle)）。
   不存在绕开规则的「生成版」捷径。
3. **统一产物。** 生成的插件就是受管目录里的普通包：可列举、可停用、可删除、可审计。

## 3. 失败是一等结果

生成是模型驱动的活动，必然会失败；失败路径被实现得与成功路径同样严格：

- 脚手架先渲染到临时目录，只在**真实**校验器接受后才移入暂存路径，因此失败不留下任何东西
  （`internal/harnessext/creation.go:28`）；
- 安装失败会移除暂存包；
- 错误原样返回给用户（不重新包装成泛泛的「出错了」）；
- 每一步都写审计行，包括失败。

回滚规则与服务图对激活施加的纪律相同：*激活失败回滚自身 effects；绝不留下半注册的监听器*
（`internal/servicegraph/lifecycle.go:36`、`internal/harnessext/plugin.go:317`）。

## 4. 创造模式如何与模型对话

创造模式是 Harness loop 中的普通参与者：

- 它与用户进行对话（是会话的扩展，而非独立向导）；
- 生成发生在一次带工具的回合中：模型在工作区写出包文件，插件的工具负责脚手架与校验；
- 安装/启用步骤**打开一条 decision** 并等待——审批是 durable 行，不是模态弹窗
  （[审批与问句](/zh/harness/approvals-and-questions)）。它是两阶段的，因为人的回答塞不进
  一次工具调用：先调 `install` 拿到 `approval_required` + `decisionId` + `answerUrl`，等人
  作答后再带该 `decisionId` 调一次 `install`。拒绝则什么都不改变。
- 每一步都作为普通事件进账本，因此整个创造过程像任何其他 run 一样可重放、可审计。

## 5. 生成的包必须满足什么

生成的插件与手写插件遵循同一标准——没有「生成版」豁免：

| 要求 | 规定在哪 |
| --- | --- |
| `plugin.yaml` 合法、`engines.keel` 满足 | [清单](/zh/harness/plugins/manifest) |
| kind: `webview`（模板），可带 sidecar | [三种 kind](/zh/harness/plugins/kinds) |
| 权限声明最窄；能力只经桥访问 | [权限](/zh/harness/plugins/permissions) |
| 使用宿主令牌（solid glass），不硬编码设计系统 | [插件 UI](/zh/harness/plugins/ui) |
| 确定性构建；资源位于 `ui/` 下 | [插件 UI](/zh/harness/plugins/ui) |
| generate/install/enable 写审计行 | §3 |

## 6. 与官方套件的关系

创造模式与官方插件套件是同一命题互补的两半：官方套件（`plugins/COVERAGE.md`，W1–W3 波次）把
keel 既有能力面*收进来*成为插件；创造模式让用户*长出新*插件而无需等待版本发布。两者最终都是
同一份清单里的行，受同一套权限与同一份审计约束。

## 7. 验收状态——尚未通过的部分

创造模式周边有两部分**没有**验收通过，文档如实写明，而不是暗示已完成：

| 项目 | 状态 | 证据 |
| --- | --- | --- |
| G8 端到端：AI 在真实会话中现场写一个原生插件，从开工到深链 | **Deferred**——端到端未跑通；webview E2E 路径已有 | `openspec/changes/archive/2026-11-24-keel-harness-v2/DISPOSITION.md:20` |
| v2 扩展面聚合与原生 chunk 主路径 | **partial**——声明格式已冻结并校验，但把每个已安装插件的 `extensibility.json` 聚合到创造面尚未完成 | `plugins/COVERAGE.md`（`keel.creation` → `partial`） |

所以诚实的总结是：六个动作、模板、审批路径与回滚纪律已交付；「说一句话，得到一个运行中的
插件」这道验收门仍然开着。在它关闭之前，把创造模式视为已实现但**尚未**完全验收。
