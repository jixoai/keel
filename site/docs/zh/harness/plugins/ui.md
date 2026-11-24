# 插件 UI

> **已实现。** 托管路由 `/plugins/<id>/` 与四动词 `keel.plugin.*` 桥均已交付；桥还会把宿主
> 主题令牌下发给插件 UI（`cmd/keel/plugins_ui.go:17`、`internal/harnessapi/plugin_bridge.go:1`）。
> 模板（webview + react + solid-glass）在仓库中，也是创造模式脚手架产出的模板
> （`plugins/templates/webview-react-solidglass/`）。

## 1. 插件 UI 如何托管

```
<plugins-root>/<id>/ui/          宿主托管于：/plugins/<id>/
├─ index.html                    入口文档（provides.ui.entry）
└─ assets/**                     JS、CSS、字体、图片

WebView（web target）            打开插件的 web 深链
   │                                （TUI 改为打开 URL，不渲染）
   ▼
keel.plugin.* 桥                 唯一能力通道：
                                 读自身状态 · 调自身工具 ·
                                 请求审批 · 订阅事件
```

门是生命周期状态，只有 `enabled` 打开它（`cmd/keel/plugins_ui.go:17`）：

| 状态 | `/plugins/<id>/…` 的回答 |
| --- | --- |
| 未安装 | `404`——该 id 不是本次部署的事实 |
| `installed`、`disabled`、`failed`、`invalid` | `403`——运维要求它关闭 |
| `enabled` | 提供，无论插件是否有运行时面（纯 UI 的 webview 会被启用但从不参与装配） |

遏制是路由的一部分：`..` 段被拒，解析后的文件必须在**符号链接解析之后**仍位于 UI 根内，
`plugin.yaml` 本身永不提供。

同源设计不是巧合：web 客户端本就由 `keel serve` 从构建目录托管（`cmd/keel/static.go:16`），
`/plugins/<id>/` 因此搭同一 origin，无需 CORS——这正是 Harness API 设计时的前提
（`internal/harnessapi/api.go:1`）。

## 2. `keel.plugin.*` 桥

桥就是插件 UI 的全部能力预算。四个动词，别无其他
（`internal/harnessapi/plugin_bridge.go:75`）：

| 桥调用 | HTTP 路由 | 做什么 |
| --- | --- | --- |
| 读自身状态 | `GET /v1/plugins/{id}/bridge/state` | 插件自己的清单行（id、version、kind、state、provides、permissions、requires、tools、hooks、triggers、diagnostics、layer、slots、贡献、扩展文档）加清单 revision/generation 与宿主**主题令牌** |
| 调自身工具 | `POST /v1/plugins/{id}/bridge/call` | 调用**本插件**声明的工具；其他插件的工具不在桥的词汇表里，不是权限问题 |
| 请求审批 | `POST /v1/plugins/{id}/bridge/approve`，随后对 `/v1/plugins/{id}/bridge/approvals/{decisionId}` 做 `GET`/`POST` | 打开 durable decision 并等待答案 |
| 订阅事件 | `GET /v1/plugins/{id}/bridge/subscribe`（SSE，15s 心跳） | 转发与本插件相关的事件 |

每个动词都要求插件处于 **enabled**——与托管路由同一道门——且插件贡献未声明的动词会以带类型的
`ERR_ACCESS_DENIED` 拒绝（`internal/harnessapi/plugin_bridge.go:295`）。贡献未声明任何
capabilities 的插件保留旧的完整桥（其工具仍做归属校验）。

状态回答是显式投影：绝不是原始清单文档、绝不是受管包路径、绝不是来源文档、绝不是凭据。插件 UI
因此即使意外也无法经桥读到宿主密钥。

刻意不在桥内的：任意 HTTP、文件系统访问、shell 执行、其他插件的工具、其他会话的事件，以及
任何密钥。需要网络能力的插件需要 sidecar 与声明的权限
（[权限](/zh/harness/plugins/permissions)）。

## 3. 模板：webview + react + solid-glass

模板唯一——**webview + react + solid-glass**——手写插件与创造模式产物共用。「solid glass」
不是气氛词；它是 web 客户端已经在用的主题：

- 令牌文档是 `web/theme.json`（「Keel Solid Glass」）：明/暗两套 OKLCH 颜色变量、圆角
  （`radius`、`radius-card`、`radius-control`、`radius-pill`）、`font-sans`/`font-mono`——以
  CSS 变量被客户端消费，并经桥的状态动词交给插件 UI
  （`internal/harnessapi/plugin_bridge.go:151`）；
- 插件 UI 必须消费这些宿主令牌而不是硬编码数值，从而自动继承宿主的明/暗模式与间距，无需自带
  设计系统。

一个符合模板的最小 `ui/index.html`：

```html
<!doctype html>
<html>
  <head>
    <meta charset="utf-8" />
    <title>example panel</title>
    <!-- 宿主令牌：与客户端消费的是同一批 CSS 变量（web/theme.json） -->
  </head>
  <body>
    <div id="root"></div>
    <script type="module">
      // 桥壳：页面托管于 /plugins/<id>/ 下，宿主对其暴露 keel.plugin.* 对象。
      const state = await keel.plugin.state();          // 自身清单行 + 主题
      const result = await keel.plugin.callTool("example_status", {});
      const answer = await keel.plugin.requestApproval({
        prompt: "安装这个插件？", options: [{ id: "allow" }, { id: "deny" }],
      });
      const unsubscribe = keel.plugin.onEvent((evt) => console.log(evt));
      document.querySelector("#root").textContent = JSON.stringify(result, null, 2);
    </script>
  </body>
</html>
```

> 四个动词是固定的；JS 对象的具体形状属于宿主桥的实现，不是已冻结的 wire 契约。

## 4. TUI 行为

TUI 是一等客户端，且有一个刻意限制：

- TUI **不得渲染插件 UI**（无 WebView、无 HTML）；
- 插件行提供**打开**，即对 web 深链调用 `openUrl`（`https://<host>/plugins/<id>/`）；
- 宿主没有 URL opener（或平台拒绝）时，行内改为保留可复制的 URL——插件仍然可达，只是不在终端
  里渲染（`web/src/screens/plugins.tsx:13`、`:220`）。

这与 TUI 对待 web 的既有方式一致：同一份数据，不同渲染面（`docs/WEBUI.md`）。注意打包限制：
`nsh package` 没有 `tui` target，因此 TUI 只能走开发宿主（`nsh run --target tui`）或嵌入
`nsh-platform-tui`，`KEEL_TUI_BIN` 指向用户自备的二进制（`web/README.md:167`）。

## 5. 插件 UI 作者不该做的事

- 除桥的动词外，不要直接 fetch `/v1`——桥才是权限模型与审计能看到的地方；
- 不要在页面里存凭据：webview 存储按宿主隔离，且不是密钥材料；
- 不要重造宿主界面；使用令牌并让宿主掌握布局——这也是客户端自身遵守的规则（单一投影、
  web 与 TUI 共享 TSX）。
