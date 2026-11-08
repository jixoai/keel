// The fixture transport: a local, scripted keel service so the UI runs before
// `@keel/chat-surface` and `keel serve` are wired. It implements the same
// `EventSource` / `SurfaceCommands` / session-list surface as `KeelHttp`, so
// switching modes changes one object, not the UI.
//
// The scripts reproduce the shapes the kernel writes (user.message with
// delivery, assistant.message with inputSeqs/toolCall blocks, tool.result,
// compaction, decision, run.finished) and the queue/guide rules: a guide sent
// to an idle session is queued; a guide sent while running joins the turn and
// is consumed by the next assistant message; stop leaves queued input alone.
import type {
  AttachmentRef,
  ConnectionState,
  Delivery,
  EventSource,
  SessionSummary,
  UiCommands,
  WireEvent,
} from "./contract";
import demoShot from "../../assets/demo-shot.png";

type Subscriber = {
  cursor: number;
  partials: boolean;
  onEvent: (event: WireEvent) => void;
  onPartial?: (p: { text: string; turnId?: string }) => void;
  onState: (s: ConnectionState["status"]) => void;
};

type QueuedInput = { seq: number; requestId: string; text: string; delivery: Delivery };

type Peer = {
  id: string;
  meta: SessionSummary;
  events: WireEvent[];
  nextSeq: number;
  running: boolean;
  queue: QueuedInput[];
  timers: number[];
  subscribers: Subscriber[];
  offline: boolean;
  drainPaused: boolean;
};

const nowISO = () => new Date().toISOString();
const delay = (ms: number) => new Promise<void>((resolve) => setTimeout(resolve, ms));

function textPreview(payload: unknown): string {
  if (!payload || typeof payload !== "object") return "";
  const row = payload as { text?: string; summary?: string; question?: string };
  const text = row.text ?? row.summary ?? row.question ?? "";
  return text.replace(/\s+/g, " ").slice(0, 90);
}

export class FixtureClient {
  private sessions = new Map<string, Peer>();
  private sequence = 0;

  constructor(private readonly imageURL: string = demoShot.url) {
    this.seed();
  }

  async probe(): Promise<"ok" | "auth" | "down"> {
    return "ok";
  }

  async capabilities(): Promise<Record<string, unknown>> {
    return {
      version: "fixture",
      capabilities: { "sessions.list": true, decisions: true, partials: true },
    };
  }

  async listSessions(): Promise<SessionSummary[]> {
    return [...this.sessions.values()]
      .map((peer) => ({ ...peer.meta }))
      .sort((a, b) => (b.lastMessageAt ?? "").localeCompare(a.lastMessageAt ?? ""));
  }

  async createSession(): Promise<string> {
    const id = `demo-${++this.sequence}`;
    const peer = this.peer(id, "新会话");
    this.sessions.set(id, peer);
    return id;
  }

  async deleteSession(sessionId: string): Promise<void> {
    const peer = this.sessions.get(sessionId);
    if (peer) this.clearTimers(peer);
    this.sessions.delete(sessionId);
  }

  source(sessionId: string): EventSource {
    const peer = this.need(sessionId);
    return {
      list: async (after: number, limit = 500) =>
        peer.events.filter((event) => event.seq > after).slice(0, limit),
      stream: (opts) => this.stream(peer, opts),
    };
  }

  commands(sessionId: string): UiCommands {
    const peer = this.need(sessionId);
    return {
      send: (text, opts) => this.send(peer, text, opts.delivery, opts.requestId),
      sendWithAttachments: (text, opts) =>
        this.send(peer, text, opts.delivery, opts.requestId, opts.attachments),
      stop: async () => this.stop(peer),
      answerDecision: async (decisionId, answer) => this.answer(peer, decisionId, answer),
      upload: async () => ({ fileId: `fixture-file-${++this.sequence}` }),
    };
  }

  /** The QA hook behind the sidebar's connection demo: events keep arriving
   * while the stream is down; reconnect flushes them in order, once. */
  simulateDrop(sessionId: string, ms = 2600): void {
    const peer = this.sessions.get(sessionId);
    if (!peer) return;
    peer.offline = true;
    for (const sub of peer.subscribers) sub.onState("reconnecting");
    const timer = setTimeout(() => {
      this.append(peer, "decision", {
        decisionId: "dec-reconnect",
        question: "网络抖动期间到达的审批：是否保留当前草稿？",
        options: [
          { optionId: "keep", label: "保留草稿", description: "继续编辑，不丢失内容" },
          { optionId: "discard", label: "放弃草稿" },
        ],
        recommendation: "keep",
        status: "open",
      });
    }, 700) as unknown as number;
    peer.timers.push(timer);
    const back = setTimeout(() => {
      peer.offline = false;
      for (const sub of peer.subscribers) sub.onState("open");
      this.flush(peer);
    }, ms) as unknown as number;
    peer.timers.push(back);
  }

  // ---- seeds ------------------------------------------------------------

  private peer(id: string, title: string, runStatus = "done"): Peer {
    return {
      id,
      meta: {
        sessionId: id,
        title,
        lastMessage: "",
        lastSeq: 0,
        runStatus,
        createdAt: nowISO(),
        updatedAt: nowISO(),
        lastMessageAt: nowISO(),
      },
      events: [],
      nextSeq: 1,
      running: false,
      queue: [],
      timers: [],
      subscribers: [],
      offline: false,
      drainPaused: false,
    };
  }

  private seed() {
    const base = Date.now();
    this.add("demo-chat", "发布准备周报", [
      { kind: "user.message", payload: { text: "帮我总结一下这周的发布准备情况。", requestId: "seed-chat-1", delivery: "queue", timestamp: base - 540_000 } },
      {
        kind: "assistant.message",
        payload: {
          role: "assistant",
          content: [
            { type: "thinking", thinking: "先看发布清单，再确认验收与剩余项。" },
            {
              type: "text",
              text: "本周发布准备已基本就绪：\n\n1. 构建与签名流程已跑通，web 产物可同源托管；\n2. 端到端验收覆盖了会话、审批、工具卡与断线重连；\n3. 还差一项——发布说明需要补齐关键截图。\n\n建议今天完成截图与说明，明天进入发布窗口。",
            },
          ],
          inputSeqs: [1],
          stopReason: "end_turn",
          timestamp: base - 532_000,
        },
      },
      {
        kind: "run.finished",
        payload: { turns: 1, tokensIn: 1820, tokensOut: 364, stopReason: "done", durationMs: 6400, timestamp: base - 531_000 },
      },
    ]);

    this.add("demo-tools", "并行工具检查", [
      { kind: "user.message", payload: { text: "检查 api、worker、web 三个服务的健康状态。", requestId: "seed-tools-1", delivery: "queue", timestamp: base - 1_500_000 } },
      {
        kind: "assistant.message",
        payload: {
          role: "assistant",
          content: [
            { type: "thinking", thinking: "三个检查彼此独立，可以并行执行。" },
            { type: "toolCall", id: "call-api", name: "health.check", arguments: { service: "api", timeoutMs: 2000 } },
            { type: "toolCall", id: "call-worker", name: "health.check", arguments: { service: "worker", timeoutMs: 2000 } },
            { type: "toolCall", id: "call-web", name: "health.check", arguments: { service: "web", timeoutMs: 2000 } },
          ],
          inputSeqs: [1],
          stopReason: "tool_use",
          timestamp: base - 1_494_000,
        },
      },
      { kind: "tool.result", payload: { toolCallId: "call-api", toolName: "health.check", content: [{ type: "text", text: "status=ok version=0.1.0 p95=42ms" }], isError: false, durationMs: 320, timestamp: base - 1_492_000 } },
      { kind: "tool.result", payload: { toolCallId: "call-worker", toolName: "health.check", content: [{ type: "text", text: "status=ok queue=0 workers=3" }], isError: false, durationMs: 410, timestamp: base - 1_491_500 } },
      { kind: "tool.result", payload: { toolCallId: "call-web", toolName: "health.check", content: [{ type: "text", text: "status=degraded upstream=503 retry=2" }], isError: true, durationMs: 280, timestamp: base - 1_491_000 } },
      {
        kind: "assistant.message",
        payload: {
          role: "assistant",
          content: [{ type: "text", text: "api 与 worker 正常；web 上游返回 503（已重试 2 次），建议先切到只读降级页。" }],
          inputSeqs: [1],
          stopReason: "end_turn",
          timestamp: base - 1_489_000,
        },
      },
      {
        kind: "run.finished",
        payload: { turns: 1, tokensIn: 2210, tokensOut: 268, stopReason: "done", durationMs: 5100, timestamp: base - 1_488_000 },
      },
    ]);

    this.add("demo-approval", "发布审批", [
      {
        kind: "user.message",
        payload: {
          text: "把 0.1.0 发布到生产环境，并附上截图。",
          requestId: "seed-approval-1",
          delivery: "queue",
          timestamp: base - 240_000,
          attachments: [
            { name: "demo-shot.png", mime: "image/png", size: 39037, url: this.imageURL },
          ],
        },
      },
      {
        kind: "assistant.message",
        payload: {
          role: "assistant",
          content: [
            { type: "thinking", thinking: "发布是不可逆动作，需要人工确认窗口。" },
            { type: "text", text: "发布包已构建并签名，准备进入生产窗口。需要你确认发布时间。" },
            { type: "toolCall", id: "call-deploy", name: "deploy.prepare", arguments: { version: "0.1.0", channel: "production" } },
          ],
          inputSeqs: [1],
          stopReason: "tool_use",
          timestamp: base - 234_000,
        },
      },
      { kind: "tool.result", payload: { toolCallId: "call-deploy", toolName: "deploy.prepare", content: [{ type: "text", text: "artifact=keel-0.1.0.nshpkg sha256=9f3c…c21a" }], isError: false, durationMs: 2400, timestamp: base - 230_000 } },
      {
        kind: "decision",
        payload: {
          decisionId: "dec-release",
          question: "是否在 22:00–23:00 窗口发布 Keel 0.1.0？",
          options: [
            { optionId: "approve", label: "批准发布", description: "立即进入发布窗口，发布后自动跑冒烟" },
            { optionId: "hold", label: "暂缓", description: "保持当前版本，明天再评估" },
            { optionId: "notify", label: "先通知团队", description: "发送发布预告，收集意见后再决定" },
          ],
          recommendation: "approve",
          status: "open",
          timeoutAt: base + 120_000,
          timestamp: base - 229_000,
        },
      },
    ]);

    this.add("demo-guide", "会议纪要整理", [
      { kind: "user.message", payload: { text: "整理这周的会议纪要，按主题归档。", requestId: "seed-guide-1", delivery: "queue", timestamp: base - 900_000 } },
      {
        kind: "assistant.message",
        payload: {
          role: "assistant",
          content: [
            { type: "thinking", thinking: "先读日历与录音，再按主题聚类。" },
            { type: "text", text: "好的，我先读取日历和录音文件，再把纪要按主题归档。" },
            { type: "toolCall", id: "call-calendar", name: "calendar.read", arguments: { range: "this-week" } },
            { type: "toolCall", id: "call-transcripts", name: "transcript.load", arguments: { source: "meeting-notes" } },
          ],
          inputSeqs: [1],
          stopReason: "tool_use",
          timestamp: base - 894_000,
        },
      },
      { kind: "tool.result", payload: { toolCallId: "call-calendar", toolName: "calendar.read", content: [{ type: "text", text: "events=7" }], isError: false, durationMs: 180, timestamp: base - 893_000 } },
      {
        kind: "user.message",
        payload: { text: "另外，把行动项按负责人分组。", requestId: "seed-guide-2", delivery: "guide", timestamp: base - 890_000 },
      },
    ]);

    this.add("demo-compact", "重构方案续传", [
      {
        kind: "compaction",
        payload: {
          summary: "此前 42 条消息已压缩：重构方案确定采用无状态投影；迁移分两步；主要风险是事件序号乱序与断线缺口。",
          manual: true,
          tokensBefore: 48000,
          timestamp: base - 2_000_000,
        },
      },
      { kind: "user.message", payload: { text: "继续上一轮的重构方案，先做第一步。", requestId: "seed-compact-1", delivery: "queue", timestamp: base - 1_990_000 } },
      {
        kind: "assistant.message",
        payload: {
          role: "assistant",
          content: [
            { type: "thinking", thinking: "从压缩摘要恢复上下文，确认第一步是无状态投影。" },
            { type: "text", text: "从压缩摘要恢复：第一步实现无状态投影（事件 → Turn 视图），保持零 DOM、可单测；第二步再接渲染器。现在先落地第一步。" },
          ],
          inputSeqs: [2],
          stopReason: "end_turn",
          timestamp: base - 1_984_000,
        },
      },
      {
        kind: "run.finished",
        payload: { turns: 1, tokensIn: 980, tokensOut: 210, stopReason: "done", durationMs: 3200, timestamp: base - 1_983_000 },
      },
    ]);

    this.add("demo-failed", "生成发布说明", [
      { kind: "user.message", payload: { text: "调用模型生成发布说明。", requestId: "seed-failed-1", delivery: "queue", timestamp: base - 3_600_000 } },
      {
        kind: "assistant.message",
        payload: {
          role: "assistant",
          content: [],
          inputSeqs: [1],
          stopReason: "error",
          errorMessage: "upstream 429: rate limit exceeded (retry-after 30s)",
          timestamp: base - 3_594_000,
        },
      },
      {
        kind: "run.finished",
        payload: { turns: 1, tokensIn: 0, tokensOut: 0, stopReason: "failed", durationMs: 1200, timestamp: base - 3_593_000 },
      },
    ], "failed");

    this.add("demo-empty", "新会话", []);
  }

  private add(id: string, title: string, events: Array<{ kind: string; payload: unknown }>, runStatus = "done") {
    const peer = this.peer(id, title, runStatus);
    for (const event of events) {
      peer.events.push({ seq: peer.nextSeq++, kind: event.kind, payload: event.payload });
    }
    const last = peer.events[peer.events.length - 1];
    peer.meta.lastSeq = last?.seq ?? 0;
    const lastMessage = [...peer.events]
      .reverse()
      .find((event) => event.kind !== "run.finished" && textPreview(event.payload));
    peer.meta.lastMessage = lastMessage ? textPreview(lastMessage.payload) : "";
    const lastAt = [...peer.events]
      .reverse()
      .map((event) => (event.payload as { timestamp?: number } | undefined)?.timestamp)
      .find((value): value is number => typeof value === "number" && Number.isFinite(value));
    peer.meta.lastMessageAt = lastAt ? new Date(lastAt).toISOString() : undefined;
    const lastIsRunFinished = last?.kind === "run.finished";
    peer.running = peer.events.length > 0 && !lastIsRunFinished && runStatus !== "failed";
    if (peer.running) peer.meta.runStatus = "running";
    this.sessions.set(id, peer);
  }

  private need(sessionId: string): Peer {
    const peer = this.sessions.get(sessionId);
    if (!peer) throw new Error(`未知会话 ${sessionId}`);
    return peer;
  }

  // ---- events -----------------------------------------------------------

  private append(peer: Peer, kind: string, payload: unknown): WireEvent {
    const event: WireEvent = { seq: peer.nextSeq++, kind, payload };
    peer.events.push(event);
    peer.meta.lastSeq = event.seq;
    const preview = textPreview(payload);
    if (preview) peer.meta.lastMessage = preview;
    peer.meta.lastMessageAt = nowISO();
    peer.meta.updatedAt = nowISO();
    if (!peer.offline) this.deliver(peer, event);
    return event;
  }

  private deliver(peer: Peer, event: WireEvent) {
    for (const sub of peer.subscribers) {
      if (event.seq > sub.cursor) {
        sub.cursor = event.seq;
        sub.onEvent(event);
      }
    }
  }

  private flush(peer: Peer) {
    for (const sub of peer.subscribers) {
      for (const event of peer.events) {
        if (event.seq > sub.cursor) {
          sub.cursor = event.seq;
          sub.onEvent(event);
        }
      }
    }
  }

  private partial(peer: Peer, text: string, turnId?: string) {
    if (peer.offline) return;
    for (const sub of peer.subscribers) {
      if (sub.partials) sub.onPartial?.({ text, turnId });
    }
  }

  private stream(
    peer: Peer,
    opts: {
      after: number;
      partials?: boolean;
      onEvent: (event: WireEvent) => void;
      onPartial?: (p: { text: string; turnId?: string }) => void;
      onState: (s: ConnectionState["status"]) => void;
    },
  ): () => void {
    const sub: Subscriber = {
      cursor: opts.after,
      partials: !!opts.partials,
      onEvent: opts.onEvent,
      onPartial: opts.onPartial,
      onState: opts.onState,
    };
    peer.subscribers.push(sub);
    const open = setTimeout(() => {
      sub.onState(peer.offline ? "reconnecting" : "open");
      if (!peer.offline) this.flush(peer);
    }, 20) as unknown as number;
    void open;
    return () => {
      peer.subscribers = peer.subscribers.filter((item) => item !== sub);
      sub.onState("closed");
    };
  }

  // ---- commands ---------------------------------------------------------

  private async send(
    peer: Peer,
    text: string,
    delivery: Delivery,
    requestId: string,
    attachments?: AttachmentRef[],
  ) {
    if (peer.running) {
      const seq = peer.nextSeq;
      const accepted: Delivery = delivery === "guide" ? "guide" : "queue";
      const event = this.append(peer, "user.message", {
        text,
        requestId,
        delivery: accepted,
        timestamp: Date.now(),
        ...(attachments?.length ? { attachments } : {}),
      });
      peer.meta.runStatus = "running";
      if (accepted === "queue") {
        peer.queue.push({ seq: event.seq, requestId, text, delivery: accepted });
      } else {
        // A guide is consumed inside the running turn.
        this.schedule(peer, 1100, () => {
          if (!peer.running) return;
          this.append(peer, "assistant.message", {
            role: "assistant",
            content: [
              { type: "text", text: `已按引导调整：${text}` },
            ],
            inputSeqs: [seq],
            stopReason: "end_turn",
            timestamp: Date.now(),
          });
          this.finish(peer, "done");
        });
      }
      void seq;
      return;
    }
    const event = this.append(peer, "user.message", {
      text,
      requestId,
      delivery: "queue",
      timestamp: Date.now(),
      ...(attachments?.length ? { attachments } : {}),
    });
    peer.meta.runStatus = "running";
    this.reply(peer, event.seq, text);
  }

  private async stop(peer: Peer) {
    if (!peer.running) return;
    this.clearTimers(peer);
    peer.drainPaused = true;
    peer.running = false;
    peer.meta.runStatus = "cancelled";
    const timer = setTimeout(() => {
      this.append(peer, "run.finished", {
        turns: 1,
        tokensIn: 0,
        tokensOut: 0,
        stopReason: "cancelled",
        durationMs: 800,
        timestamp: Date.now(),
      });
    }, 320) as unknown as number;
    peer.timers.push(timer);
  }

  private async answer(peer: Peer, decisionId: string, answer: string) {
    this.append(peer, "decision", {
      decisionId,
      status: "answered",
      answer,
      timestamp: Date.now(),
    });
    this.schedule(peer, 600, () => {
      this.append(peer, "assistant.message", {
        role: "assistant",
        content: [{ type: "text", text: answer === "hold" ? "已暂缓发布，当前版本保持不变。" : "已记录你的选择，我会继续执行并汇报结果。" }],
        stopReason: "end_turn",
        timestamp: Date.now(),
      });
      this.finish(peer, "done");
    });
  }

  // ---- scripted runs ----------------------------------------------------

  private reply(peer: Peer, inputSeq: number, prompt: string) {
    peer.running = true;
    peer.meta.runStatus = "running";
    peer.drainPaused = false;
    const withTools = /工具|检查|health|部署|发布/i.test(prompt);
    const thinking = "收到。先确认上下文，再给出可执行的下一步。";
    const toolA = `call-${peer.id}-${peer.nextSeq}-a`;
    const toolB = `call-${peer.id}-${peer.nextSeq}-b`;
    this.schedule(peer, 260, () => {
      this.append(peer, "assistant.message", {
        role: "assistant",
        content: [
          { type: "thinking", thinking },
          ...(withTools
            ? [
                { type: "toolCall", id: toolA, name: "context.read", arguments: { scope: "session" } },
                { type: "toolCall", id: toolB, name: "workspace.list", arguments: { path: "." } },
              ]
            : []),
        ],
        inputSeqs: [inputSeq],
        stopReason: withTools ? "tool_use" : "end_turn",
        timestamp: Date.now(),
      });
    });
    let start = 900;
    if (withTools) {
      this.schedule(peer, 420, () => {
        this.append(peer, "tool.result", {
          toolCallId: toolA,
          toolName: "context.read",
          content: [{ type: "text", text: "loaded 2 files" }],
          isError: false,
          durationMs: 160,
          timestamp: Date.now(),
        });
      });
      this.schedule(peer, 460, () => {
        this.append(peer, "tool.result", {
          toolCallId: toolB,
          toolName: "workspace.list",
          content: [{ type: "text", text: "src/  web/  docs/" }],
          isError: false,
          durationMs: 90,
          timestamp: Date.now(),
        });
      });
      start = 1200;
    }
    const full = withTools
      ? "已检查当前工作区：src/、web/、docs/ 均可读，没有阻塞项。\n\n下一步建议：\n1. 先跑一遍 web 构建；\n2. 再用真实服务验证审批与停止；\n3. 最后补发布说明。"
      : `已收到：「${prompt}」。这是演示数据下的流式回复——\n\n- 会话、工具卡、审批与停止都可以操作；\n- 打开「partial」可以看 token 级增量；\n- 接入真实服务后，同一份界面直接消费 AgentAPI。`;
    const chunks = full.match(/.{1,12}/gs) ?? [full];
    chunks.forEach((_, index) => {
      this.schedule(peer, start + index * 70, () => {
        this.partial(peer, chunks.slice(0, index + 1).join(""), `live-${inputSeq}`);
      });
    });
    const end = start + chunks.length * 70 + 120;
    this.schedule(peer, end, () => {
      this.append(peer, "assistant.message", {
        role: "assistant",
        content: [{ type: "text", text: full }],
        inputSeqs: [inputSeq],
        stopReason: "end_turn",
        timestamp: Date.now(),
      });
      this.finish(peer, "done");
    });
  }

  private finish(peer: Peer, stopReason: string) {
    peer.running = false;
    peer.meta.runStatus = stopReason === "done" ? "done" : stopReason;
    this.append(peer, "run.finished", {
      turns: 1,
      tokensIn: 640,
      tokensOut: 180,
      stopReason,
      durationMs: 2400,
      timestamp: Date.now(),
    });
    if (peer.queue.length > 0 && !peer.drainPaused) {
      const next = peer.queue.shift()!;
      this.reply(peer, next.seq, next.text);
    }
  }

  private schedule(peer: Peer, ms: number, fn: () => void) {
    const timer = setTimeout(() => {
      peer.timers = peer.timers.filter((item) => item !== timer);
      try {
        fn();
      } catch (error) {
        console.warn("fixture script failed", error);
      }
    }, ms) as unknown as number;
    peer.timers.push(timer);
  }

  private clearTimers(peer: Peer) {
    for (const timer of peer.timers) clearTimeout(timer);
    peer.timers = [];
  }
}

/** A tiny helper for the empty-state demo: wait a beat before the first paint
 * so the loading skeleton is visible in screenshots. */
export const fixtureBootDelay = () => delay(120);
