// MIME narrowing for attachment preview claims (change keel-plugin-attachment,
// task 3.1). The grammar and the matcher are pinned together: a malformed
// pattern is never a wildcard, and an absent list is the wildcard only for a
// contribution the registry already identified as `part=attachment`.
import { expect, test } from "nsh/test";
import { mimeMatches, mimePatternProblem, normalizeMime } from "./mime";

test("normalizeMime: lowercase, parameters dropped, trimmed", () => {
  expect(normalizeMime(" Image/PNG ; charset=binary ")).toBe("image/png");
  expect(normalizeMime(undefined)).toBe("");
});

test("MIME grammar: type/subtype and type/* only, lowercase", () => {
  expect(mimePatternProblem("image/png")).toBe("");
  expect(mimePatternProblem("image/*")).toBe("");
  expect(mimePatternProblem("application/vnd.openxmlformats-officedocument.wordprocessingml.document")).toBe("");
  expect(mimePatternProblem("")).not.toBe("");
  expect(mimePatternProblem("image")).not.toBe("");
  expect(mimePatternProblem("image/png/extra")).not.toBe("");
  expect(mimePatternProblem("*/png")).not.toBe("");
  expect(mimePatternProblem("Image/PNG")).not.toBe("");
  expect(mimePatternProblem("image/p ng")).not.toBe("");
  expect(mimePatternProblem("image/")).not.toBe("");
});

test("matching: exact and type wildcard, parameters ignored", () => {
  expect(mimeMatches(["image/png"], "image/png")).toBe(true);
  expect(mimeMatches(["image/png"], "image/png; charset=binary")).toBe(true);
  expect(mimeMatches(["image/*"], "image/webp")).toBe(true);
  expect(mimeMatches(["image/*"], "application/pdf")).toBe(false);
  expect(mimeMatches(["application/pdf", "image/*"], "image/gif")).toBe(true);
  expect(mimeMatches([], "text/plain")).toBe(true);
  expect(mimeMatches(undefined, "text/plain")).toBe(true);
});

test("matching is fail-closed: a malformed pattern matches nothing, never everything", () => {
  expect(mimeMatches([""], "image/png")).toBe(false);
  expect(mimeMatches(["*"], "image/png")).toBe(false);
  expect(mimeMatches(["*/png"], "image/png")).toBe(false);
  expect(mimeMatches(["IMAGE/PNG"], "image/png")).toBe(false);
  expect(mimeMatches(["image/png"], "")).toBe(false);
  // One bad entry does not poison a well-formed sibling.
  expect(mimeMatches(["", "image/*"], "image/png")).toBe(true);
});
