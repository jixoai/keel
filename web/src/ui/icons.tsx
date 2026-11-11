// Monochrome line icons on a 24×24 grid, drawn with Canvas paths (lucide's
// visual language: 1.8pt strokes, round caps and joins, no fills except the
// stop square and status dots). The tint is always a theme token, so the app
// never introduces a second icon color language.
import { Canvas, type CanvasRenderingContext2D, Path2D } from "nsh";

export type IconName =
  | "plus"
  | "search"
  | "trash"
  | "chevron-left"
  | "chevron-down"
  | "chevron-right"
  | "arrow-up"
  | "arrow-down"
  | "stop"
  | "paperclip"
  | "x"
  | "refresh"
  | "check"
  | "alert"
  | "clock"
  | "tool"
  | "brain"
  | "file"
  | "image"
  | "dots"
  | "wifi"
  | "wifi-off"
  | "sun"
  | "moon"
  | "message"
  | "panel"
  | "spark"
  | "terminal"
  | "external"
  | "layers"
  | "settings"
  | "key"
  | "keyboard"
  | "info"
  | "play";

const paths: Record<Exclude<IconName, "stop" | "dots">, string[]> = {
  plus: ["M5 12h14", "M12 5v14"],
  search: ["M11 19a8 8 0 1 0 0-16 8 8 0 0 0 0 16z", "M21 21l-4.35-4.35"],
  trash: ["M3 6h18", "M8 6V4h8v2", "M19 6l-1 14H6L5 6", "M10 11v6", "M14 11v6"],
  "chevron-left": ["M15 18l-6-6 6-6"],
  "chevron-down": ["M6 9l6 6 6-6"],
  "chevron-right": ["M9 18l6-6-6-6"],
  "arrow-up": ["M12 19V5", "M5 12l7-7 7 7"],
  "arrow-down": ["M12 5v14", "M19 12l-7 7-7-7"],
  paperclip: [
    "M20.5 11.5l-8.6 8.6a6 6 0 0 1-8.5-8.5l8.6-8.6a4 4 0 0 1 5.7 5.7l-8.6 8.6a2 2 0 0 1-2.8-2.8l8.5-8.5",
  ],
  x: ["M18 6L6 18", "M6 6l12 12"],
  refresh: ["M21 12a9 9 0 1 1-3-6.7", "M21 3v6h-6"],
  check: ["M20 6L9 17l-5-5"],
  alert: ["M12 21a9 9 0 1 0 0-18 9 9 0 0 0 0 18z", "M12 8v4", "M12 16h.01"],
  clock: ["M12 21a9 9 0 1 0 0-18 9 9 0 0 0 0 18z", "M12 7v5l3 2"],
  tool: ["M14.7 6.3a4 4 0 0 0-5.4 5.4L3 18l3 3 6.3-6.3a4 4 0 0 0 5.4-5.4l-2.8 2.8-2.1-2.1 2.9-2.7z"],
  brain: [
    "M10 4.5C5.5 2.5 2.5 7 4.5 10c-2 3.5 1.5 7 5.5 5.5 4 1.5 7.5-2 5.5-5.5 2-3-1-6.5-5.5-4.5",
    "M10 4.5v11",
  ],
  file: ["M5 3h7l3.5 3.5V21H5V3z", "M12 3v3.5h3.5"],
  image: ["M4 4h16v16H4V4z", "M8.5 10a1.5 1.5 0 1 0 0-3 1.5 1.5 0 0 0 0 3z", "M4 17l5-5 3 3 3-3 5 5"],
  wifi: ["M2.5 9a15 15 0 0 1 19 0", "M6 12.5a10 10 0 0 1 12 0", "M9.5 16a5 5 0 0 1 5 0", "M12 19h.01"],
  "wifi-off": ["M2.5 9a15 15 0 0 1 6.5-3.2", "M15.5 6.2A15 15 0 0 1 21.5 9", "M6 12.5a10 10 0 0 1 3-1.6", "M16 11.5a10 10 0 0 1 2 1", "M9.5 16a5 5 0 0 1 5 0", "M2 2l20 20"],
  sun: ["M12 16a4 4 0 1 0 0-8 4 4 0 0 0 0 8z", "M12 2v2", "M12 20v2", "M4.9 4.9l1.4 1.4", "M17.7 17.7l1.4 1.4", "M2 12h2", "M20 12h2", "M4.9 19.1l1.4-1.4", "M17.7 6.3l1.4-1.4"],
  moon: ["M21 12.8A9 9 0 1 1 11.2 3a7 7 0 0 0 9.8 9.8z"],
  message: ["M21 15a2 2 0 0 1-2 2H7l-4 4V5a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2v10z"],
  panel: ["M3 3h18v18H3V3z", "M9 3v18"],
  spark: ["M12 3l1.8 5.2L19 10l-5.2 1.8L12 17l-1.8-5.2L5 10l5.2-1.8L12 3z", "M19 16l.9 2.1L22 19l-2.1.9L19 22l-.9-2.1L16 19l2.1-.9L19 16z"],
  terminal: ["M4 5h16v14H4V5z", "M8 9l3 3-3 3", "M13 15h4"],
  external: ["M14 3h7v7", "M21 3l-9 9", "M21 14v5a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h5"],
  layers: ["M12 3l9 5-9 5-9-5 9-5z", "M3 13l9 5 9-5", "M3 18l9 5 9-5"],
  // lucide's `settings`, `key` and `play`.
  settings: [
    "M12.22 2h-.44a2 2 0 0 0-2 2v.18a2 2 0 0 1-1 1.73l-.43.25a2 2 0 0 1-2 0l-.15-.08a2 2 0 0 0-2.73.73l-.22.38a2 2 0 0 0 .73 2.73l.15.1a2 2 0 0 1 1 1.72v.51a2 2 0 0 1-1 1.74l-.15.09a2 2 0 0 0-.73 2.73l.22.38a2 2 0 0 0 2.73.73l.15-.08a2 2 0 0 1 2 0l.43.25a2 2 0 0 1 1 1.73V20a2 2 0 0 0 2 2h.44a2 2 0 0 0 2-2v-.18a2 2 0 0 1 1-1.73l.43-.25a2 2 0 0 1 2 0l.15.08a2 2 0 0 0 2.73-.73l.22-.39a2 2 0 0 0-.73-2.73l-.15-.08a2 2 0 0 1-1-1.74v-.5a2 2 0 0 1 1-1.74l.15-.09a2 2 0 0 0 .73-2.73l-.22-.38a2 2 0 0 0-2.73-.73l-.15.08a2 2 0 0 1-2 0l-.43-.25a2 2 0 0 1-1-1.73V4a2 2 0 0 0-2-2z",
    "M12 15a3 3 0 1 0 0-6 3 3 0 0 0 0 6z",
  ],
  key: ["M21 2l-9.6 9.6", "M15.5 7.5l3 3L22 7l-3-3", "M7.5 21a5.5 5.5 0 1 0 0-11 5.5 5.5 0 0 0 0 11z"],
  // lucide's `keyboard` (a key row, the space bar) and `info` (a circled i).
  keyboard: [
    "M4 4h16a2 2 0 0 1 2 2v12a2 2 0 0 1-2 2H4a2 2 0 0 1-2-2V6a2 2 0 0 1 2-2z",
    "M6 8h.01",
    "M10 8h.01",
    "M14 8h.01",
    "M18 8h.01",
    "M8 12h.01",
    "M12 12h.01",
    "M16 12h.01",
    "M7 16h10",
  ],
  info: ["M12 21a9 9 0 1 0 0-18 9 9 0 0 0 0 18z", "M12 11v5", "M12 8h.01"],
  play: ["M6 3l14 9-14 9V3z"],
};

export function Icon(props: { name: IconName; size?: number; tint: string }) {
  const size = props.size ?? 18;
  return (
    <Canvas
      style={{ pointerEvents: "none", width: size, height: size }}
      draw={(cx: CanvasRenderingContext2D, { width }) => {
        cx.scale(width / 24, width / 24);
        cx.strokeStyle = props.tint;
        cx.fillStyle = props.tint;
        cx.lineWidth = 1.8;
        cx.lineCap = "round";
        cx.lineJoin = "round";
        if (props.name === "stop") {
          cx.beginPath();
          cx.roundRect(6, 6, 12, 12, 2.5);
          cx.fill();
          return;
        }
        if (props.name === "dots") {
          for (const x of [5, 12, 19]) {
            cx.beginPath();
            cx.arc(x, 12, 1.6, 0, Math.PI * 2);
            cx.fill();
          }
          return;
        }
        for (const path of paths[props.name]) {
          cx.stroke(new Path2D(path));
        }
      }}
    />
  );
}
