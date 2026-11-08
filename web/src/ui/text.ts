// Text budget helpers for the terminal grid. The TUI lays text out in cells
// and does not clamp a flex child the way the web does, so rows that must
// stay on one line are measured here: CJK and fullwidth glyphs take two cells,
// everything else one. Truncation lands on a boundary when one is near the
// cut, and always ends with an ellipsis so a clipped line never reads as a
// complete sentence.

const WIDE = (code: number): boolean =>
  (code >= 0x1100 && code <= 0x115f) ||
  (code >= 0x2e80 && code <= 0xa4cf) ||
  (code >= 0xac00 && code <= 0xd7a3) ||
  (code >= 0xf900 && code <= 0xfaff) ||
  (code >= 0xfe10 && code <= 0xfe19) ||
  (code >= 0xfe30 && code <= 0xfe6f) ||
  (code >= 0xff00 && code <= 0xff60) ||
  (code >= 0xffe0 && code <= 0xffe6) ||
  (code >= 0x1f300 && code <= 0x1faff);

export const cellWidth = (text: string): number => {
  let width = 0;
  for (const char of text) width += WIDE(char.codePointAt(0) ?? 0) ? 2 : 1;
  return width;
};

const isBoundary = (char: string | undefined): boolean =>
  char === undefined || char === " " || char === "，" || char === "。" || char === "、" ||
  char === "；" || char === "：" || char === "！" || char === "？" || char === "/" ||
  char === "," || char === "." || char === ";" || char === ":";

/** `text` cut to at most `max` cells, with an ellipsis when it was cut.
 * Prefers cutting after a word/punctuation boundary within four cells of the
 * limit; falls back to a hard cut. */
export function fitCells(text: string, max: number): string {
  if (max <= 0) return "";
  if (cellWidth(text) <= max) return text;
  const limit = Math.max(1, max - 1);
  let out = "";
  let width = 0;
  let lastBoundary = -1;
  for (const char of text) {
    const w = WIDE(char.codePointAt(0) ?? 0) ? 2 : 1;
    if (width + w > limit) break;
    out += char;
    width += w;
    if (isBoundary(char)) lastBoundary = out.length;
  }
  if (lastBoundary >= 0 && cellWidth(out.slice(lastBoundary)) <= 4) {
    out = out.slice(0, lastBoundary);
  }
  return `${out.replace(/\s+$/, "")}…`;
}

/** The continuation marker for a clipped streaming line (terminal only; the
 * web uses its streaming caret). */
export const CONTINUATION = "… 仍在输入";

const CLOSING = new Set(["。", "，", "、", "；", "：", "！", "？", "…", "）", "」", "』", "】", ".", ",", ";", ":", "!", "?"]);

/** Greedy wrap by display cells for a terminal paragraph, with a widow guard:
 * a wrapped line never starts with closing punctuation and never holds a
 * single character alone. Newlines in the input become paragraph breaks. */
export function wrapCells(text: string, max: number): string {
  const lines: string[] = [];
  for (const paragraph of text.split("\n")) {
    if (!paragraph) {
      lines.push("");
      continue;
    }
    const marker = paragraph.match(/^(\d+\.\s|[-*]\s)/);
    const indent = marker ? " ".repeat(cellWidth(marker[0])) : "";
    let line = "";
    let width = 0;
    for (const char of paragraph) {
      const w = WIDE(char.codePointAt(0) ?? 0) ? 2 : 1;
      if (line && width + w > max) {
        lines.push(line);
        line = indent + char;
        width = cellWidth(indent) + w;
      } else {
        line += char;
        width += w;
      }
    }
    if (line) lines.push(line);
  }
  for (let index = 1; index < lines.length; index += 1) {
    if (!lines[index] || !lines[index - 1]) continue;
    const indent = lines[index].match(/^ */)?.[0] ?? "";
    const body = lines[index].slice(indent.length);
    const first = [...body][0];
    if (CLOSING.has(first) || cellWidth(body) <= 4) {
      const previous = [...lines[index - 1]];
      const take = cellWidth(body) <= 2 ? Math.min(2, previous.length) : 1;
      const moved = previous.splice(previous.length - take, take).join("");
      if (moved) {
        lines[index - 1] = previous.join("");
        lines[index] = `${indent}${moved}${body}`;
      }
    }
  }
  return lines.join("\n");
}
