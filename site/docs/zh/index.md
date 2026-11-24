---
layout: home

hero:
  name: Keel
  text: 事件账本内核，加上一层 Harness
  tagline: 开源的 Go 智能体运行时。无状态、多租户，2GB 预算内跑 1000 个持续会话——杀掉进程，会话继续。
  actions:
    - theme: brand
      text: 下载 v0.2.0
      link: /zh/downloads
    - theme: alt
      text: Core —— 原理
      link: /zh/core/
    - theme: alt
      text: Harness —— 插件与运维
      link: /zh/harness/
    - theme: alt
      text: 30 秒快速开始
      link: /zh/core/#快速开始

features:
  - title: 账本是唯一真值
    details: 每次状态变化都是 PostgreSQL 里一行 append-only 事件。checkpoint 与转录都是投影；run 只是会话上的一把租约，不是内存里的对象。
    link: /zh/core/architecture
  - title: 无状态执行
    details: 任意 await 点 kill -9 都安全。新 owner 以更高的 fencing epoch 接管租约，从账本重建后继续——不丢事件，不重复最终回复。
    link: /zh/core/events
  - title: 真正的流式反压
    details: 图片从存储到 base64 再到 HTTP 全程流式，绝不整图进内存。按尺寸分池、全局令牌桶、逐次写超时，让带宽成为显式上限。
    link: /zh/core/storage
  - title: 两组扩展面
    details: Core 把服务、loop、provider 与外部 backend 做成可替换的接缝；其上的 Harness 再加一套插件标准，覆盖工具、hooks、触发器与 UI。
    link: /zh/core/extend/
---

## Keel 是什么

Keel 是通用的、无状态的、多租户**智能体运行时**。它回答一个工程问题：当你同时跑上千个持续会话
（每个都可能带图片、工具调用与长上下文）时，执行、存储、流式与反压应该长什么样——而且不能让进程内存随
会话数增长，也不能在重启时丢掉会话。

仓库同时交付两条主线：

| 主线 | 它是什么 | 入口 |
|---|---|---|
| **Core** | 账本、内核、服务图、存储与运行时门禁 | `./cmd/runtime`，文档 `/zh/core/` |
| **Harness** | Core 之上的产品层：会话、审批、插件、触发器、子代理、TUI 与 Web UI | `./cmd/keel`（`keel serve`），文档 `/zh/harness/` |

Keel 是 [PI](https://github.com/earendil-works/pi) 的 Go 重构：接口、事件语义与生态产物
（`SKILL.md`、MCP、压缩、遥测）以 PI 为唯一参照；只替换运行时（PostgreSQL / SSE / 多租户 / 2GB）。
loop 本体由 [pigo](https://github.com/smallnest/pigo)（MIT）搬运最小必要代码后改造，保留原始版权头。

## 30 秒快速开始

以下命令在本站所述修订上实际跑过；回复形状也是实际返回。

```bash
# 1. 账本的数据库
docker run -d --name keel-pg \
  -e POSTGRES_PASSWORD=runtime -e POSTGRES_DB=runtime \
  -p 13741:5432 postgres:18

# 2. 构建、引导首个超管并启动 Harness（产品壳 + API + worker）
git clone https://github.com/jixoai/keel.git && cd keel
go build -o keel ./cmd/keel
export KEEL_DSN='postgres://postgres:runtime@127.0.0.1:13741/runtime?sslmode=disable'
export KEEL_SERVE_TENANT=11111111-2222-3333-4444-555555555555
export KEEL_SECRET_KEY="$(openssl rand -hex 32)"   # 签 session/CSRF cookie
./keel admin bootstrap --tenant "$KEEL_SERVE_TENANT" --token-file ./setup.token
./keel serve --addr 127.0.0.1:13750
```

不需要 Docker：`keel` 只依赖一个 PostgreSQL 18 的 DSN。任何本机实例都可以（`createdb keel`，
再用 `KEEL_DSN=postgres://<user>@127.0.0.1:5432/keel?sslmode=disable`）；上面的 `keel-pg` 容器
只是一次性本地库的便利。少数验收脚本默认找名为 `keel-pg` 的容器，但都可用
`KEEL_DSN`/`KEEL_TEST_DSN` 指向本机库；只有 2GB cgroup 电池
（`scripts/acceptance-perf.sh`）确实需要 Docker/Linux——E0 容器电池因此标「平台受限」。

打开 `http://127.0.0.1:13750`，输入 setup token 与首个超管的用户名/密码，设置即完成并直接进入应用。
没有口令，也没有“第一个访问者自动成为超管”的路径。机器客户端用绑定到 Bot 的 key
（`keel_bot_...`），不走浏览器登录。登录/CSRF 请求与每个端点的形状见
[AgentAPI 参考](/zh/core/agentapi)。

没有配置模型时，运行时会使用确定性的 **fake provider**，所以回答是预置文本，但整条路径——edge 提交、
claim、loop、提交、投影——都真实跑过。要真实回答，把 `KEEL_MODEL`（配合 `KEEL_BASE_URL`/`KEEL_API_KEY`）
指向真实 provider。完整运行手册（测试、压测、2GB cgroup 复跑）见
[`docs/RUNBOOK.md`](https://github.com/jixoai/keel/blob/main/docs/RUNBOOK.md)。

## 接下来读什么

- **Core** —— 从 [Keel 是什么](/zh/core/) 开始，然后是[架构](/zh/core/architecture)、
  [事件与协议](/zh/core/events)、[Harness API](/zh/core/agentapi)、[存储与运行](/zh/core/storage)，
  以及[扩展地图](/zh/core/extend/)。
- **Harness** —— 从 [Harness 是什么](/zh/harness/) 与[插件](/zh/harness/plugins/)开始。
- **术语表** —— 两条主线共用的词汇：[术语表](/zh/glossary)。

## 仓库与许可

- 源码：`https://github.com/jixoai/keel`（Go module `github.com/jixoai/keel`，Go 1.27.3，见
  `go.mod:3`）
- 许可：Apache-2.0（[`LICENSE`](https://github.com/jixoai/keel/blob/main/LICENSE)）；自 pigo
  搬运的文件保留 MIT 版权声明
  （[`third_party/pigo-LICENSE`](https://github.com/jixoai/keel/blob/main/third_party/pigo-LICENSE)，
  归属说明见 [`NOTICE`](https://github.com/jixoai/keel/blob/main/NOTICE)）。
- 成熟度：每个能力按 G0–G5 分级，见
  [`docs/MATURITY.md`](https://github.com/jixoai/keel/blob/main/docs/MATURITY.md)；GA 的定义是
  全部能力达到 G5。
