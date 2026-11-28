---
title: 槽位与贡献
---

# 槽位与贡献

槽位（slot）是插件**声明**的具名扩展点；贡献（contribution）是另一个插件往里面放东西。Harness
拥有机制而不是目录：运行系统里的每个槽位 id 都属于声明它的那个插件，并携带该插件的命名空间
（`docs/PLUGIN-EXTENSIBILITY.md:9`）。

这正是第三方扩展不需要 Harness 发版的原因：一个插件可以声明 `acme.analytics.panel`，另一个插件向它
投贡献，而宿主从不需要知道这个名字。

## 1. 词汇

五个闭集，彼此正交（`docs/PLUGIN-EXTENSIBILITY.md:16`）：

| 词 | 取值 | 回答的问题 |
|---|---|---|
| `kind` | `in-process` / `sidecar` / `webview` | 插件如何承载 |
| `layer` | `kernel` / `harness` / `provider` / `ui` / `service` | 契约住在哪一层 |
| `form` | `chunk` / `package` / `iframe` / `data` | 贡献以什么形态运行 |
| `arity` | `single` / `multi` / `ordered` | 槽位接受几个贡献 |
| `target` | `web` / `native` / `tui` | 哪个客户端能渲染它 |

`layer` 可选，省略时由 `kind` 推导：`webview → ui`、`sidecar → provider`、`in-process → harness`
（`docs/PLUGIN-EXTENSIBILITY.md:25`）。显式 layer 不得与承载约束冲突（契约 §2.2）。

## 2. 声明一个槽位

```yaml
id: acme.panel
version: 1.0.0
kind: webview
description: A review panel with one slot of its own.

provides:
  slots:
    - id: acme.panel.card          # 必须带本插件 id 前缀
      forms: [chunk, data]         # 非空、闭集、去重
      arity: multi                 # 必填：single | multi | ordered
      targets: [web, native, tui]  # 非空
      schema:                      # forms 含 data 时必填
        type: object
        required: [title]
        properties:
          title: { type: string }
```

校验器执行的规则：

- **命名空间。** 槽位 id 形如 `<plugin>.<name>`，前缀必须是声明者 id
  （`internal/pluginpkg/manifest.go:914`）。裸名 `route` 被拒；`acme.panel.card` 被接受。
- **自洽。** 每个声明的 target 至少要有一个可渲染的形态——「只收 iframe、只声明 tui」不是合法槽位
  （`internal/pluginpkg/manifest.go:964`）。
- **data 需要 schema。** 接受 `data` 的槽位必须声明这些 payload 遵守的 JSON schema
  （`internal/pluginpkg/manifest.go:968`）。
- **单一声明者。** 一个槽位 id 只能由一个插件声明；第二个声明是 `ERR_SLOT_CONFLICT`，整次注册被拒
  （`web/src/plugins/registry.ts:124`）。

契约冻结了一组第一方槽位——`keel.ui.route`、`keel.ui.nav`、`keel.ui.action`、`keel.ui.tool-renderer`、
`keel.ui.message-part`、`keel.ui.sidebar`、`keel.ui.settings-section`、`keel.ui.surface`、
`keel.tasks.widget`（`internal/pluginpkg/manifest.go:329`、`docs/PLUGIN-EXTENSIBILITY.md:93`）。
它们仍然是插件声明：卸载声明者（`keel.ui`、`keel.tasks`）会连同它的槽位以及所有投向它们的贡献
一起撤销。

web 客户端还声明了自己的基础槽位，包括系统栏插件行 `keel.ui.top-band`
（`web/src/plugins/base.ts:15`、`:24`）。在瞄准一个并非自己声明的槽位前，先确认部署客户端的
基础集合。

## 3. 投贡献

```yaml
contributes:
  - { slot: acme.panel.card, form: chunk, entry: ui/card.js, title: Card }
  - { slot: acme.panel.card, form: data, payload: { title: Card } }
  - { slot: keel.ui.settings-section, form: chunk, entry: ui/settings.js }
```

| 形态 | 运行方式 | 隔离 | 宿主主题 | web | native | tui |
|---|---|---|---|---|---|---|
| `chunk` | NSH 动态导入，宿主树内 | 无（刻意） | 是 | ✅ | ✅ | ✅ |
| `package` | 签名 NSH 包，独立 surface + HostLimits | VM | 是 | ✅ | ✅ | ❌ |
| `iframe` | iframe / WebView 页面 | 浏览器 | 否 | ✅ | ✅ | ❌ |
| `data` | 宿主消费的 JSON | 无 | 是 | ✅ | ✅ | ✅ |

这张矩阵是契约（`docs/PLUGIN-EXTENSIBILITY.md:112`），也是客户端自己的表
（`web/src/plugins/forms.ts:32`）。

形态规则：

- `chunk` / `package` / `iframe` 必须带 `entry`：包内相对路径，不得含 `..`、反斜杠或控制字符；
  文件必须真实存在于包内（`internal/pluginpkg/package.go:140`）。`package` 的 entry 必须以
  `.nshpkg` 结尾；`iframe` 的 entry 不得是 `javascript:` URL
  （`internal/harnessext/plugin_extension.go:476`）。
- `data` 必须带非空 `payload`，且不得带 `entry`。
- 投向**本清单声明的槽位**的贡献按该槽位的 forms 校验；投向外部槽位的贡献在装配期按已安装插件声明的
  槽位校验（`Environment.KnownSlot`）；该解析器存在时，未知槽位 fail-closed
  （`internal/pluginpkg/manifest.go:431`、`:1075`）。
- `iframe` 需要 `provides.ui.entry`（`/plugins/<id>/` 页面）。`webview` 插件可以是纯 chunk 包，
  完全省略 `provides.ui`（`docs/PLUGIN-EXTENSIBILITY.md:74`、`:75`）。

## 4. 目标与降级

客户端按 target（`web`、`native`、`tui`）解析槽位。不可用是显式的，绝不是空白洞：

- 某形态在目标上跑不了时，返回 `unavailable` 并给出错误码与一句话
  （`web/src/plugins/forms.ts:47`）。在终端里，这句话说明该贡献需要图形界面，并提供在浏览器打开的入口。
- 槽位本身没有声明该 target 时是 `ERR_TARGET_UNAVAILABLE`（`web/src/plugins/forms.ts:75`）。
- `package` 目前返回 `ERR_UNSUPPORTED`：本客户端还没有接线独立 surface 运行器，页面如实说明，
  而不是伪造一个面板（`web/src/plugins/forms.ts:62`）。

## 5. Arity

| Arity | 行为 |
|---|---|
| `single` | 只收一个贡献；第二个插件的贡献是 `ERR_SLOT_FULL`，被拒绝（`web/src/plugins/registry.ts:185`） |
| `multi` | 所有合法贡献都渲染；宿主按声明顺序绘制 |
| `ordered` | 清单与客户端都会校验；客户端不额外排序，因此声明顺序即顺序 |

贡献按 `(pluginId, contributionId)` 挂载；被替换贡献的迟到 chunk 不会覆盖后继，因为每次加载都带
世代号（`web/src/plugins/registry.ts:54`）。

## 6. 失败隔离

坏插件不能拖垮宿主，代码就是按这条规则写的：

- **坏声明不会让 manager 失败。** `DecodeExtension` 把字段问题写进 `Problems`，本身不返回错误
  （`internal/harnessext/plugin_extension.go:185`）。插件可以安装，只是它的 UI 面不加载。
- **注册是原子的。** 任何字段错误都让该插件之前的声明原样保留：绝不会出现半个贡献
  （`web/src/plugins/registry.ts:83`）。
- **chunk 失败是一种界面，不是一个洞。** 贡献被标记 `failed`，带错误码
  （`web/src/plugins/registry.ts:495`），并提供重试（`web/src/plugins/registry.ts:421`）；槽位其余部分
  照常渲染。
- **槽位与贡献一起消失。** 禁用/卸载插件会注销它的槽位**以及**其他插件投向这些槽位的贡献，并给出警告
  （`web/src/plugins/registry.ts:308`）。
- **意图与装配分离。** manager 拥有数据库说的状态，runtime 拥有实际加载的东西；已启用但加载失败的
  插件按 runtime 诊断上报，不假装健康（`internal/harnessext/plugin_manager.go:25`）。

在浏览器侧，插件的扩展面就是 `GET /v1/plugins`：每行带 `layer`、`slots`、`contributes` 与
`extensibility` 指针（`internal/harnessext/plugin_manager_view.go:66`）。插件被禁用时声明照样下发，
由客户端决定加载什么。

## 7. `extensibility.json` 与目录

`extensibility.json` 是 `EXTENSIBILITY.md` 的机读对应物（`docs/PLUGIN-EXTENSIBILITY.md:135`）。
其 `schema` 必须是 `keel.plugin-extensibility/1`，且 `plugin` / `version` / `slots` / `contributes`
必须与 `plugin.yaml` 完全一致；漂移在装配期被拒绝（`docs/PLUGIN-EXTENSIBILITY.md:182`）。

- 官方插件**必须**声明 `extensibility.doc` + `extensibility.schema` 并随包提供两个文件
  （`docs/PLUGIN-EXTENSIBILITY.md:177`）。
- 第三方插件可以省略；安装照常成功，但会被标记「不可被创造模式利用」。
- 创造模式把本机所有已安装插件的文档聚合成一份机读目录
  （`docs/PLUGIN-EXTENSIBILITY.md:184`）。

不要把它和 `plugins/keel.plugin-market/catalog.json` 混为一谈：后者是**覆盖矩阵**
（schema `keel.plugin-market.catalog/1`，`plugins/keel.plugin-market/catalog.json:2`）——哪项上游能力
由哪个 keel 插件覆盖、处于什么状态。它不是槽位清单：槽位是否存在由 `GET /v1/plugins` 里的声明决定。
它的人读投影是 `plugins/COVERAGE.md`，唯一真源由 `docs/PLUGIN-CATALOG.md:3` 界定。

## 8. 校验一个扩展面

```bash
go test ./internal/pluginpkg                 # 字段级校验器 + 官方文档门禁
go run ./plugins/tools/verifypkg             # manifest + 真实 sidecar 握手 + 文档
node scripts/plugin-catalog.mjs --check      # catalog 唯一真源与 COVERAGE 对账
(cd web && nsh test --target tui)            # 客户端注册表与槽位渲染测试
```

前三条与契约列出的入口一致（`docs/PLUGIN-EXTENSIBILITY.md:262`），最后一条是客户端套件。字段错误会
点名作者写的路径，例如 `provides.slots[0].arity` 或 `contributes[2].form`。

## 9. 相关页面

- [插件](/zh/harness/plugins/)——这些声明外面的包格式、生命周期与权限。
- [清单](/zh/harness/plugins/manifest)——`plugin.yaml` 的每个字段。
- [插件 UI](/zh/harness/plugins/ui)——`/plugins/<id>/` 与 `keel.plugin.*` 桥。
- [创造模式](/zh/harness/creation-mode)——`extensibility.json` 的消费者。
