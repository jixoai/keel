// The first-load verdict (change feat-mcp-apps-events, P6A2a follow-up): a
// `ui/initialize` sent while the document is still parsing must not make the
// subsequent first `load` look like a self-navigation.
import { expect, test } from "nsh/test";
import { frameLoadVerdict } from "./frame-load";

test("首载判定：loads=0 + idle → start", () => {
  expect(frameLoadVerdict(0, "idle", false)).toBe("start");
});

test("首载判定：loads=0 + handshaking/initialized（pre-load initialize）→ start", () => {
  // The regression: the app initialized before the frame's load event.
  expect(frameLoadVerdict(0, "handshaking", false)).toBe("start");
  expect(frameLoadVerdict(0, "ready", true)).toBe("start");
});

test("首载判定：loads=1 + handshaking/initialized → reload", () => {
  expect(frameLoadVerdict(1, "handshaking", false)).toBe("reload");
  expect(frameLoadVerdict(1, "ready", true)).toBe("reload");
});

test("首载判定：loads=1 + idle → start（display-mode 重建后的首个 load）", () => {
  expect(frameLoadVerdict(1, "idle", false)).toBe("start");
});

test("首载判定：destroyed → 不启动也不重载", () => {
  expect(frameLoadVerdict(0, "destroyed", false)).toBe("ignore");
  expect(frameLoadVerdict(1, "destroyed", true)).toBe("ignore");
});
