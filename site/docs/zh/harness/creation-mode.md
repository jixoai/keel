# 创造模式

> **规划中。** 创造模式在
> `openspec/changes/keel-plugin-core/specs/plugin-system/spec.md`（「创造模式（作为内置插件
> `keel.creation`）」）中定规格，并在 `plugins/COVERAGE.md` 跟踪。本页内容在当前检出中均不
> 存在；它所基于的机制均已实现并给出出处。

创造模式回答一个简单的问题：*如果一切皆插件，那插件由谁来写？* 答案是智能体自己——在与一切
其余环节相同的审批与审计规则之下。

## 1. 它是什么

一个名为 `keel.creation` 的**内置插件**。它不是特权的内核特性：它可以像任何插件一样被停用，
并受制于它自己的权限集（`spec: 创造模式`）。它做的事：

```
用户用自然语言描述一个能力
        │
        ▼
keel.creation：生成插件包
        │   webview + react + solid-glass 模板
        │   plugin.yaml（id/version/kind/provides/permissions）
        │   ui/index.html + assets
        ▼
校验（清单 · 版本门 · 内容 hash · 模板 lint）
        │
        ▼
安装 → 启用（需要 durable 审批）
        │
        ▼
返回 UI 深链：/plugins/<id>/
```

规格的验收场景是此特性最短的陈述：*说一句「做一个展示当前会话 token 用量的小面板」，就会生成
一个 webview 插件包、安装并启用；插件区出现该插件；打开 UI 能看到数据；审计有 `install` 与
`enable` 两条记录*（`spec: 一句话生成插件`）。

## 2. 为什么它是插件，而不是内核模式

把创造模式做成插件会带来三个结果，而这三个都是目的所在：

1. **可停用。** 不想要自我扩展的部署停用 `keel.creation` 即可；其他一切不变。
2. **统一复核。** 生成的插件与手写插件走完全相同的管线：清单校验、审批、权限强制、审计
   （[权限](/zh/harness/plugins/permissions)、[生命周期](/zh/harness/plugins/lifecycle)）。
   不存在绕开规则的「生成版」捷径。
3. **统一产物。** 生成的插件就是受管目录里的普通包：可列举、可停用、可删除、可审计
   （`spec: 产物 MUST 可列举、可停用、可删除`）。

## 3. 失败是一等结果

生成是模型驱动的活动，必然会失败；失败路径被规定得与成功路径同样严格
（`spec: 生成失败可回滚`）：

- 生成或校验失败**不产生半装状态**——安装步骤在包级别是原子的；
- 错误原样返回给用户（不重新包装成泛泛的「出错了」）；
- 生成过程中产生的临时产物被清理；
- 生成与安装都写审计行，包括失败。

回滚规则与服务图对激活施加的纪律相同：*激活失败回滚自身 effects；绝不留下半注册的监听器*
（`internal/servicegraph/lifecycle.go:36`、`internal/agentext/plugin.go:317`）。

## 4. 创造模式如何与模型对话

创造模式是 Harness loop 中的普通参与者：

- 它与用户进行对话（是会话的扩展，而非独立向导）；
- 生成发生在一次带工具的回合中：模型在工作区写出包文件，插件的工具负责脚手架与校验；
- 安装/启用步骤**打开一条 decision** 并等待——审批是 durable 行，不是模态弹窗
  （[审批与问句](/zh/harness/approvals-and-questions)）；
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
