// The plugin bridge's event lifecycle (change keel-plugin-attachment, P1-2).
//
// The host is injected, so these tests prove the real subscribe/unsubscribe
// calls and listener fan-out without an embedding app. A disabled or replaced
// plugin must not keep a listener, a host stream or a server subscription.
import { expect, test } from "nsh/test";
import {
  createPluginBridge,
  pluginEventStreamActive,
  pluginEventSubscriberCount,
  type BridgeHost,
} from "./bridge";
import { SlotRegistry } from "./registry";

class FakeBridgeHost implements BridgeHost {
  readonly calls: { name: string; data?: unknown }[] = [];
  private listener: ((payload: unknown) => void) | null = null;
  stops = 0;

  available(): boolean {
    return true;
  }

  async call<T>(name: string, data?: unknown): Promise<T> {
    this.calls.push({ name, data });
    if (name === "keel.plugin.scope") return { ok: true, token: "scope-token" } as T;
    return { ok: true } as T;
  }

  on<T>(name: string, listener: (payload: T) => void): () => void {
    if (name !== "keel.plugin.event") throw new Error(`unexpected host event ${name}`);
    this.listener = listener as (payload: unknown) => void;
    return () => {
      this.stops += 1;
      this.listener = null;
    };
  }

  emit(payload: unknown): void {
    this.listener?.(payload);
  }
}

const tick = async () => new Promise((resolve) => setTimeout(resolve, 50));

test("bridge: subscribe starts one stream, dispose unsubscribes and drops every listener", async () => {
  const host = new FakeBridgeHost();
  const bridge = createPluginBridge(
    { pluginId: "demo.bridge", capabilities: ["state", "subscribe"], tools: [], events: ["turn.end"] },
    host,
  );
  const seen: string[] = [];
  bridge.onEvent((event) => seen.push(event.event));
  await tick();
  const names = host.calls.map((call) =>
    call.name === "keel.plugin.call" ? String((call.data as { name?: string })?.name ?? "") : call.name,
  );
  expect(names).toEqual(["keel.plugin.scope", "keel.plugin.subscribe"]);
  expect(pluginEventSubscriberCount("demo.bridge")).toBe(1);
  expect(pluginEventStreamActive("demo.bridge")).toBe(true);

  host.emit({ pluginId: "demo.bridge", event: "turn.end" });
  host.emit({ pluginId: "another.plugin", event: "turn.end" });
  expect(seen).toEqual(["turn.end"]);

  bridge.dispose?.();
  await tick();
  expect(pluginEventSubscriberCount("demo.bridge")).toBe(0);
  expect(pluginEventStreamActive("demo.bridge")).toBe(false);
  expect(host.stops).toBe(1);
  expect(
    host.calls.some(
      (call) => call.name === "keel.plugin.call" && (call.data as { name?: string })?.name === "keel.plugin.unsubscribe",
    ),
  ).toBe(true);

  // The old listener cannot receive anything after disposal.
  host.emit({ pluginId: "demo.bridge", event: "turn.end" });
  expect(seen).toEqual(["turn.end"]);
});

test("registry: uninstalling a plugin withdraws its bridge subscription", async () => {
  const host = new FakeBridgeHost();
  const slots = new SlotRegistry(host);
  slots.registerBaseSlots();
  slots.registerPlugin({
    pluginId: "demo.subscriber",
    contributes: [
      {
        id: "panel",
        slot: "keel.ui.sidebar",
        form: "chunk",
        entry: "fixture:w12-hello",
        capabilities: ["state", "subscribe"],
      },
    ],
  });
  await tick();
  const bridge = slots.bridgeFor("demo.subscriber");
  bridge.onEvent(() => {});
  await tick();
  expect(pluginEventSubscriberCount("demo.subscriber")).toBe(1);
  const names = host.calls.map((call) =>
    call.name === "keel.plugin.call" ? String((call.data as { name?: string })?.name ?? "") : call.name,
  );
  expect(names).toEqual(["keel.plugin.scope", "keel.plugin.subscribe"]);

  slots.unregisterPlugin("demo.subscriber");
  await tick();
  expect(pluginEventSubscriberCount("demo.subscriber")).toBe(0);
  expect(pluginEventStreamActive("demo.subscriber")).toBe(false);
  expect(host.stops).toBe(1);
  expect(
    host.calls.some(
      (call) => call.name === "keel.plugin.call" && (call.data as { name?: string })?.name === "keel.plugin.unsubscribe",
    ),
  ).toBe(true);
});

test("registry: replacing a declaration also withdraws the previous subscription", async () => {
  const host = new FakeBridgeHost();
  const slots = new SlotRegistry(host);
  slots.registerBaseSlots();
  slots.registerPlugin({
    pluginId: "demo.replaced",
    contributes: [
      {
        id: "panel",
        slot: "keel.ui.sidebar",
        form: "chunk",
        entry: "fixture:w12-hello",
        capabilities: ["state", "subscribe"],
      },
    ],
  });
  await tick();
  slots.bridgeFor("demo.replaced").onEvent(() => {});
  await tick();
  expect(pluginEventSubscriberCount("demo.replaced")).toBe(1);

  slots.registerPlugin({
    pluginId: "demo.replaced",
    contributes: [
      {
        id: "panel",
        slot: "keel.ui.sidebar",
        form: "chunk",
        entry: "fixture:w12-hello",
        title: "replaced",
        capabilities: ["state", "subscribe"],
      },
    ],
  });
  expect(pluginEventSubscriberCount("demo.replaced")).toBe(0);
  expect(pluginEventStreamActive("demo.replaced")).toBe(false);
  expect(host.stops).toBeGreaterThanOrEqual(1);
});
