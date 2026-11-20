// Time stamps (P6V): the event log's missing/epoch times are data, not a
// date. A 0 or a pre-2000 timestamp must read as empty, never as 1970.
import { expect, test } from "nsh/test";
import { formatTime } from "./wire";

const NOW = new Date(2026, 10, 20, 12, 0, 0).getTime();

test("时间：0 / 负数 / 2000 年前 / 非法值都不渲染", () => {
  expect(formatTime(0, NOW)).toBe("");
  expect(formatTime(-1, NOW)).toBe("");
  expect(formatTime(Date.parse("1999-12-31T23:59:59Z"), NOW)).toBe("");
  expect(formatTime(Date.parse("1970-01-01T00:00:00Z"), NOW)).toBe("");
  expect(formatTime(undefined, NOW)).toBe("");
  expect(formatTime("", NOW)).toBe("");
  expect(formatTime("not-a-time", NOW)).toBe("");
});

test("时间：最近一小时相对，更早绝对", () => {
  expect(formatTime(NOW, NOW)).toBe("刚刚");
  expect(formatTime(NOW - 59_000, NOW)).toBe("刚刚");
  expect(formatTime(NOW - 60_000, NOW)).toBe("1 分钟前");
  expect(formatTime(NOW - 59 * 60_000, NOW)).toBe("59 分钟前");
  // One hour is the first absolute stamp (not "60 分钟前").
  expect(formatTime(NOW - 60 * 60_000, NOW)).toBe("11月20日 11:00");
});

test("时间：跨年时带年份", () => {
  const newYear = new Date(2026, 0, 2, 0, 0, 0).getTime();
  const lastYear = new Date(2025, 11, 31, 23, 30, 0).getTime();
  expect(formatTime(lastYear, newYear)).toBe("2025年12月31日 23:30");
});
