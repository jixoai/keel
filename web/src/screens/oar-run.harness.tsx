// The external run card's states — change keel-oar-renderer.
//
// A harness is needed because a test's module graph is not the app's: the
// conversation host (`ExternalRunRow`), the builtin contribution and the
// transport seam must all be installed inside the launched app. The scenario
// arrives in the launch parameters (`host.params.scenario`):
//
//   oar-card            a fake runtime's full projection (simulated, long row,
//                       usage, attribution)
//   oar-codex           a live-shaped projection: runtime codex, no simulated
//                       mark, no task attribution
//   oar-empty           an empty journal: the same schema with no rows
//   oar-error           the route's 501 `oar_not_wired`
//   oar-no-transport    a transport that never served the surface
//   oar-no-contribution the builtin renderer unregistered (chunk unavailable)
//   oar-loading         the read never settles
//
// The product's own success face is driven by the app's fixture transport
// (`?fixture=1&oar=1`), not here.
import { host, mount, Text, View } from "nsh";
import { KeelApiError } from "../data/http";
import type { IdentityApi } from "../data/identity-api";
import { store } from "../data/store";
import { oarAccess } from "../data/oar";
import { demoOarProjection } from "../data/fixture-oar";
import { color, space, typeScale } from "../ui/kit";
import { installPluginRuntime, pluginSlots } from "../plugins";
import { currentPluginTarget } from "../plugins/forms";
import { OAR_PLUGIN_ID } from "../plugins/oar";
import { ExternalRunRow } from "./messages";

const scenario = (): string => String(host.params.scenario ?? "oar-card");

const SESSION = "8a10c1de-0000-4000-8000-0000000000a1";

const API = (): IdentityApi => {
  const api: IdentityApi = {
    async listAccounts() {
      return { items: [], limit: 100, hasMore: false };
    },
  };
  const name = scenario();
  if (name === "oar-no-transport") return api;
  api.externalProjection = async (sessionId: string) => {
    if (name === "oar-loading") return new Promise(() => {});
    if (name === "oar-error") {
      throw new KeelApiError(501, "this deployment has not wired the OAR journal", [], undefined, "oar_not_wired");
    }
    if (name === "oar-empty") {
      return { ...demoOarProjection(sessionId), items: [], agents: [] };
    }
    if (name === "oar-fake-unmarked") {
      // A malformed projection: the adapter says fake, the mark was forgotten.
      return { ...demoOarProjection(sessionId), simulated: undefined };
    }
    if (name === "oar-codex") {
      return {
        ...demoOarProjection(sessionId),
        runtime: "codex",
        simulated: undefined,
        taskId: undefined,
      };
    }
    return demoOarProjection(sessionId);
  };
  return api;
};

store.skipBootForTest();
store.setIdentityTransport(API());
installPluginRuntime();
// The chunk-unavailable face: the declaration is gone, so the slot has no
// renderer and the host keeps its text row.
if (scenario() === "oar-no-contribution") pluginSlots.unregisterPlugin(OAR_PLUGIN_ID);
oarAccess.reset();

function Harness() {
  return (
    <View style={{ flex: 1, backgroundColor: color.background, padding: space.xl }}>
      {/* The host the app is running as: a test may not assume the dev host's
          own target (the terminal launch opens the app elsewhere). */}
      <Text testID="oar-harness-target" style={{ fontSize: typeScale.xs, color: color.mutedForeground }}>
        {`target=${currentPluginTarget()}`}
      </Text>
      <ExternalRunRow sessionId={SESSION} seq={3} />
    </View>
  );
}

mount(Harness);
