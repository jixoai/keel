// TUI gate for the settings「密钥」section (change keel-model-catalog A/paint).
//
// The section must show the masked preview of each stored credential, not just
// its name: `first6…last6` for a long one, the fixed mask for a short one and
// 「已保存」 for a row that predates the preview column. The value is never
// rendered; a write clears the input and the list keeps showing only the mask.
import { expect, open, test } from "nsh/test";

const HARNESS = "./settings-secrets.harness.tsx";

const openSecrets = (width?: number) =>
  open(HARNESS, { ...(width ? { width } : {}), host: { params: { fixture: true } } });

test("TUI: 密钥——每行显示掩码预览；老行（没有预览）显示「已保存」", async () => {
  const app = await openSecrets();
  await app.waitFor("id:settings-secrets");

  const text = app.texts().join("\n");
  // The long credential's masked form (first6…last6) is the server's preview.
  expect(text).toContain("sk-liv…-a1b2-c");
  expect(app.shows("id:secret-preview-s2api")).toBe(true);
  // The short credential keeps the fixed mask only.
  expect(app.shows("id:secret-preview-webhook-token")).toBe(true);
  // A row from before the preview column says 已保存 instead of inventing one.
  expect(app.shows("id:secret-preview-legacy-env-key")).toBe(true);
  const legacyRow = app.texts().filter((row) => row === "已保存");
  expect(legacyRow.length).toBeGreaterThanOrEqual(1);
  // Never a value: the seeded plaintext-ish token is absent from the screen.
  expect(text).not.toContain("sk-live");
});

test("TUI: 密钥——写入后输入框清空，列表只多一个掩码行", async () => {
  const app = await openSecrets();
  await app.waitFor("id:secret-name");

  await app.type("id:secret-name", "fresh-key");
  await app.type("id:secret-value", "sk-live-0123456789-abcdefghijklmnop");
  await app.press("label:保存密钥");

  await app.waitFor("id:secret-preview-fresh-key");
  expect(app.value("id:secret-value")).toBe("");
  expect(app.value("id:secret-name")).toBe("fresh-key");
  const text = app.texts().join("\n");
  expect(text).toContain("sk-liv…klmnop");
  expect(text).not.toContain("sk-live-0123456789-abcdefghijklmnop");
});

test("TUI: 密钥——390 宽度下名字、预览与时间仍在同一行内可读", async () => {
  const app = await openSecrets(390);
  await app.waitFor("id:settings-secrets");
  const text = app.texts().join("\n");
  expect(text).toContain("s2api");
  expect(text).toContain("sk-liv…-a1b2-c");
  expect(text).toContain("创建");
  expect(app.shows("id:secret-preview-s2api")).toBe(true);
});
