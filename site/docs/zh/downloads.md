---
title: 下载
---

# 下载 keel

keel 每个平台只交付**一个自包含二进制**：PostgreSQL 18、网页客户端与 README 都在里面。
下载后一条命令就能在本机启动产品：不需要 Docker、不需要 `KEEL_DSN`、不需要手动 `initdb`。

**当前版本：`v0.3.0`**（1.0 之前；小版本之间可能有不兼容变更。）

## 下载

| 平台 | 下载 |
|---|---|
| macOS · Apple 芯片 | [keel-0.3.0-darwin-arm64.tar.gz](https://github.com/jixoai/keel/releases/download/v0.3.0/keel-0.3.0-darwin-arm64.tar.gz) |
| macOS · Intel | [keel-0.3.0-darwin-amd64.tar.gz](https://github.com/jixoai/keel/releases/download/v0.3.0/keel-0.3.0-darwin-amd64.tar.gz) |
| Linux · x86-64 | [keel-0.3.0-linux-amd64.tar.gz](https://github.com/jixoai/keel/releases/download/v0.3.0/keel-0.3.0-linux-amd64.tar.gz) |
| Linux · arm64 | [keel-0.3.0-linux-arm64.tar.gz](https://github.com/jixoai/keel/releases/download/v0.3.0/keel-0.3.0-linux-arm64.tar.gz) |
| Windows · x86-64 | [keel-0.3.0-windows-amd64.zip](https://github.com/jixoai/keel/releases/download/v0.3.0/keel-0.3.0-windows-amd64.zip) |

全部产物：[v0.3.0 发布页](https://github.com/jixoai/keel/releases/tag/v0.3.0) ·
[`SHA256SUMS.txt`](https://github.com/jixoai/keel/releases/download/v0.3.0/SHA256SUMS.txt)

## 启动

```bash
# macOS：生成 ~/Applications/Keel.app（本机 ad-hoc 签名，不公证）
chmod +x keel
./keel install

# Linux / Windows：原地启动
chmod +x keel
./keel start
```

`keel start` 会把内嵌运行时解到 `$KEEL_HOME/runtime/<version>`，初始化本地 PostgreSQL，
以 local-only 模式在 `http://127.0.0.1:13750` 起 Harness 并打开浏览器。数据在
`$KEEL_HOME`（默认 `~/.keel`）；`keel db status|stop|reset --yes` 管理本地集群。

本版 `keel install` 仅 macOS；Linux/Windows 直接 `keel start`（桌面注册后续补齐）。
`keel uninstall` 移除应用但保留数据，`--purge` 才删数据。

## Docker 变体

镜像与 compose 用 PostgreSQL service，不内嵌数据库：

```bash
docker compose up -d --build
```

原生二进制与 Docker 变体共用同一个服务端与网页客户端。

## 校验你下到的东西

```bash
curl -LO https://github.com/jixoai/keel/releases/download/v0.3.0/SHA256SUMS.txt
shasum -a 256 -c SHA256SUMS.txt --ignore-missing
```

## 说明与边界

- **下载的二进制没有代码签名。** macOS 可能隔离它：先执行
  `xattr -dr com.apple.quarantine ./keel`。`keel install` 生成的 `.app` 在本机
  ad-hoc 签名，不走公证。
- **单体二进制，无外部运行时。** PostgreSQL 18 已内嵌；应用在 `$KEEL_HOME` 下使用
  仅回环的本地集群。部署仍可用 `KEEL_DSN` 指向自己的 PostgreSQL，忽略内嵌运行时。
- **1.0 之前。** Harness API 有版本（`apiVersion: v1`），但产品面仍会变；插件契约是稳定部分。
- **Alpha 零兼容。** 旧环境变量与旧账号 schema 不再支持；旧库会拒绝启动并打印重建指引。
- **镜像。** GitHub 慢时可换 `https://gh.fde2e.tweb.xin/` 或
  `https://ghfast.top/https://github.com/…`，路径与校验和一致。
