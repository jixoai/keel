// Fixture · the event stream's transient-404 policy.
//
// The store treats a "closed" stream as permanent (it has no re-subscribe path),
// so one 404 from the stream route would end live updates for the whole session.
// This host answers the stream with 404 twice and then a single SSE frame; the
// assertions are that the client retried and then opened instead of closing.
//
// The responses are plain objects shaped like what `stream()` reads (ok/status/
// text/body.getReader) rather than real `Response`s: the terminal target has no
// fetch/ReadableStream globals, and the code under test reads no more than this.
import { mount, signal, Text, View } from "nsh";
import { KeelHttp } from "./http";

const state = signal("connecting");
const events = signal(0);
const attempts = signal(0);
const lastState = signal("");
const states = signal<string[]>([]);

const readerOf = (text: string) => {
  let done = false;
  return {
    getReader: () => ({
      read: async () => {
        if (done) return { done: true, value: undefined };
        done = true;
        return { done: false, value: new Uint8Array([...text].map((c) => c.charCodeAt(0))) };
      },
      cancel: async () => {},
      releaseLock: () => {},
    }),
  };
};

const http = new KeelHttp("http://keel.test", (async (input: unknown) => {
  const url = typeof input === "string" ? input : String((input as { url?: string }).url ?? "");
  if (!url.includes("/events/stream")) {
    return { ok: true, status: 200, text: async () => "{}", json: async () => ({}), body: null };
  }
  const n = attempts() + 1;
  attempts.set(n);
  if (n <= 2) {
    return { ok: false, status: 404, text: async () => '{"error":"session not found"}', json: async () => ({ error: "session not found" }), body: null };
  }
  return {
    ok: true,
    status: 200,
    text: async () => "",
    json: async () => ({}),
    body: readerOf('data: {"seq":1,"kind":"user.message","payload":{"text":"hi"}}\n\n'),
  };
}) as unknown as typeof fetch);

function Harness() {
  const source = http.source("11111111-1111-1111-1111-111111111111");
  source.stream({
    after: 0,
    onEvent: () => events.update((n) => n + 1),
    onState: (next) => { state.set(next); lastState.set(next); states.update((l) => [...l, next]); },
  });
  return (
    <View style={{ flex: 1 }}>
      <Text testID="state">{state()}</Text>
      <Text testID="events">{String(events())}</Text>
      <Text testID="attempts">{String(attempts())}</Text>
      <Text testID="log">{states().join(",")}</Text>
    </View>
  );
}

mount(Harness);
