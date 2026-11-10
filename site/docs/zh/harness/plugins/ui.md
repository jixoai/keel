# 插件 UI

> **规划中。** webview 托管路径、`keel.plugin.*` 桥与模板要求来自
> `openspec/changes/keel-plugin-core/specs/plugin-system/spec.md`（「UI 承载与桥接」「TUI 行为」
> 「创造模式」）。模板所面向的主题词汇与客户端栈在本仓库中**已实现**，下文给出出处。

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

规格固定的规则：

- 页面从 **`/plugins/<id>/`** 加载，能显示自身状态并调用自身工具——对此的验证属于验收场景
  （`spec: 插件 UI 可用`）；
- 桥是唯一通道：**UI 绝不能拿到宿主密钥**（`spec: UI 承载与桥接`）；
- 主题必须使用**宿主令牌**，即 webview + react + solid-glass 模板（§3）。

同源设计不是巧合：web 客户端本就由 `keel serve` 从构建目录托管（`cmd/keel/static.go:16`），
`/plugins/<id>/` 因此搭同一 origin，无需 CORS——这正是 AgentAPI 设计时的前提
（`internal/agentapi/api.go:1`）。

## 2. `keel.plugin.*` 桥

桥就是插件 UI 的全部能力预算。四个动词，别无其他：

| 桥调用 | 做什么 | 今天由什么支撑 |
| --- | --- | --- |
| 读自身状态 | 插件在清单中的那一行：id、version、state、provides、diagnostics | `GET /v1/plugins` 快照（`internal/agentapi/ext.go:18`） |
| 调自身工具 | 调用**本插件**声明的工具（宿主按会话解析所属插件） | 与模型共用的同一工具派发 |
| 请求审批 | 打开一条 decision 并等待答案 | decision 原语（[审批与问句](/zh/harness/approvals-and-questions)） |
| 订阅事件 | 接收与本插件相关的事件（其触发器/decision/子代理生命周期） | hook 总线及其闭集（`internal/agentext/hooks.go:18`） |

刻意不在桥内的：任意 HTTP、文件系统访问、shell 执行、其他插件的工具、其他会话的事件，以及
任何密钥。需要网络能力的插件需要 sidecar 与声明的权限
（[权限](/zh/harness/plugins/permissions)）。

## 3. 模板：webview + react + solid-glass

规格要求模板唯一——**webview + react + solid-glass**——手写插件与创造模式产物共用
（`spec: 创造模式`「生成插件包（webview+react+solid-glass 模板）」）。「solid glass」不是气氛
词；它是 web 客户端已经在用的主题：

- 令牌文档是 `web/theme.json`（「Keel Solid Glass」）：明/暗两套 OKLCH 颜色变量、圆角
  （`radius`、`radius-card`、`radius-control`、`radius-pill`）、`font-sans`/`font-mono`——以
  CSS 变量被客户端消费；
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
      // 桥壳：宿主页面在 /plugins/<id>/ 下提供
      const state = await keel.plugin.state();          // 自身清单行
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

> 桥对象的精确形状属于规划中的 change；这段代码展示的是 §2 固定的四个动词，不是已冻结的 API。

## 4. TUI 行为

TUI 是一等客户端，且有一个刻意限制（`spec: TUI 行为`）：

- TUI **不得渲染插件 UI**（无 WebView、无 HTML）；
- 插件行提供**打开**，即对 web 深链调用 `openUrl`（`https://<host>/plugins/<id>/`）；
- 宿主没有 URL opener 时，TUI 改为显示可复制的 URL 文本——插件仍然可达，只是不在终端里渲染。

这与 TUI 对待 web 的既有方式一致：同一份数据，不同渲染面（`web/README.md`、
`docs/WEBUI.md`）。

## 5. 插件 UI 作者不该做的事

- 除桥的动词外，不要直接 fetch `/v1`——桥才是权限模型与审计能看到的地方；
- 不要在页面里存凭据：webview 存储按宿主隔离，且不是密钥材料；
- 不要重造宿主界面；使用令牌并让宿主掌握布局——这也是客户端自身遵守的规则（单一投影、
  web 与 TUI 共享 TSX）。
