# Wave 2 A 轴安全复核与处置（2026-11-19）

对象：冻结快照 `06e3747`（净变更 `2b26a6b..06e3747`，约 21k 行新增 Go）。复核：独立 reviewer
子代理（只读，探针全部在 /tmp 且已删除；自跑 `-race` 10 包 + PG 集成 + 两轮沙箱逃逸探针）。
**结论：8.5/10，P0=0、P1=0。**

## 结论

九个审查面全走读 + 关键路径实测，未发现：跨租户数据可达、宿主文件系统逃逸、审批自答、
工具面越权。B1（崩溃重放走本 attempt 过滤注册表）与 B2（外部事件只允许 ClassEdge）回归均在位。

关键实证：沙箱内 `/etc/passwd`、`/proc/self`、`curl/wget/nc/node/python3` 均不可达；宿主 env
零泄漏；`/bin/echo` 是 gbash 虚拟 stub；`../` 与绝对路径写入只落 gbash 内存 base；符号链接穿越
被 `SymlinkDeny` 拒绝；作业 id 为不可猜 uuid v4，全部查询带租户谓词；通知 token 256-bit、
只存 SHA-256、单次使用、常数时间比较；7 张新表全部 ENABLE+FORCE RLS，跨租户读为零行。

## 非阻塞与处置

| # | 建议（reviewer 原话摘要） | 处置 |
|---|---|---|
| 1 | spawn 子代理的 `selection` 未过与 `/input`、`/events` 相同的 `ValidateSelection` 提交门；同租户可存入永远无法执行的 selection，每轮 attempt 提交前失败（违反「MUST 在提交处拒绝」不变量，限本租户自伤） | **本轮修复**（A 轴清理切片）：spawn 处用同一 seam 校验，或仅接受 `agentId` |
| 2 | 模型可用 `notify {action:"ack"}` 压制自己的 fallback push（actor 可识别为 `agent:<runID>`） | 本轮裁决：先查 spec 的 ack 语义；若 ack 是消费器动作，则模型侧排除、API 保留 |
| 3 | `Service.tokens` 仅在 `finishPush` 清理，API/tool ack 路径不清；notifications 表无租户配额 | **本轮修复 token 清理**；配额记 backlog（需配额模型） |
| 4 | MCP image 内容无字节上限（只截文本 20KiB） | **本轮修复**：image 块加字节上限 |
| 5 | `Materialize` 无 `O_NOFOLLOW`/`EvalSymlinks` 守卫（当前生产未启用 CaptureRoot） | backlog：fork/materialize 生产接线前必须补 |
| 6 | `shellScope` 无 run scope 时回落 boot 租户（当前不可达，多租户下未来调用方会错记租户） | backlog：多租户 pack 前改为类型化拒绝 |
| 7 | `mcp.call` 的 `arguments` 非对象也透传（schema 声明 object） | **本轮修复**：工具层类型校验，非对象返回 `invalid_arguments` |
| 8 | `X-Keel-Actor` 可被同租户凭证伪造（属信任模型内） | backlog：文档/audit UI 标注「租户自证」，不作授权依据 |

## 复核证据

- `go test -race -count=1`：toolsurface / bashbridge / layerfs(+gbashfs) / fileplugin / notify /
  approval / tasklog / agentext / jobqueue / agent 全 PASS。
- `KEEL_TEST_DSN_W2_RVA` 真库：notify / approval / fileplugin / jobqueue / store 全 PASS
  （含 RLS 断言与跨租户零行）。
- 两轮沙箱探针（19+9 条命令）：宿主零残留；本会话未启动常驻进程。

## 说明

本复核基于 `06e3747`，早于 seam 合入（`de680cf`）。seam 新增的 `fs.capture` 系统通道与沙箱作业
视图属于新攻击面，由 W2.jobcap 切片实现后另跑一次 A 轴增量复核（系统通道只能由 harness 内部
触达、客户端事件门仍只放 ClassEdge）。
