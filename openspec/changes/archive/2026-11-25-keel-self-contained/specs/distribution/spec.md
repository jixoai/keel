## ADDED Requirements

### Requirement: 单体二进制分发

每个平台 SHALL 提供一个单体 `keel` 二进制，内嵌该平台的 PostgreSQL 18 运行树、web 客户端
与图标/README；安装或首次启动 MUST 从内嵌资产解压，MUST NOT 要求用户另行安装数据库、
设置 `KEEL_DSN` 或运行 Docker。

`keel start` MUST 用内嵌 PostgreSQL 自动初始化本地库（仅回环）、以 `auth=local-only`
启动 `keel serve` 并打开浏览器；`--no-open` MUST 只跳过浏览器。

`keel install` MUST 把单体二进制注册为平台应用：macOS 生成 `.app`（含 `keel` scheme 与
ad-hoc 签名，不公证）、Linux 生成 `.desktop`/`x-scheme-handler=keel`、Windows 生成开始菜单
快捷方式与 HKCU `keel` scheme。`keel uninstall` MUST 可移除注册；用户数据默认保留，
只有 `--purge` 才删除。

内嵌资产缺失（开发构建）时 MUST 回退到外部 PostgreSQL/web，并给出可读提示；Docker 变体
MAY 使用 PG 容器而不内嵌。

#### Scenario: 下载后一键启动
- **WHEN** 用户下载平台单体二进制并运行 `./keel start`
- **THEN** 自动解压运行时、初始化本地库、启动服务并打开浏览器；无需任何环境变量

#### Scenario: 生成 macOS 应用
- **WHEN** 用户运行 `./keel install`
- **THEN** `~/Applications/Keel.app` 生成并注册 `keel` scheme，可双击启动；不触发公证
