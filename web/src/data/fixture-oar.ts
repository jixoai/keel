// The OAR sample of the development transport — change keel-oar-renderer.
//
// The fixture transport exists for development runs only (`?fixture=1` plus
// `?oar=1`, or `host.params.fixture`/`host.params.oar`). This module is the
// sample's data: the external session's one run (three `oar.record` events, so
// the "one card per session" rule is exercised), the server-folded projection
// the card draws, and the task card the run is attributed to.
//
// The projection is the Go fixture's shape (`internal/oar/testdata/
// tool-renderer.fixture.json`) with this sample's ids and one long message, so
// the row truncation is visible. It is `runtime: "fake"` on purpose: the demo
// must never look like a live model run.
import { OAR_PROJECTION_SCHEMA, type ExternalProjectionView } from "./oar-projection";

/** The demo external session's id. */
export const FIXTURE_OAR_SESSION_ID = "demo-oar";

/** The existing task card the demo run belongs to (never a new card). */
export const FIXTURE_OAR_TASK_ID = "task-oar-codex";

const LONG_NOTE =
  "构建脚本只发布 web 产物：nsh package --target web 把入口、主题与 assets 一起打进 dist，" +
  "签名与公证交给 CI；本地的 release 目录不进包，也不在 dev 服务器上留快照。" +
  "如果发布说明要截图，记得从同一份构建取，避免把开发机的临时样式带进证据。";

/** The three `oar.record` external events of the demo turn. The payload is the
 * durable external-event shape (`internal/externalgw/agent.go`): provenance
 * marks plus the record itself in `data`, so `payloadNote` reads
 * `type: "oar.record"` exactly as it does against a live sidecar. */
export function demoOarEvents(): Array<{ kind: string; payload: unknown }> {
  const record = (seq: number, data: Record<string, unknown>) => ({
    backend: "oar",
    externalRef: "oar-demo-1",
    stateOwner: "external",
    compatibility: true,
    direction: "receive",
    seq,
    type: "oar.record",
    data: {
      schema: "keel.oar.record/1",
      seq,
      agentPath: "/main",
      sessionId: "oar-demo-1",
      turnId: "turn-1",
      attribution: { sessionId: FIXTURE_OAR_SESSION_ID, taskId: FIXTURE_OAR_TASK_ID },
      runtime: "fake",
      ts: `2026-11-24T09:00:0${seq}.000Z`,
      ...data,
    },
  });
  return [
    {
      kind: "external.event",
      payload: record(1, { kind: "status", status: "started", name: "turn.started" }),
    },
    {
      kind: "external.event",
      payload: record(2, {
        kind: "call",
        callId: "call-1",
        name: "shell",
        status: "completed",
        // The demo's own command: read the package script, not a made-up one.
        input: { command: "sed -n '1,120p' scripts/package-web.sh" },
      }),
    },
    {
      kind: "external.event",
      payload: record(3, { kind: "status", status: "completed", name: "turn.finished" }),
    },
  ];
}

/** The projection the route would answer (`keel.oar.render-projection/1`). A
 * fake runtime keeps `simulated: true`, which the card shows. */
export function demoOarProjection(sessionId: string = FIXTURE_OAR_SESSION_ID): ExternalProjectionView {
  return {
    schema: OAR_PROJECTION_SCHEMA,
    backendSessionId: "oar-demo-1",
    hostSessionId: sessionId,
    taskId: FIXTURE_OAR_TASK_ID,
    runtime: "fake",
    simulated: true,
    agents: [
      { agentPath: "/main", runtime: "fake", status: "completed", calls: 1, failures: 0 },
      { agentPath: "/main/worker-1", runtime: "fake", status: "completed", calls: 1, failures: 0 },
    ],
    items: [
      {
        seq: 1,
        agentPath: "/main",
        kind: "status",
        name: "turn.started",
        state: "started",
        text: "turn started",
      },
      {
        seq: 2,
        resultSeq: 3,
        agentPath: "/main",
        kind: "call",
        callId: "call-1",
        name: "shell",
        state: "completed",
        input: { command: "go test ./internal/oar/... -count=1" },
        output: { stdout: "ok  keel/internal/oar  1.24s", exitCode: 0 },
      },
      {
        seq: 4,
        resultSeq: 5,
        agentPath: "/main/worker-1",
        kind: "call",
        callId: "call-2",
        name: "read_file",
        state: "completed",
        input: { path: "scripts/package-web.sh" },
        output: { bytes: 2140, lines: 63 },
      },
      {
        seq: 6,
        agentPath: "/main",
        kind: "message",
        name: "agent_message",
        text: LONG_NOTE,
      },
      {
        seq: 7,
        agentPath: "/main",
        kind: "usage",
        name: "turn.usage",
        usage: {
          metering: "external",
          model: "fake-model",
          inputTokens: 12,
          outputTokens: 5,
          totalTokens: 17,
        },
      },
      {
        seq: 8,
        agentPath: "/main",
        kind: "status",
        name: "turn.finished",
        state: "completed",
        text: "turn completed",
      },
    ],
  };
}
