---
title: 发布与升级
---

# 发布与升级

一次 keel 发布＝一个被打上 tag 的提交、CI 从它构建出的六个产物、一份校验和文件，以及给运行中进程
的一套交接口径。没有任何东西在笔记本上构建，所以公布出来的 SHA 才有意义。

## 1. 一个版本常量，两个版本号

`keel` 有产品版本与协议版本，两者不是一回事：

| 版本号 | 位置 | 含义 |
|---|---|---|
| 产品版本 | `cmd/keel/main.go:23` | 发版号（`0.2.0`）；`keel version` 输出的就是它 |
| HarnessAPI 版本 | `harnessapi.APIVersion` | `/v1` 契约；客户端只锁 major 并查 `/v1/capabilities` |

常量上方的注释写明了这个区分（`cmd/keel/main.go:20`），`printVersion` 渲染 `keel <version>`
以及构建信息（`cmd/keel/main.go:127`）。改产品版本是一次源码变更：像其他改动一样提交、评审、打 tag。

## 2. tag 是触发器，门禁先跑

发布工作流由 `v*` tag（或显式指定 tag 的手工 dispatch）触发。在构建任何东西之前，它先把 tag 与
二进制自己的报告比对：

```bash
# 门禁在本地做的事
go run ./cmd/keel version          # 输出：keel 0.2.0
# tag 必须是 v + 这个数字，否则在发布前失败
```

门禁在 `.github/workflows/release.yml:43`：解析 `go run ./cmd/keel version` 的输出，tag 与报告版本不一致即中止，
并提示「先 bump 版本常量再打 tag」（`.github/workflows/release.yml:50`）。一个对二进制撒谎的 tag
比一次红色构建更糟。

## 3. 一个 tag 发布什么

| 产物 | 平台 / 内容 | 构建处 |
|---|---|---|
| `keel-<ver>-darwin-arm64.tar.gz` | macOS Apple 芯片 | `.github/workflows/release.yml:65` |
| `keel-<ver>-darwin-amd64.tar.gz` | macOS Intel | `.github/workflows/release.yml:65` |
| `keel-<ver>-linux-amd64.tar.gz` | Linux x86-64 | `.github/workflows/release.yml:65` |
| `keel-<ver>-linux-arm64.tar.gz` | Linux arm64 | `.github/workflows/release.yml:65` |
| `keel-<ver>-windows-amd64.zip` | Windows x86-64（`keel.exe`） | `.github/workflows/release.yml:65` |
| `keel-web-<ver>.zip` | 打包好的网页客户端 | `.github/workflows/release.yml:101` |
| `SHA256SUMS.txt` | 排序后每个产物一行 | `.github/workflows/release.yml:104` |

每个归档含二进制，以及存在时的 `README.md`；web zip 是打包好的客户端，含宿主主题文档
（`scripts/package-web.sh:21`）。`serve`/`tui`/`doctor`/`version` 子命令在每个二进制里。**没有任何
代码签名**；macOS 首次运行需清一次隔离标记：`xattr -dr com.apple.quarantine ./keel`
（`.github/workflows/release.yml:123`）。

TUI 不是打包产物：`nsh package` 没有 `tui` target，所以 `keel tui` 只能跑在开发宿主
（`nsh run --target tui`）或嵌入的 `nsh-platform-tui` 上（`web/README.md:167`）。

## 4. 流水线顺序

1. **版本门**——tag ↔ 二进制（`.github/workflows/release.yml:43`）。
2. **交叉编译**——五个 `GOOS/GOARCH` 目标，`CGO_ENABLED=0`、`-trimpath`、`-s -w`
   （`.github/workflows/release.yml:70`）。`-ldflags -X main.version=$VERSION` 把 tag 写进二进制。
3. **网页客户端**——安装固定版本的 NSH 并打包站点（`.github/workflows/release.yml:83`）。NSH 固定到 release，不用
   滚动 nightly（`.github/workflows/release.yml:94`）——nightly 漂移已经弄坏过这个构建一次。
4. **校验和**——对 `dist` 内全部产物 `sha256sum`，按排序输出（`.github/workflows/release.yml:104`）。
5. **发布**——在本仓创建 release；重试时上传到同一条（`.github/workflows/release.yml:115`）。
6. **镜像**——配置了 `FDE2E_RELEASES_TOKEN` 时，同样的字节进 `jixoai/fde2e-releases`，tag 为
   `keel-v<ver>`（`.github/workflows/release.yml:129`）；没有 token 则跳过且保持绿灯。

网页构建刻意 fail-closed：v0.2.0 曾因该步骤是 `continue-on-error` 而漏发 zip，而「半个发布」比一次
可修复的红色构建更糟（`.github/workflows/release.yml:85`）。

可复现是意图，不是保证：`-trimpath` 加固定工具链让同一份源码重跑产出同样字节
（`.github/workflows/release.yml:7`）；换了 Go 补丁版本或换了 NSH pin，就是换了输入。

## 5. 站点切版

站点是静态的 VitePress 构建；版本字符串活在页面里，而不是某个配置常量。因此一次发版切版＝版本 bump、
页面修改，再加门禁：

```bash
# 1. 版本常量（一个文件一行）
sed -n '23p' cmd/keel/main.go

# 2. 站点里引用版本号的地方
rg -n 'v0\.2\.0' site/docs/index.md site/docs/downloads.md site/docs/zh/downloads.md

# 3. 提交 bump，再打 tag 并推送
git tag v0.2.1 && git push origin v0.2.1

# 4. 发布绿了之后，把页面指向新 tag 并重建站点
cd site && pnpm build
```

`site/scripts/build.mjs` 是构建契约：死链会让构建失败，EN/ZH 页面集必须一致，每条 `path:line`
引用都必须能解析（`sed -n '1,10p' site/scripts/build.mjs`）。任何一步失败都保留先前已部署的版本
（`sed -n '9,10p' site/scripts/build.mjs`），所以坏掉的切版是「没有部署」，而不是「站点坏了」。

切版的规矩：

- **校验和只能照抄，不能手打。** 页面的 SHA 块与 `SHA256SUMS.txt` 必须是同一批字符串；从 release
  复制，不要重敲。
- **三个语言面一起动**：`index.md`（hero 按钮）、`downloads.md`、`zh/downloads.md`。parity 门不会
  发现陈旧的版本号——grep 才是检查。
- **不要为了让页面匹配而重建已发布的版本。** 如果 SHA 变了，那是一个新的补丁版本。

## 6. 镜像

GitHub 是源站；慢网络下同一路径后缀由镜像提供：

```bash
VER=0.2.0
BASE=https://github.com/jixoai/keel/releases/download/v$VER
MIRROR=https://gh.fde2e.tweb.xin/jixoai/keel/releases/download/v$VER

# 1. 镜像的校验和文件必须与源站逐字节一致
curl -fsSL "$BASE/SHA256SUMS.txt" -o sums.origin
curl -fsSL "$MIRROR/SHA256SUMS.txt" -o sums.mirror
cmp sums.origin sums.mirror

# 2. 产物必须对得上这份校验和文件
curl -fsSL "$MIRROR/keel-web-$VER.zip" -o "keel-web-$VER.zip"
shasum -a 256 -c sums.mirror --ignore-missing
```

- `https://gh.fde2e.tweb.xin/`——自建镜像，国内首选；把 GitHub 域名换掉，路径其余部分不变。
- `https://ghfast.top/https://github.com/…`——公共代理兜底；规则相同。
- 聚合仓在 `jixoai/fde2e-releases`，tag `keel-v<ver>`（`.github/workflows/release.yml:138`），带同一份
  `SHA256SUMS.txt`。

校验过的用户即使在镜像上也是安全的：文件按哈希校验，而不是按信任。

## 7. 升级运行中的部署

升级原语是**换班**，不是热拔插：先把旧进程排水，再启动新二进制。新旧写者绝不能并存，因为旧写路径
不经过 fencing 检查（`docs/HANDOFF.md:24`）。

```bash
# 1. 用同一个 DSN 启动新 runtime（KEEL_RUNTIME_ID 必填）
# 2. 让旧进程进入 handoff 排水
curl -sS -X POST "$OLD/v1/runtime/drain" \
  -H "Authorization: Bearer $KEEL_ADMIN_KEY" \
  -d '{"mode":"handoff","deadlineMs":30000}'

# 3. 观察它停止领取、在途 run 排空
curl -sS "$OLD/v1/runtime/status" -H "Authorization: Bearer $KEEL_ADMIN_KEY"
```

handoff 期间旧进程停止领取新 run，但继续接受 edge 输入；在途 run 排水到提交边界，未提交的 partial
（半条流式消息、执行中的工具）在截止时丢弃（`docs/HANDOFF.md:78`）。新 owner 以 `epoch+1` 接班并
从账本重建；SSE 客户端带 `Last-Event-ID` 重连，首帧 = 游标+1。

SIGTERM 是 **shutdown** 排水，不是 handoff：它拒绝新写、放弃在途 run（不 Finish 也不 release），
因此新进程可能等一个有界租约 TTL（`docs/HANDOFF.md:83`）。

## 8. 回滚

回滚是同一套协议的逆过程（`docs/HANDOFF.md:39`）：

1. **先排水**——handoff 模式或 SIGTERM；确认 `claiming=false` 且 `inFlightRuns=0`，或等截止。
2. **停新进程、起旧二进制**，同一个 DSN。旧二进制能容忍新列，因为迁移是增量的。
3. **绝不让两个写者并存。** 回滚与升级遵守同一条铁律。
4. **不回滚数据。** 账本只追加；旧读取器会跳过未知事件 kind，客户端可能要自行过滤。
5. **预期一个有界窗口**：旧式 `Finish` 不清 `sessions.lease_until`，某会话可能一个 TTL 内领不到；
   `KEEL_LEASE_MS` 可调小（`docs/HANDOFF.md:51`）。

至于发布本身：已公布的 tag 是不可变的。工作流的上传步骤用 `--clobber`（`.github/workflows/release.yml:126`），那是
修复一次半途失败的重跑，不是用来改用户已经校验过的字节。修复请发新的补丁版本。

## 9. 校验一次下载

```bash
VER=0.2.0
curl -LO "https://github.com/jixoai/keel/releases/download/v$VER/SHA256SUMS.txt"
curl -LO "https://github.com/jixoai/keel/releases/download/v$VER/keel-$VER-linux-amd64.tar.gz"
shasum -a 256 -c SHA256SUMS.txt --ignore-missing
tar -xzf "keel-$VER-linux-amd64.tar.gz" && ./keel version
```

最后一行必须打印 tag 所指的同一个版本——这就是版本门的不变式，在用户机器上再验一次。
