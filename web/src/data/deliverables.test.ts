// The deliverables row's formatting — change keel-harness-v2, W12
// `keel.deliverables`. The row's numbers are the manifest's; these helpers only
// shorten their display, never recompute them.
import { expect, test } from "nsh/test";
import { deliverableSize, shortDeliverableHash } from "./deliverables";

test("deliverables: 哈希只在显示层截断，完整值仍是清单的那一份", () => {
  const bare = "a".repeat(64);
  const hash = `sha256:${bare}`;
  expect(shortDeliverableHash(hash)).toBe(`sha256:${"a".repeat(10)}…`);
  expect(shortDeliverableHash("sha256:abc")).toBe("sha256:abc");
  expect(shortDeliverableHash("")).toBe("");
});

test("deliverables: 大小按清单字节数排版", () => {
  expect(deliverableSize(12)).toBe("12 B");
  expect(deliverableSize(2048)).toBe("2.0 KB");
  expect(deliverableSize(2 * 1024 * 1024)).toBe("2.0 MB");
});
