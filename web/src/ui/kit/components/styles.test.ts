// The control-size ladder — the one number every toolbar, filter row and form
// control reads. `field` is the product's shell height (34px); `sm`/`default`/
// `lg` stay the kit's dense ladder (24/28/32). The 44px touch floor is a call
// site override (a phone passes `height`/`height={44}`), never a second ladder.
import { expect, test } from "nsh/test";
import { controlHeight, controlIconSize, controlPadding, controlRadius, withoutUndefined } from "./styles";
import { radius, space } from "../tokens";

test("kit 尺寸：field 是 34px，其余仍是 24/28/32 的密集阶梯", () => {
  expect(controlHeight("sm")).toBe(24);
  expect(controlHeight("default")).toBe(28);
  expect(controlHeight("lg")).toBe(32);
  expect(controlHeight("field")).toBe(34);
});

test("kit 尺寸：field 的半径与内边距跟 default 同一语言", () => {
  expect(controlRadius("field")).toBe(radius.md);
  expect(controlPadding("field")).toBe(space.md);
  expect(controlIconSize("field")).toBe(14);
});

test("withoutUndefined：条件值为空时不抹掉组件的默认高度", () => {
  const style = withoutUndefined({ height: undefined, width: 120, flex: 1 });
  expect(style).toEqual({ width: 120, flex: 1 });
  const nested = withoutUndefined([{ height: 34 }, { height: undefined, flex: 1 }]);
  expect(nested).toEqual([{ height: 34 }, { flex: 1 }]);
  expect(withoutUndefined(undefined)).toBe(undefined);
  expect(withoutUndefined(false)).toBe(false);
});
