// Harness for the /command send gate when the catalog is not ready
// (change keel-plugin-commands, review P1-1).
//
// The real Composer is mounted with the real store; `store.send` is replaced by
// a counter so the test can prove a `/name ` draft never becomes a user
// message. The command transport is injected: `loading` never answers,
// `error` answers 500.
import { effect, host, mount, signal, Text, View } from "nsh";
import type { CommandApi, CommandCatalogView, CommandExecutionView } from "../data/contract";
import { KeelApiError } from "../data/http";
import { store } from "../data/store";
import { Composer } from "./composer";

const sent = signal(0);
let configured = false;

function scenario(): string {
  return String((host.params as Record<string, unknown> | undefined)?.scenario ?? "loading");
}

function transport(): CommandApi {
  if (scenario() === "error") {
    return {
      commandCatalog: async () => {
        throw new KeelApiError(500, "服务暂时没有响应", [], undefined, "internal");
      },
      executeCommand: async (): Promise<CommandExecutionView> => ({
        requestId: "never",
        command: "compact",
        source: "builtin",
        status: "ok",
      }),
    };
  }
  return {
    commandCatalog: async () => await new Promise<CommandCatalogView>(() => {}),
    executeCommand: async (): Promise<CommandExecutionView> => ({
      requestId: "never",
      command: "compact",
      source: "builtin",
      status: "ok",
    }),
  };
}

function Harness() {
  effect(() => {
    if (store.mode() === "checking") return;
    if (!store.activeId()) {
      const first = store.sessions()[0];
      if (first) store.activeId.set(first.sessionId);
      return;
    }
    if (configured) return;
    configured = true;
    store.setCommandTransport(transport());
    // The spy: a command attempt must never reach the ordinary send path.
    (store as unknown as { send: () => Promise<boolean> }).send = async () => {
      sent.update((value) => value + 1);
      return true;
    };
    void store.loadCommands({ force: true });
  });
  return (
    <View style={{ flex: 1, flexDirection: "column", padding: 8 }}>
      <Text testID="commands-sent">{`sent:${sent()}`}</Text>
      <Text testID="commands-catalog-state">
        {store.commandCatalogError()
          ? `目录错误：${store.commandCatalogError()}`
          : store.commandCatalogLoading()
            ? "目录加载中"
            : store.commandCatalog()
              ? "目录就绪"
              : "目录未读取"}
      </Text>
      <Text testID="commands-notice">{store.notice()?.text ?? ""}</Text>
      <Composer view={null} />
    </View>
  );
}

mount(Harness);
