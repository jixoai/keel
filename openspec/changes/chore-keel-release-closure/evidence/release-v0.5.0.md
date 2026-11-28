# v0.5.0 release closure — facts

> 记录时间：2026-11-28（采集自 GitHub tag / Release、Release workflow、官网与
> `SHA256SUMS.txt`）。本文件只记录事实；tag 与资产保持发布时字节，不重建。

## Tag and commit

| 项 | 值 |
|---|---|
| tag | `v0.5.0`（lightweight tag） |
| commit | `d0184d02e40559e09ed1a27d697e6b6a705b1735` |
| commit 时间 | 2026-11-28 11:25:22 +0800 |
| commit 标题 | `test(board): wait for the post-response owner grant` |
| 远端 tag | `refs/tags/v0.5.0` → `d0184d02e40559e09ed1a27d697e6b6a705b1735` |

## Release

| 项 | 值 |
|---|---|
| Release 页 | https://github.com/jixoai/keel/releases/tag/v0.5.0 |
| 标题 | `keel 0.5.0` |
| 状态 | published（非 draft / 非 prerelease），2026-11-28T03:51:04Z |
| 发布工作流 | https://github.com/jixoai/keel/actions/runs/46103711132（conclusion=success） |
| 聚合仓镜像 | `jixoai/fde2e-releases` 无 `keel-v0.5.0` release（`gh release view` → release not found） |

## Assets and SHA-256

`SHA256SUMS.txt` 自身 SHA-256：`063312d97c46c6c4b8d2c493dd75dc0ba0c872f3c7758df68a2f733e8de2c429`

| 资产 | 大小 (bytes) | SHA-256 |
|---|---:|---|
| `keel-0.5.0-darwin-amd64.tar.gz` | 104930721 | `1b0d8dbbb1c25c11c54710c25d52ade1ff35b02bfac9ea6ed8209735660c472b` |
| `keel-0.5.0-darwin-arm64.tar.gz` | 104487654 | `49999b7f0f8f981d5ddd6b04510fac491e095279c9c064575616a6d563b1f6cd` |
| `keel-0.5.0-linux-amd64.tar.gz` | 48504162 | `7c75583f0c6f9941476354a82f578f2216506e0c723b4eb8e9d776ba5374d2da` |
| `keel-0.5.0-linux-arm64.tar.gz` | 46048059 | `8925b7793f1e587f7f1f76e5c1ed0ee61f824db23835a4397c508c1c9de447ae` |
| `keel-0.5.0-windows-amd64.zip` | 59205956 | `fae44f862ea83e71b0d45f5bf69ae0c019f9c0526272f1dee76c066db39ff943` |
| `keel-web-0.5.0.zip` | 18632433 | `74434142d3d5908935827bf2534b1194d1249a70dca6042b1a94634fc93e08d5` |

GitHub Release 的 asset digest 与上表逐项一致（2026-11-28 `gh release view` 采集）。

## Verification commands

```bash
VER=0.5.0
curl -fsSL "https://github.com/jixoai/keel/releases/download/v$VER/SHA256SUMS.txt" -o SHA256SUMS.txt
shasum -a 256 -c SHA256SUMS.txt --ignore-missing   # 下载对应资产后执行
```

## Official download page

| 面 | URL | 探测 |
|---|---|---|
| EN 下载页 | https://keel.jixoai.com/downloads | HTTP 200（2026-11-28） |
| ZH 下载页 | https://keel.jixoai.com/zh/downloads | HTTP 200（2026-11-28） |
| 站点根 | https://keel.jixoai.com/ | HTTP 200（2026-11-28） |

页面版本为 `v0.5.0`，五平台链接与 `SHA256SUMS.txt` 链接均指向本仓 Release。

## Known external items

1. **Windows 未实机验证**：CI 构建并打包 `keel-0.5.0-windows-amd64.zip`，但本版没有
   Windows 机器上的启动/安装实机证据；不得对外写成“Windows 已验证”。
2. **二进制未代码签名**：macOS 下载物可能被 Gatekeeper 隔离；`.app` 仅本机 ad-hoc 签名，
   未公证（与 Release notes 一致）。
3. **镜像探测**：2026-11-28 本机探测
   `https://gh.fde2e.tweb.xin/jixoai/keel/releases/download/v0.5.0/SHA256SUMS.txt`
   返回 HTTP 400；`https://ghfast.top/...` 在本机 TLS 失败。两者都是外部/网络相关项，
   未据此修改文档；源站 GitHub 下载与校验已通过。
4. **聚合仓不再镜像**：从本 change 起 release workflow 不再有 `FDE2E_RELEASES_TOKEN`
   分支；`jixoai/fde2e-releases` 中历史 `keel-v*` 发布不由本仓维护或删除。
5. **官网/文档站部署**：本站点由 `site.yml` 独立构建部署；v0.5.0 切版 commit
   `2c1245e9` 已包含下载页与版本引用，本次发布收尾不再重跑站点构建（避免无意义重部署）。
