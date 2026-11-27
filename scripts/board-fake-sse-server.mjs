#!/usr/bin/env node
// A real HTTP SSE stand-in for the board client evidence run.
//
// It serves the handful of board routes the harness reads, then one stream:
// `: connected`, a hidden id-only frame, and a visible task.claimed frame.
// The test asserts the hidden frame advances the cursor without touching the
// view, and the visible one moves the row from 可认领 to 已认领 through the
// real `KeelHttp` SSE reader — not a mocked onFrame call.
import http from "node:http";

const portArg = process.argv.find((arg) => arg.startsWith("--port="));
const requested = portArg ? Number(portArg.slice("--port=".length)) : 0;

let currentState = "ready";

const movesFor = (state) =>
  state === "ready"
    ? ["claim", "block", "reply"]
    : state === "blocked"
      ? ["unblock", "assign", "needs", "cancel", "reply"]
      : ["reply"];

const task = (state, withMoves = true) => ({
  state,
  owner: "account:test",
  by: "account:test",
  needs: [],
  epoch: state === "ready" ? 1 : 2,
  claim: null,
  blocked: null,
  result: null,
  reason: null,
  ...(withMoves ? { moves: movesFor(state) } : {}),
});

const message = (state, withMoves = true) => ({
  id: "900",
  channel: "work",
  text: "SSE 假服务器任务",
  createdAt: new Date(Date.now() - 60_000).toISOString(),
  labels: {},
  reactions: {},
  attachments: [],
  links: [],
  task: task(state, withMoves),
  version: state === "ready" ? 1 : 2,
  revision: 1,
  by: { id: "account:test", handle: "test", name: "测试员" },
  author: { id: "account:test", handle: "test", name: "测试员" },
});

const json = (res, body) => {
  res.writeHead(200, {
    "content-type": "application/json; charset=utf-8",
    "cache-control": "no-store",
    "access-control-allow-origin": "*",
  });
  res.end(JSON.stringify(body));
};

const server = http.createServer((req, res) => {
  const url = new URL(req.url, "http://127.0.0.1");
  const path = url.pathname;
  if (req.method === "OPTIONS") {
    res.writeHead(204, {
      "access-control-allow-origin": "*",
      "access-control-allow-methods": "GET,POST,DELETE,OPTIONS",
      "access-control-allow-headers": "content-type,last-event-id,if-revision",
    });
    res.end();
    return;
  }
  if (path === "/v1/board/me") {
    return json(res, {
      account: { id: "account:test", username: "test", displayName: "测试员" },
      actor: "account:test",
      actorName: "测试员",
      can: { board: { read: true, write: true, manage: true } },
    });
  }
  if (path === "/v1/board/channels") {
    return json(res, { channels: [{ path: "work", lastIndex: 1, messages: 1, threads: 1 }], cursor: "1" });
  }
  if (path === "/v1/board/actors") {
    return json(res, {
      actors: [{ id: "account:test", kind: "human", handle: "test", name: "测试员" }],
      cursor: "1",
    });
  }
  if (path === "/v1/board/tasks") {
    return json(res, { tasks: [{ message: message(currentState), replies: 0 }], cursor: "1" });
  }
  if (path === "/v1/board/messages/900") {
    return json(res, { message: message(currentState), replies: [], cursor: "1" });
  }
  if (path === "/v1/board/events") {
    res.writeHead(200, {
      "content-type": "text/event-stream; charset=utf-8",
      "cache-control": "no-cache",
      connection: "keep-alive",
      "access-control-allow-origin": "*",
      "x-accel-buffering": "no",
    });
    res.write(": connected\n\n");
    // The hidden event: id only, no data. A client that drops it replays it on
    // every reconnect instead of advancing.
    res.write("id: 2\n\n");
    const timer = setTimeout(() => {
      currentState = "blocked";
      const body = {
        cursor: "3",
        type: "task.blocked",
        createdAt: new Date().toISOString(),
        payload: {},
        // The raw SSE projection deliberately omits viewer-dependent moves;
        // the client must invalidate and GET the augmented view.
        message: message("blocked", false),
      };
      res.write(`id: 3\ndata: ${JSON.stringify(body)}\n\n`);
    }, 900);
    const ping = setInterval(() => res.write(": ping\n\n"), 5000);
    req.on("close", () => {
      clearTimeout(timer);
      clearInterval(ping);
    });
    return;
  }
  res.writeHead(404, { "content-type": "application/json", "access-control-allow-origin": "*" });
  res.end(JSON.stringify({ error: "not found", code: "not_found" }));
});

server.listen(requested, "127.0.0.1", () => {
  const address = server.address();
  console.log(JSON.stringify({ base: `http://127.0.0.1:${address.port}` }));
});
