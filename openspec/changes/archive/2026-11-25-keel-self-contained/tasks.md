# Tasks: keel-self-contained

## 1. 资产与运行时

- [x] 1.1 `cmd/keel/assets.go` + `assets/README.txt` 占位；`go:embed assets`（pg.txz/web/icon）。
- [x] 1.2 `ensureRuntime`：解压 `pg.txz`（纯 Go xz+tar，路径安全）与 `web/` 到
      `$KEEL_HOME/runtime/<version>`；版本/校验标记；已存在则复用。
- [x] 1.3 `pgTool` 查找顺序：`KEEL_PG_DIR` → `.app/Contents/Resources/pg/bin` →
      可执行文件旁 `pg/bin` → runtime `pg/bin` → PATH；`keel serve` 的 web dir 同理。

## 2. 命令

- [x] 2.1 `keel start`：确保 runtime → 自动 DB（bundled PG）→ `serve --auth local-only`
      → 打开浏览器；`--no-open`；Ctrl-C 停服；数据保留。
- [x] 2.2 `keel install` macOS：生成 `~/Applications/Keel.app`（Info.plist、keel scheme、
      Resources pg/web、ad-hoc codesign、去 quarantine），`open`；`--dir/--no-open/--force`。
- [x] 2.3 `keel uninstall`：移除 app；`--purge` 删 runtime/data；默认保留数据。
- [x] 2.4 Linux/Windows 的 `keel start` 走同一内嵌运行时；桌面注册本版仅 macOS（边界）。
- [x] 2.5 单测：pg 查找顺序、install 路径/Info.plist、非法资产回退。

## 3. 构建与验证

- [x] 3.1 `scripts/package-self-contained.sh`：下载 darwin-arm64/v8 18.7 zonky 包 →
      `assets/pg.txz`；复制 web dist；构建单体二进制；报告大小。
- [x] 3.2 干净环境实测：`PATH=/usr/bin:/bin`、无 `KEEL_DSN`、无外部 PG →
      `./keel start --no-open` health 200；`ps` 确认 postgres 来自 runtime；
      `./keel install --dir /tmp/KeelApps --no-open` 生成 .app/Info.plist。
- [x] 3.3 README/官网首页/下载页/发布文档改「单文件 + `./keel install/start`」；Docker 变体保留 compose。
- [x] 3.4 release workflow 矩阵（macOS/Linux/Windows + 架构）与 SHA/大小；报告/归档。

## 证据

- `evidence/macos-self-contained.txt`：121M 单体二进制；干净 PATH/无 DSN 下 `start` health 200，
  runtime 内 `pg/bin/postgres` 启动；`install` 生成 0.3.0 `.app`（ad-hoc 签名、keel scheme），
  `open` 后 health 200；`uninstall` 移除应用保留数据。
- `go vet`/`go test ./cmd/keel`；站点构建 37 对/3312 链接/1466 引用。
- 边界：Linux/Windows 本机未实跑；桌面注册本版仅 macOS；二进制未签名，`.app` 不公证；
  文件关联/updater/assets/tools 未做。
