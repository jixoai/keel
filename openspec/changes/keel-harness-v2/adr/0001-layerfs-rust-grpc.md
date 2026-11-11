# ADR 0001 — layerfs 采用 Rust + gRPC 独立服务

- 状态：Accepted（Owner 裁决 2026-11-11）
- 决策者：Owner
- 影响面：`layerfs`、`keel.fs`、`keel.shell`、`keel.exec-surface`、部署与验收

## 背景

`docs/BLOB-STORAGE-RESEARCH.md`（2026-11-05）的书面结论是：**"本地 CAS + 页缓存 + PG 元数据
已是最优；不引入专用格式/引擎"**，并逐项论证了额外进程与跳数没有收益。v1 的实现也据此采用
`internal/blob`（PG 分块 + 本地 CAS）。

本 change 的 Owner 指示是：**用 Rust 开发 layerfs + gRPC，为 file/bash 插件提供文件系统能力**。
这是一次明确的**推翻**，必须留下 ADR，不能当作没发生。

## 为什么这次的约束变了（不是"重做一遍"）

旧结论比较的是**存字节的引擎**；v2 新增了四项旧结论没有覆盖的约束：

1. **tree object 与 O(1) fork**：目录树按内容寻址，fork 以 `(session_id, eventSeq)` 的
   tree hash 继承，不复制 manifest 行与字节。
2. **跨进程共享**：一个 Group 一个 worker，多个 worker 与 layerfs 需要共享同一份字节池；
   v1 的进程内 CAS 无法跨进程去重。
3. **配额与 GC**：按 group/租户的存储配额、引用计数与 GC，需要在进程外统一执行。
4. **内存硬预算**：后端 300MB 目标 / 500MB 上限，字节缓存必须与 Harness 堆隔离并可独立限流。

## 决策

- `layerfs` 以 **Rust 独立服务**实现，gRPC 流式接口：`Put/Get/Stat/RefCount/GC`（file object
  与 tree object）。
- Harness 侧只做 manifest（路径 → hash）与配额前置检查；layerfs **不认识路径、会话、任务**。
- 服务随 Harness 部署（同机），端口 MUST 在 13610–13799 段内并登记 `.agents/docs/PORT.md`。

## 后果（必须进 tasks）

- Rust 工具链进 CI；`Dockerfile` / `docker-compose.yml` 增服务；健康检查与就绪探针。
- 进程监督与回收（含 SIGKILL 后的 sweep）；端口登记与防火墙核对。
- 内存记账：layerfs 的 RSS 必须纳入后端总和（G4）。
- 协议版本化与向后兼容；错误码 `quota_exceeded / not_found / io / unavailable`。
- 回归路径：若基准证明无收益或突破预算，**回退为进程内 Go CAS（`internal/blob`）**，
  并以此 ADR 的反向记录为准；不得两套并存。

## 证据门（不达标即回退）

- 1000 并发流（读写混合）下的 RSS、p99 延迟、GC 停顿，与进程内 Go CAS 基线对照；
- 十万文件目录的 tree 去重收益（manifest 行数、字节数）；
- fork 100 次的继承成本（应为 O(1) 引用）。
- 以上证据进 G4 与 G10 报告；缺一项不得宣布"已达标"。
