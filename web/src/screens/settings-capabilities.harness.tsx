// TUI harness · settings「能力」— the section alone, with its two probes
// answered in memory.
//
// The product's transport is same-origin `globalThis.fetch`; an in-memory
// terminal has no origin for a relative URL to resolve against, so the harness
// installs a stub through the section's test/host seam. The scenario comes
// from the launch parameters (`host.params.scenario`), keeping one harness for
// every state the gate covers. What is under test is still the real `KeelHttp`
// parsing and the section's rendering, not a second fake client.
import { host, mount, View } from "nsh";
import type { FetchLike } from "../data/http";
import { CapabilitiesSection, setCapabilityFetch } from "./settings";

const scenario = (): string => String(host.params.scenario ?? "mixed");

const respond = (status: number, body: unknown): Response =>
  new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json" } });

const media = (): unknown => ({
  configured: true,
  capabilities: {
    chat: { status: "available", model: "deepseek-v4.1-flash" },
    image: { status: "available", model: "image-3" },
    imageEdit: { status: "unsupported", reason: "provider does not support image editing" },
    decision: { status: "unconfigured", env: ["KEEL_MEDIA_MODEL_DECISION"] },
    video: { status: "unconfigured" },
    tts: { status: "available" },
    stt: {
      status: "unsupported",
      reason: "failed: GET https://media.internal/v1/models?api_key=sk-live-abcdef rejected",
    },
  },
});

const configuredRealtime = (): unknown => ({
  configured: true,
  provider: "qwen",
  kind: "qwen",
  models: ["qwen-omni-realtime"],
  inputSampleRate: 24000,
  outputSampleRate: 24000,
});

/** The media gateway's own report, exactly as `GET /v1/media/capabilities`
 * answers it: a flat map of booleans plus a `models` map. */
const flatGateway = (): unknown => ({
  configured: true,
  llm: true,
  image: true,
  decision: true,
  video: false,
  tts: false,
  stt: false,
  realtime: false,
  models: {
    llm: "bailian-token-plan/qwen3.8-flash",
    image: "bailian-token-plan/wan2.7-image",
    decision: "bailian-token-plan/decision-model-preview",
  },
});

const stub: FetchLike = async (input) => {
  const path = input;
  switch (scenario()) {
    case "auth":
      return respond(401, { error: "unauthorized: bearer sk-live-abcdef123456" });
    case "missing":
      return respond(404, { error: "404 page not found" });
    case "offline":
      throw new Error("connection reset by peer");
    case "media-unconfigured":
      return path.includes("/v1/realtime/status")
        ? respond(200, configuredRealtime())
        : respond(503, { error: "media gateway is not configured" });
    case "flat":
      return path.includes("/v1/realtime/status")
        ? respond(200, { configured: false, provider: "", models: [] })
        : respond(200, flatGateway());
    case "mixed":
    default:
      return path.includes("/v1/realtime/status")
        ? respond(503, { error: "realtime provider is not configured" })
        : respond(200, media());
  }
};

setCapabilityFetch(stub);

function Harness() {
  return (
    <View style={{ flex: 1, flexDirection: "column", padding: 8 }}>
      <CapabilitiesSection />
    </View>
  );
}

mount(Harness);
