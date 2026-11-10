// The gallery's own TUI test: the same KitGallery.tsx on NSH's terminal host.
//
// It asserts the one thing the door promises at every stage of the kit's
// landing: every manifest entry is on the page, either rendered by a real
// component or marked with its own placeholder ("missing: <name>" /
// "compound: <name>"). The web side of the same assertion is the gate's
// three-viewport text check (scripts/kit-gate.sh).
import { expect, open, test } from "nsh/test";
import { KIT_MANIFEST } from "./kit-manifest";

test("TUI: gallery renders the grid and every manifest entry is accounted for", async () => {
  const app = await open("./App.tsx");
  const page = app.texts().join("\n");
  expect(page).toContain("Keel UI Kit Gallery");
  for (const entry of KIT_MANIFEST) {
    const accounted =
      page.includes(entry.name) ||
      page.includes(`missing: ${entry.name}`) ||
      page.includes(`compound: ${entry.name}`);
    expect(`${entry.name}: ${accounted}`).toContain("true");
  }
});
