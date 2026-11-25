# Proposal: keel-client-updater

> 立项：2026-11-25（Owner：updater 不必依赖签名，reply9842 也没签名）。
> 收回「updater 需要签名环境」的判断：自更新只要 HTTPS + 发布校验和 + 原子替换即可先落地；
> 代码签名是后续增强，不是前置条件。

## Why

P2 还差 updater。单体二进制已经自带版本与平台资产，更新只需要：查 latest release →
下载对应平台归档 → 校验 SHA256SUMS → 替换自身/暂存。无需 macOS 公证或 Windows 代码签名。

## What Changes

1. `keel update [--check] [--force] [--manifest URL]`：
   - 查询 GitHub latest release（可被 `--manifest`/`KEEL_UPDATE_MANIFEST` 覆盖）；
   - 按 `keel-<ver>-<os>-<arch>` 选资产，下载归档与 `SHA256SUMS.txt`，校验 SHA-256；
   - 解出 `keel`/`keel.exe`，校验可执行/版本后原子替换当前二进制；
   - 已是最新版则只报告；`--force` 允许重装同版本。
2. **无签名依赖**：不要求 macOS 公证或 Windows 证书；macOS `.app` 内替换后重新 ad-hoc 签名，
   Windows 用退出后执行的小助手替换运行中的 exe。
3. 安全边界：只走 HTTPS（自定义 manifest 可显式 http 但提示）；校验和来自同一 release；
   不自动重启服务，提示用户重启/下次启动生效。
4. 文档/测试：README 加 `keel update`；单测版本比较/资产名/校验和/tar 提取；macOS 实机
   `--check` 与 `--force` 实测。

## Impact

- 代码：`cmd/keel/update.go`、`main.go` 注册；Windows helper 脚本生成。
- 不依赖签名/公证；后续可加 minisign/cosign 或 OTA 包，不改变命令面。
