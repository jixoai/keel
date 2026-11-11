---
title: 下载
---

# 下载 keel

keel 交付为一个 Go 二进制加一份静态网页客户端。两者来自同一个 tag、在 CI 里交叉编译 —— 不在任何人的笔记本上构建，所以下面的校验和是有意义的。

**当前版本：`v0.1.0`**（1.0 之前；小版本之间可能有不兼容变更。）

## 你会拿到什么

| 产物 | 说明 |
|---|---|
| `keel` | 产品外壳：`keel serve`（AgentAPI + 网页界面 + SSE 同一个源）、`keel tui`（终端客户端）、`keel doctor`、`keel version` |
| `keel-web-0.1.0.zip` | 打包好的网页客户端。解压后用 `keel serve -web-dir <目录>` 指过去 |

## 运行前提

- **PostgreSQL 14+** —— 事件账本存在那里，`keel serve` 需要一个 DSN。
- **一个模型端点** —— 任意 OpenAI 兼容接口（或 Anthropic），外加一把 key。
- **网页客户端** —— 单独提供（二进制里没有内嵌）。

## 各平台构建

| 平台 | 下载 |
|---|---|
| macOS · Apple 芯片 | [keel-0.1.0-darwin-arm64.tar.gz](https://github.com/jixoai/keel/releases/download/v0.1.0/keel-0.1.0-darwin-arm64.tar.gz) |
| macOS · Intel | [keel-0.1.0-darwin-amd64.tar.gz](https://github.com/jixoai/keel/releases/download/v0.1.0/keel-0.1.0-darwin-amd64.tar.gz) |
| Linux · x86-64 | [keel-0.1.0-linux-amd64.tar.gz](https://github.com/jixoai/keel/releases/download/v0.1.0/keel-0.1.0-linux-amd64.tar.gz) |
| Linux · arm64 | [keel-0.1.0-linux-arm64.tar.gz](https://github.com/jixoai/keel/releases/download/v0.1.0/keel-0.1.0-linux-arm64.tar.gz) |
| Windows · x86-64 | [keel-0.1.0-windows-amd64.zip](https://github.com/jixoai/keel/releases/download/v0.1.0/keel-0.1.0-windows-amd64.zip) |
| 网页客户端 | [keel-web-0.1.0.zip](https://github.com/jixoai/keel/releases/download/v0.1.0/keel-web-0.1.0.zip) |

全部产物：[v0.1.0 发布页](https://github.com/jixoai/keel/releases/tag/v0.1.0) ·
更新的版本：[releases](https://github.com/jixoai/keel/releases) ·
[`SHA256SUMS.txt`](https://github.com/jixoai/keel/releases/download/v0.1.0/SHA256SUMS.txt)

## 校验你下到的东西

```bash
curl -LO https://github.com/jixoai/keel/releases/download/v0.1.0/SHA256SUMS.txt
shasum -a 256 -c SHA256SUMS.txt --ignore-missing
```

## 第一次运行

```bash
# 1. 账本需要一个数据库
createdb keel

# 2. 起服务（/v1、网页界面与 SSE 都在一个端口上）
export KEEL_DSN='postgres://postgres@127.0.0.1:5432/keel?sslmode=disable'
export KEEL_SERVE_TENANT="$(uuidgen | tr 'A-Z' 'a-z')"
export KEEL_SERVE_PASSWORD='自己定一个口令'
export KEEL_SERVE_WEB_DIR="$PWD/keel-web"   # 解压后的网页客户端
export KEEL_MODEL='deepseek-v4.1-flash'
export KEEL_PROVIDER='openai'
export KEEL_BASE_URL='https://api.example.com/v1'
export KEEL_API_KEY='…'
keel serve -addr 127.0.0.1:13750
```

打开 `http://127.0.0.1:13750`，输入口令，发一条消息。起不来时先跑 `keel doctor` —— 它会打印进程实际解析到的配置、构建信息与产物路径。

终端客户端连的是同一个服务：

```bash
keel tui --url http://127.0.0.1:13750
```

## 说明与如实边界

- **没有代码签名。** macOS 上先清一次隔离标记：`xattr -dr com.apple.quarantine ./keel`。
- **不带数据库。** 自备 PostgreSQL；`keel` 首次启动时跑自己的迁移。
- **1.0 之前。** AgentAPI 有版本号（`apiVersion: v1`），但产品面仍在动；稳定的是插件契约。
- **镜像。** GitHub 慢时把域名换成 `https://gh.fde2e.tweb.xin/`（自建镜像，国内首选）或
  `https://ghfast.top/https://github.com/…` —— 路径与校验和完全一致。
