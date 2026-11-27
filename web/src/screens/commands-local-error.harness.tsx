// The local-failure overlay on a command row (review P1-B): a 403/404 that
// happened before ClaimCommandInvocation has no durable command.invoked row,
// so the row must say it is a local record instead of pretending to wait.
import { mount, View } from "nsh";
import type { CommandInvocationView } from "../data/contract";
import { store } from "../data/store";
import { CommandInvocationRow } from "./command-invocation";

const row: CommandInvocationView = {
  requestId: "r-local",
  command: "e2e.commands:purge",
  source: "plugin",
  status: "pending_decision",
  seq: 1,
  errorCode: "permission_denied",
  error: "permission denied",
};

store.commandLocalOutcomes.set({
  "r-local": {
    status: "error",
    errorCode: "permission_denied",
    error: "没有执行该命令的权限。",
    local: true,
  },
});

mount(() => (
  <View style={{ flex: 1, flexDirection: "column", padding: 8 }}>
    <CommandInvocationRow invocation={() => row} />
  </View>
));
