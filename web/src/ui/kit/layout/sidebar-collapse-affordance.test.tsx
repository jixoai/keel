// Permanent case sunk from the dbg10/dbg11 experiments (the dbg* files are gone).
//
// The behavior it pins: with the shell (not a bare `Sidebar` in a panel group)
// a collapsed sidebar in the terminal is reachable — the shell's own strip
// carries `id:sidebar-collapse-strip` and `label:展开侧栏`, it is pressable, and
// pressing it brings the sidebar back. A bare `Sidebar` renders no such entry by
// design (sidebar.tsx: the shell draws the strip; nsh 0.4.0 gives a hairline
// panel's children no layout in a group of three or more), which is what the
// deleted dbg10 asserted by mistake.
import { expect, open, test } from "nsh/test";

test("TUI: 侧栏收起后唯一入口是 shell 的收起条（id:sidebar-collapse-strip / label:展开侧栏），按下即展开", async () => {
  const app = await open("./sidebar-collapse-affordance.fixture.tsx", { width: 900, height: 400 });

  // Expanded: the sidebar shows its content, the strip does not exist.
  expect(app.shows("id:fixture-sidebar-content")).toBe(true);
  expect(app.count("id:sidebar-collapse-strip")).toBe(0);

  // Collapse through the app's own control.
  await app.press("label:收起侧栏");
  await app.waitFor("id:sidebar-collapse-strip");
  expect(app.count("id:sidebar-collapse-strip")).toBe(1);
  expect(app.count("label:展开侧栏")).toBe(1);
  expect(app.shows("id:fixture-sidebar-content")).toBe(false);

  // The strip is the entry back: pressable, and it expands the sidebar again.
  await app.press("label:展开侧栏");
  await app.waitFor("id:fixture-sidebar-content");
  expect(app.count("id:sidebar-collapse-strip")).toBe(0);
});
