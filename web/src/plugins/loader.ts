// The chunk loader (W12): NSH's dynamic import, same VM.
//
// A `chunk` contribution is an NSH module the host imports and renders in its
// own tree. Two paths exist:
//
//   · a chunk shipped with this build (a fixture, or a first-party panel) is a
//     static `import()` in `bundled.ts`, so dev hosts and the TUI bundle can
//     resolve it without a fetch;
//   · a runtime-installed plugin's chunk is asked of NSH's own dynamic import
//     (`__nsh_import`). The browser resolves it relative to the app's entry, so
//     `/plugins/<id>/...` is a same-origin module; QuickJS (native/TUI) only
//     accepts a plain chunk name that the bundle or the host's chunk directory
//     already holds. A chunk that cannot be resolved there is an explicit
//     `ERR_TARGET_UNAVAILABLE`, never a blank panel.
import { currentPluginTarget } from "./forms";
import type { PluginChunkModule } from "./types";
import { BUNDLED_CHUNKS } from "./bundled";

export interface ChunkLoadResult {
  module?: PluginChunkModule;
  code?: string;
  message?: string;
}

interface ChunkImporter {
  __nsh_import?: (id: string, needs?: string[]) => Promise<unknown>;
}

/** QuickJS's own chunk-name rule (`nsh-js chunks.rs is_chunk_name`): a plain
 * file name, nothing that could address another path. */
const PLAIN_CHUNK = /^[^./\\:?#%\u0000-\u001f][^/\\:?#%\u0000-\u001f]*\.js$/;

/** The module specifier a `chunk` entry names. */
export function chunkSpecifier(entry: string, pluginId: string): string {
  const value = entry.trim();
  if (value.startsWith("fixture:") || value.startsWith("/") || /^https?:\/\//i.test(value)) return value;
  return `/plugins/${encodeURIComponent(pluginId)}/${value.replace(/^\.\//, "")}`;
}

/** Import one chunk and check its contract. The caller turns the result into a
 * contribution status, never into a thrown host error. */
export async function loadChunkModule(entry: string, pluginId: string): Promise<ChunkLoadResult> {
  const bundled = BUNDLED_CHUNKS[entry];
  if (bundled) {
    try {
      const module = await bundled();
      return checkChunk(module);
    } catch (error) {
      return {
        code: "ERR_CHUNK_LOAD",
        message: `chunk「${entry}」加载失败：${error instanceof Error ? error.message : String(error)}`,
      };
    }
  }

  const target = currentPluginTarget();
  const specifier = chunkSpecifier(entry, pluginId);
  if (target !== "web" && !PLAIN_CHUNK.test(specifier)) {
    return {
      code: "ERR_TARGET_UNAVAILABLE",
      message:
        target === "tui"
          ? `终端只能加载随应用构建的 chunk；「${entry}」需要图形界面或重新构建。`
          : `原生 target 只能加载 chunk 目录里的平铺文件名；「${entry}」不是这样的名字。`,
    };
  }
  const importer = (globalThis as ChunkImporter).__nsh_import;
  if (typeof importer !== "function") {
    return {
      code: "ERR_UNSUPPORTED",
      message: "这个宿主没有 NSH 动态导入（__nsh_import），chunk 贡献无法装载。",
    };
  }
  try {
    return checkChunk(await importer(specifier, []));
  } catch (error) {
    const message = error instanceof Error ? error.message : String(error);
    if (/is not a chunk of this app/i.test(message) || /load_chunk|chunk/i.test(message)) {
      return {
        code: "ERR_TARGET_UNAVAILABLE",
        message:
          target === "tui"
            ? `chunk「${entry}」不在终端 bundle 里（终端不会从插件目录取代码）。`
            : `chunk「${entry}」无法从宿主取到：${message}`,
      };
    }
    return { code: "ERR_CHUNK_LOAD", message: `chunk「${entry}」加载失败：${message}` };
  }
}

function checkChunk(value: unknown): ChunkLoadResult {
  if (!value || typeof value !== "object") {
    return { code: "ERR_CHUNK_LOAD", message: "chunk 没有导出模块对象。" };
  }
  const module = value as PluginChunkModule;
  if (typeof module.render !== "function") {
    return { code: "ERR_CHUNK_LOAD", message: "chunk 必须导出 render(context) 函数。" };
  }
  return { module };
}
