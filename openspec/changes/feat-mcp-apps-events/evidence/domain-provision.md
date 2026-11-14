# mcp-app 双 origin 沙箱域开通证据（2026-11-14）

目的：为 MCP Apps 提供 `<appId>.mcp-app.fde2e.com` 沙箱子域（不同 origin 隔离要求）。
域形态：`mcp-app.fde2e.com`（host/网关面）+ `*.mcp-app.fde2e.com`（每 App 沙箱子域）。

## 变更清单（服务器 + 本机各一处）

| 项 | 内容 |
|---|---|
| DNS | **零改动**：既有 `* A ${SERVER_IP}` 泛解析已覆盖二级通配（服务器侧 `getent` 实测 `a.b.mcp-app`、`deep.test.mcp-app` 均解析） |
| 证书 | acme.sh DNS-01（`dns_ali`，凭据已存 account.conf）：`mcp-app.fde2e.com` + `*.mcp-app.fde2e.com` 一体签发，LE ec-256；`--install-cert` 落盘 `${SITE_ROOT_DIR}/sites/mcp-app.fde2e.com/ssl/{fullchain,privkey}.pem`，reloadcmd（容器 `nginx -s reload`）已挂——cron 自动续期 |
| vhost | `conf.d/mcp-app.fde2e.com.conf`：单块 `listen 80 + 443 ssl http2`，`$scheme=http` 301；`proxy_pass 127.0.0.1:13601`（frps vhost，Host 透传）；WS/SSE 就绪（Upgrade 映射、`proxy_buffering off`、3600s 超时）。与 alpha 泛域（负向前瞻正则）无冲突：exact/通配 server_name 优先级恒高于正则 |
| 隧道 | 本机 frpc 手写片段 `~/.config/frp/conf.d/mcp-app-fde2e.toml`（pin；`customDomains=[mcp-app.fde2e.com, *.mcp-app.fde2e.com]` → `127.0.0.1:13760` keel serve）；`frpc verify` + `reload` 热载（其它域名零中断） |

激活时序：服务器 `nginx -t` 通过 → reload OK；frps 在线表 `mcp-app.fde2e.com:13601, *.mcp-app.fde2e.com:13601 running`。

## 验收（全链）

服务器侧（`curl -sk --resolve <domain>:443:127.0.0.1` 回环）：

```
# 443 apex
curl -sk --resolve mcp-app.fde2e.com:443:127.0.0.1 https://mcp-app.fde2e.com/
→ 200 text/html（Keel SPA，8066B）
# 443 通配子域
curl -sk --resolve demo.mcp-app.fde2e.com:443:127.0.0.1 https://demo.mcp-app.fde2e.com/
→ 200（8066B，同件）
# 80 → 443
curl -s --resolve mcp-app.fde2e.com:80:127.0.0.1 http://mcp-app.fde2e.com/
→ 301 https://mcp-app.fde2e.com/
# 证书 SAN（SNI=demo.mcp-app.fde2e.com）
X509v3 Subject Alternative Name: DNS:*.mcp-app.fde2e.com, DNS:mcp-app.fde2e.com
```

本机（公网直连，`--noproxy '*'`，Owner 机 DIRECT 路由）：apex 200 · 通配子域 200。
浏览器（ZCode 内置）：`https://mcp-app.fde2e.com/` 完整渲染 Keel 登录面（title=Keel，服务口令门）。

## 后续

- P2.4 起 keel 在此域发布 app view（CSP/`frame-ancestors` 按 spec）；生产部署时 keel 正式位接管同域（dev 期由 frpc 片段指向本机 keel）。
- 注册面/工具面迁移按 Alpha 零兼容裁决推进（见 `docs/RESIDUAL-PLAN.md` §5 裁决 9）。
