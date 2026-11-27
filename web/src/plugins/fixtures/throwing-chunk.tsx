// A deliberately broken chunk for the attachment fallback tests (change
// keel-plugin-attachment, task 3.4). The host must catch the throw, keep the
// default attachment line and leave every other row alone.
import type { JSX } from "nsh";
import type { PluginChunkContext } from "../types";

export function render(_context: PluginChunkContext): JSX.Element {
  throw new Error("附件预览渲染器故意抛错（测试夹具）");
}
