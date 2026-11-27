// TUI states harness for the command menu (change keel-plugin-commands 4.4).
//
// The fixture transport is not enough for the failing states: this harness
// injects a command transport whose catalog is loading/empty/failing/restricted,
// so the REAL store, the REAL CommandMenu and its five faces are exercised
// without a network. `?scenario=` (host.params.scenario) picks one.
import { effect, host, mount, View } from "nsh";
import { KeelApiError } from "../data/http";
import type { CommandApi, CommandCatalogView, CommandExecutionView } from "../data/contract";
import { store } from "../data/store";
import { CommandMenu } from "./composer-commands";

const scenario = () => String((host.params as Record<string, unknown> | undefined)?.scenario ?? "success");

function catalog(commands: CommandCatalogView["commands"]): CommandCatalogView {
  return { commands, truncated: false, generation: 1 };
}

function client(): CommandApi {
  const name = scenario();
  return {
    commandCatalog: async (): Promise<CommandCatalogView> => {
      if (name === "loading") return await new Promise<CommandCatalogView>(() => {});
      if (name === "error") {
        throw new KeelApiError(500, "服务暂时没有响应", [], undefined, "internal", {}, "req-command-error");
      }
      if (name === "empty") return catalog([]);
      if (name === "permission") {
        return catalog([
          {
            name: "compact",
            source: "builtin",
            summary: "压缩当前会话上下文",
            argsSchema: { type: "object" },
            danger: "write",
            idempotent: true,
            targets: ["web", "native", "tui"],
            requiredPermissions: ["session.write", "model.invoke"],
            available: false,
            reason: "missing_permissions",
            missingPermissions: ["model.invoke"],
          },
        ]);
      }
      return catalog([
        {
          name: "compact",
          source: "builtin",
          summary: "压缩当前会话上下文",
          argsSchema: { type: "object" },
          danger: "write",
          idempotent: true,
          targets: ["web", "native", "tui"],
          requiredPermissions: ["session.write", "model.invoke"],
          available: true,
        },
        {
          name: "demo.workflow:plan",
          source: "plugin",
          summary: "把目标拆成可执行的计划",
          argsSchema: {
            type: "object",
            properties: { goal: { type: "string" } },
            required: ["goal"],
          },
          danger: "write",
          idempotent: false,
          targets: ["web", "native", "tui"],
          requiredPermissions: ["session.write", "plugin.invoke"],
          available: true,
        },
      ]);
    },
    executeCommand: async (): Promise<CommandExecutionView> => ({
      requestId: "noop",
      command: "compact",
      source: "builtin",
      status: "ok",
    }),
  };
}

function Harness() {
  let booted = false;
  effect(() => {
    if (booted) return;
    booted = true;
    store.activeId.set("commands-states");
    store.setCommandTransport(client());
    void store.loadCommands({ force: true });
  });
  return (
    <View style={{ flex: 1, flexDirection: "column", padding: 8 }}>
      <CommandMenu query="" onPick={() => {}} />
    </View>
  );
}

mount(Harness);
