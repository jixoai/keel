// The scoped `keel.plugin.*` bridge (W12).
//
// A contribution never receives the app's `host` object. It receives this
// bridge, opened with the grants its declaration asked for:
//
//   state      read the plugin's own inventory row (+ host theme tokens)
//   call       call a tool THIS plugin declares (the server checks ownership)
//   approve    open a decision and wait for its answer
//   subscribe  receive the plugin's own events
//
// Every call goes through `keel.plugin.call` with the scope token the host
// issued for this plugin; the host re-checks the name and the grant. An
// undeclared name fails with `ERR_ACCESS_DENIED` here and on the host, so a
// plugin cannot reach `keel.admin.*` or another plugin's tools by accident.
//
// The chunk form shares the host VM by design (spec: chunk 无隔离, trusted by
// assembly). This bridge is the capability boundary the contract fixes, not a
// sandbox against malicious same-VM code; that limitation is documented in the
// evidence for G7.
import { host } from "nsh";
import { PluginFault, asPluginFault } from "./errors";
import type {
  ApprovalRequest,
  ApprovalView,
  BridgeCapability,
  PluginBridge,
  PluginEvent,
  PluginStateView,
} from "./types";

export interface BridgeGrant {
  pluginId: string;
  capabilities: readonly BridgeCapability[];
  /** The tools this plugin declares (`provides.tools` / live names). */
  tools: string[];
  /** Hook events the plugin may subscribe to (server filters anyway). */
  events?: string[];
}

interface BridgeReply {
  ok?: boolean;
  code?: string;
  message?: string;
  result?: unknown;
  token?: string;
  subscriptionId?: string;
  [key: string]: unknown;
}

const CALL_TIMEOUT_MS = 15_000;
const SCOPE_TIMEOUT_MS = 5_000;

/** The host surface the bridge needs. Injected so tests can prove the
 * subscribe/unsubscribe lifecycle without a real embedding app. */
export interface BridgeHost {
  available(): boolean;
  call<T>(name: string, data?: unknown, options?: { timeout?: number }): Promise<T>;
  on<T>(name: string, listener: (payload: T) => void): () => void;
}

const defaultBridgeHost: BridgeHost = {
  available: () => {
    try {
      return host.available();
    } catch {
      return false;
    }
  },
  call: <T>(name: string, data?: unknown, options?: { timeout?: number }) =>
    host.call<T>(name, data, options) as Promise<T>,
  on: <T>(name: string, listener: (payload: T) => void) => host.on<T>(name, listener),
};

/** True when a host answers at all (a terminal test host has no bridge). */
export function hostBridgeAvailable(): boolean {
  return defaultBridgeHost.available();
}

/** Per-plugin event subscribers, shared by every bridge instance of a plugin. */
const eventListeners = new Map<string, Set<(event: PluginEvent) => void>>();
const eventStops = new Map<string, () => void>();
const eventStarting = new Map<string, Promise<void>>();

/** Diagnostics for the lifecycle tests: how many listeners and whether a host
 * stream is live for one plugin. Not used by a screen. */
export function pluginEventSubscriberCount(pluginId: string): number {
  return eventListeners.get(pluginId)?.size ?? 0;
}
export function pluginEventStreamActive(pluginId: string): boolean {
  return eventStops.has(pluginId) || eventStarting.has(pluginId);
}

/** A bridge is one contribution's view of the plugin's grants. */
class ScopedPluginBridge implements PluginBridge {
  readonly pluginId: string;
  readonly capabilities: readonly BridgeCapability[];
  private readonly tools: string[];
  private readonly events: string[];
  private scope: Promise<string> | null = null;
  /** Set by `dispose`: a subscribe that resolves after the withdrawal must not
   * install its host listener. */
  private disposed = false;

  constructor(
    grant: BridgeGrant,
    private readonly port: BridgeHost = defaultBridgeHost,
  ) {
    this.pluginId = grant.pluginId;
    this.capabilities = grant.capabilities;
    this.tools = grant.tools;
    this.events = grant.events ?? [];
  }

  async state(): Promise<PluginStateView> {
    this.require("state");
    const reply = await this.call("keel.plugin.state", {});
    if (!reply.result || typeof reply.result !== "object") {
      throw new PluginFault("ERR_PLUGIN", "宿主没有返回插件状态文档。");
    }
    return reply.result as PluginStateView;
  }

  async callTool(tool: string, args: unknown = {}): Promise<unknown> {
    this.require("call");
    const name = String(tool ?? "").trim();
    if (!name) throw new PluginFault("ERR_PLUGIN", "callTool 需要工具名。");
    if (!this.tools.includes(name)) {
      throw new PluginFault(
        "ERR_ACCESS_DENIED",
        `工具「${name}」不在插件 ${this.pluginId} 声明的 provides.tools 里。`,
      );
    }
    const reply = await this.call("keel.plugin.callTool", { tool: name, args });
    return reply.result;
  }

  async requestApproval(request: ApprovalRequest): Promise<ApprovalView> {
    this.require("approve");
    const reply = await this.call("keel.plugin.requestApproval", { request });
    const view = (reply.result ?? reply) as ApprovalView;
    return view;
  }

  onEvent(listener: (event: PluginEvent) => void): () => void {
    this.require("subscribe");
    const id = this.pluginId;
    let listeners = eventListeners.get(id);
    if (!listeners) {
      listeners = new Set();
      eventListeners.set(id, listeners);
    }
    listeners.add(listener);
    void this.startEvents().catch(() => {
      /* the subscription failure is the plugin's to observe through its own
         event stream; an unhandled rejection must not reach the host */
    });
    return () => {
      const current = eventListeners.get(id);
      current?.delete(listener);
      if (current && current.size === 0) {
        eventListeners.delete(id);
        const stop = eventStops.get(id);
        eventStops.delete(id);
        eventStarting.delete(id);
        stop?.();
        void this.call("keel.plugin.unsubscribe", {}).catch(() => {
          /* a stream that already closed is not an error to the page */
        });
      }
    };
  }

  /** Withdraw this plugin's event stream and every listener. The registry calls
   * it on disable/uninstall and when a declaration is replaced, so a plugin
   * cannot leave a subscription or a listener behind. */
  dispose(): void {
    const id = this.pluginId;
    this.disposed = true;
    const streamActive = eventStops.has(id) || eventStarting.has(id);
    eventListeners.delete(id);
    const stop = eventStops.get(id);
    eventStops.delete(id);
    eventStarting.delete(id);
    stop?.();
    this.scope = null;
    if (streamActive && this.capabilities.includes("subscribe") && this.port.available()) {
      void this.call("keel.plugin.unsubscribe", {}).catch(() => {
        /* a stream that already closed is not an error to the page */
      });
    }
  }

  /** Start the server stream once; its events fan out by plugin id. */
  private startEvents(): Promise<void> {
    const id = this.pluginId;
    if (this.disposed) return Promise.resolve();
    if (eventStops.has(id)) return Promise.resolve();
    const starting = eventStarting.get(id);
    if (starting) return starting;
    const start = (async () => {
      await this.call("keel.plugin.subscribe", { events: this.events });
      if (this.disposed) {
        // The plugin was withdrawn while the subscribe was in flight: the
        // server subscription exists, so close it and install no listener.
        await this.call("keel.plugin.unsubscribe", {}).catch(() => {});
        return;
      }
      try {
        const unsubscribe = this.port.on<PluginEvent>("keel.plugin.event", (payload) => {
          if (!payload || payload.pluginId !== id) return;
          for (const listener of eventListeners.get(id) ?? []) {
            try {
              listener(payload);
            } catch {
              /* one listener's throw must not stop the others */
            }
          }
        });
        eventStops.set(id, typeof unsubscribe === "function" ? unsubscribe : () => {});
      } catch {
        eventStops.set(id, () => {});
      }
    })().catch((error) => {
      eventStarting.delete(id);
      throw asPluginFault(error, "ERR_UNAVAILABLE");
    });
    eventStarting.set(id, start);
    return start;
  }

  private require(capability: BridgeCapability): void {
    if (!this.capabilities.includes(capability)) {
      throw new PluginFault(
        "ERR_ACCESS_DENIED",
        `插件 ${this.pluginId} 的贡献没有声明桥能力「${capability}」；在 contributes[].capabilities 里声明后再用。`,
      );
    }
  }

  private async scopeToken(): Promise<string> {
    if (!this.scope) {
      this.scope = (async () => {
        if (!this.port.available()) {
          throw new PluginFault("ERR_UNSUPPORTED", "这个宿主没有 keel 插件桥（host.call 不可用）。");
        }
        const reply = await this.port.call<BridgeReply>(
          "keel.plugin.scope",
          {
            pluginId: this.pluginId,
            capabilities: [...this.capabilities],
            tools: this.tools,
            events: this.events,
          },
          { timeout: SCOPE_TIMEOUT_MS },
        );
        if (!reply || reply.ok === false || typeof reply.token !== "string" || !reply.token) {
          throw new PluginFault(
            (reply?.code as never) ?? "ERR_ACCESS_DENIED",
            reply?.message ?? "宿主没有签发插件桥凭据。",
          );
        }
        return reply.token;
      })().catch((error) => {
        this.scope = null;
        throw asPluginFault(error, "ERR_ACCESS_DENIED");
      });
    }
    return this.scope;
  }

  private async call(name: string, args: Record<string, unknown>): Promise<BridgeReply> {
    if (!this.port.available()) {
      throw new PluginFault("ERR_UNSUPPORTED", "这个宿主没有 keel 插件桥（host.call 不可用）。");
    }
    let token: string;
    try {
      token = await this.scopeToken();
    } catch (error) {
      throw asPluginFault(error, "ERR_ACCESS_DENIED");
    }
    let reply: BridgeReply;
    try {
      reply = await this.port.call<BridgeReply>(
        "keel.plugin.call",
        { token, name, args },
        { timeout: CALL_TIMEOUT_MS },
      );
    } catch (error) {
      throw asPluginFault(error, "ERR_UNAVAILABLE");
    }
    if (!reply || typeof reply !== "object") {
      throw new PluginFault("ERR_PLUGIN", `插件桥「${name}」返回了空文档。`);
    }
    if (reply.ok === false) {
      throw new PluginFault((reply.code as never) ?? "ERR_PLUGIN", reply.message ?? `插件桥「${name}」失败。`);
    }
    return reply;
  }
}

/** Open one contribution's scoped bridge. */
export function createPluginBridge(grant: BridgeGrant, port?: BridgeHost): PluginBridge {
  return new ScopedPluginBridge(grant, port);
}
