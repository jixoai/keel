// TUI harness · the board's five states and permission faces.
//
// A test's module graph is not the app's, so the board transport must be
// installed inside the launched app. `host.params.scenario` picks one:
//
//   success          the scripted tenant (every task state, mirror, archive)
//   loading          the reads never settle
//   empty            readable channels, no tasks
//   no-channels      no readable channel
//   error            the queue read answers 500 (request id carried)
//   disabled         board_disabled 404
//   denied           no board.read and no task.read -> 404 page
//   legacy           task.read only -> migration page
//   reader           board.read only -> queue, no sync/automation tabs
//   sync-reader      board.read only, deep link to /board/sync -> 403 page
//   sync-error       manage, sync reads answer 500
//   automation-error manage, automation reads answer 500
//   gone             the retired /superagent address
//
// The success writes run on the app's own fixture transport; this harness only
// supplies the failing/permission states.
import { effect, host, mount, View } from "nsh";
import { KeelApiError, KeelHttp } from "../../data/http";
import type { BoardApi, BoardSyncEntryView } from "../../data/board-wire";
import { FixtureBoard } from "../../data/fixture-board";
import { boardStore } from "../../data/board";
import { setEffectivePermissions } from "../../data/permissions";
import { store } from "../../data/store";
import { BooksChrome } from "../books/shell";
import { BoardPage, GonePage } from "./page";

const scenario = (): string => String((host.params as Record<string, unknown> | undefined)?.scenario ?? "success");

const MANAGER = {
  "board.read": "tenant",
  "board.write": "tenant",
  "board.manage": "tenant",
  "task.read": "team",
} as const;
const READER = { "board.read": "tenant", "task.read": "team" } as const;
const LEGACY = { "task.read": "team" } as const;

function permissions(): Record<string, "own" | "team" | "tenant" | "all"> {
  switch (scenario()) {
    case "denied":
      return {};
    case "legacy":
      return { ...LEGACY };
    case "reader":
    case "sync-reader":
    case "automation-reader":
      return { ...READER };
    default:
      return { ...MANAGER };
  }
}

function failure(status: number, message: string, code?: string, requestId?: string): KeelApiError {
  return new KeelApiError(status, message, [], undefined, code, {}, requestId);
}

function readOnly(api: BoardApi): BoardApi {
  const tasks = api.boardTasks.bind(api);
  api.boardTasks = async (query) => {
    const page = await tasks(query);
    return {
      ...page,
      tasks: page.tasks.map((summary) => ({
        ...summary,
        message: {
          ...summary.message,
          task: summary.message.task ? { ...summary.message.task, moves: [] } : summary.message.task,
        },
      })),
    };
  };
  const thread = api.boardThread.bind(api);
  api.boardThread = async (id, after, limit) => {
    const view = await thread(id, after, limit);
    return {
      ...view,
      message: {
        ...view.message,
        task: view.message.task ? { ...view.message.task, moves: [] } : view.message.task,
      },
    };
  };
  return api;
}

function stripMoves(api: BoardApi): BoardApi {
  const tasks = api.boardTasks.bind(api);
  api.boardTasks = async (query) => {
    const page = await tasks(query);
    return {
      ...page,
      tasks: page.tasks.map((summary) => ({
        ...summary,
        message: {
          ...summary.message,
          task: summary.message.task ? { ...summary.message.task, moves: undefined } : summary.message.task,
        },
      })),
    };
  };
  const thread = api.boardThread.bind(api);
  api.boardThread = async (id, after, limit) => {
    const view = await thread(id, after, limit);
    return {
      ...view,
      message: {
        ...view.message,
        task: view.message.task ? { ...view.message.task, moves: undefined } : view.message.task,
      },
    };
  };
  return api;
}

function transport(): BoardApi {
  const base = new FixtureBoard();
  const name = scenario();
  if (name === "reader" || name === "sync-reader") return readOnly(base);
  if (name === "no-moves") return stripMoves(base);
  if (name === "loading") {
    const never = <T,>(): Promise<T> => new Promise<T>(() => {});
    base.boardChannels = never;
    base.boardTasks = never;
    base.boardActors = never;
    return base;
  }
  if (name === "empty") {
    base.boardTasks = async () => ({ tasks: [], cursor: "0" });
    return base;
  }
  if (name === "task-loading") {
    base.boardThread = () => new Promise(() => {});
    return base;
  }
  if (name === "task-error") {
    base.boardThread = async () => {
      throw failure(500, "线程读取失败", "internal", "req-thread-1");
    };
    return base;
  }
  if (name === "sync-empty") {
    base.boardSyncEntries = async () => ({ entries: [] });
    base.boardSyncStatus = async () => ({ keys: [], outbox: { pending: 0, leased: 0, done: 0, dead: 0 }, syncEnabled: true });
    base.boardSyncInbox = async () => ({ dead: [] });
    base.boardSyncFailures = async () => ({ failures: [] });
    base.boardSyncCredentials = async () => ({ entries: [] });
    return base;
  }
  if (name === "sync-missing") {
    const strip = (row: unknown): BoardSyncEntryView => {
      const entry = { ...(row as BoardSyncEntryView) };
      delete entry.targets;
      delete entry.lag;
      delete entry.failures24h;
      delete entry.oldestPendingAgeSeconds;
      return entry;
    };
    const entries = base.boardSyncEntries.bind(base);
    base.boardSyncEntries = async () => {
      const page = await entries();
      return { ...page, entries: page.entries.map(strip) };
    };
    const status = base.boardSyncStatus.bind(base);
    base.boardSyncStatus = async () => {
      const page = await status();
      return { ...page, keys: page.keys.map(strip), failures24h: undefined, lag: undefined };
    };
    return base;
  }
  if (name === "sync-loading") {
    const never = <T,>(): Promise<T> => new Promise<T>(() => {});
    base.boardSyncEntries = never;
    base.boardSyncStatus = never;
    base.boardSyncInbox = never;
    base.boardSyncFailures = never;
    base.boardSyncCredentials = never;
    return base;
  }
  if (name === "automation-empty") {
    base.boardWatchers = async () => ({ watchers: [] });
    base.boardSchedules = async () => ({ schedules: [] });
    return base;
  }
  if (name === "automation-loading") {
    const never = <T,>(): Promise<T> => new Promise<T>(() => {});
    base.boardWatchers = never;
    base.boardSchedules = never;
    return base;
  }
  if (name === "no-channels") {
    base.boardChannels = async () => ({ channels: [], cursor: "0" });
    base.boardTasks = async () => ({ tasks: [], cursor: "0" });
    return base;
  }
  if (name === "error") {
    base.boardTasks = async () => {
      throw failure(500, "服务暂时没有响应", "internal", "req-board-1");
    };
    return base;
  }
  if (name === "disabled") {
    base.boardTasks = async () => {
      throw failure(404, "board disabled", "board_disabled");
    };
    return base;
  }
  if (name === "me-missing") {
    base.boardMe = async () => {
      throw failure(404, "not found", "not_found", "req-me-1");
    };
    return base;
  }
  if (name === "sync-error") {
    base.boardSyncEntries = async () => {
      throw failure(500, "同步状态不可用", "internal", "req-sync-1");
    };
    return base;
  }
  if (name === "automation-error") {
    base.boardWatchers = async () => {
      throw failure(500, "自动化状态不可用", "internal", "req-auto-1");
    };
    return base;
  }
  return base;
}

function Harness() {
  let booted = false;
  effect(() => {
    if (booted) return;
    booted = true;
    const params = (host.params ?? {}) as Record<string, unknown>;
    store.skipBootForTest();
    store.resetAuthForTest();
    setEffectivePermissions(permissions(), false);
    const base = typeof params.base === "string" ? params.base : "";
    store.setBoardTransport(scenario() === "sse-live" && base ? new KeelHttp(base) : transport());
    store.auth.set("ok");
    boardStore.reset();
    const taskId = typeof params.task === "string" ? params.task : null;
    const search = typeof params.search === "string" ? params.search : "";
    if (scenario() === "sync-reader") store.openBoard("sync");
    else if (scenario().startsWith("sync-")) store.openBoard("sync");
    else if (scenario().startsWith("automation-")) store.openBoard("automation");
    else if (scenario() === "task-loading" || scenario() === "task-error") store.openBoard("queue", "999");
    else if (scenario() === "gone") store.openGone();
    else store.openBoard("queue", taskId, search);
  });
  return (
    <BooksChrome>
      <View style={{ flex: 1, minHeight: 0 }}>
        <If when={store.route() === "gone"} else={<BoardPage />}>
          <GonePage />
        </If>
      </View>
    </BooksChrome>
  );
}

mount(Harness);
