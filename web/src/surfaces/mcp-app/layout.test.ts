// Fullscreen layout choices (P6V), pinned as values: the dialog covers the
// modal layer, and the WebView grows with the panel instead of keeping the
// inline height the app reported before the mode change.
import { expect, test } from "nsh/test";
import { INLINE_MIN_HEIGHT } from "./bridge";
import { frameStyle, frameWrapperStyle, fullscreenDialogStyle } from "./layout";

test("全屏弹层：Dialog 占满模态层（100%，不再 92% x 88% 露背景）", () => {
  expect(fullscreenDialogStyle()).toEqual({
    width: "100%",
    maxWidth: "100%",
    height: "100%",
    maxHeight: "100%",
  });
});

test("全屏 frame：wrapper 与 WebView 伸展；内联沿用 app 上报高度", () => {
  const fullWrapper = frameWrapperStyle(true);
  expect(fullWrapper.flexDirection).toBe("column");
  expect(fullWrapper.flex).toBe(1);
  expect(fullWrapper.minHeight).toBe(0);

  const inlineWrapper = frameWrapperStyle(false);
  expect(inlineWrapper.flex).toBe(undefined);
  expect(inlineWrapper.minHeight).toBe(undefined);

  // The app said 355 while inline; fullscreen ignores that number.
  const fullFrame = frameStyle(true, 355);
  expect(fullFrame.height).toBe("100%");
  expect(fullFrame.flex).toBe(1);
  expect(fullFrame.minHeight).toBe(0);

  const inlineFrame = frameStyle(false, 355);
  expect(inlineFrame.height).toBe(355);
  expect(inlineFrame.flex).toBe(undefined);
  expect(inlineFrame.minHeight).toBe(INLINE_MIN_HEIGHT);
});
