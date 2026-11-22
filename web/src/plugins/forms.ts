// Form capabilities and target fallback (W12).
//
// The four forms are not interchangeable: a chunk shares the host VM (trusted
// by assembly, cheapest, works in every target including the terminal), a
// package is its own VM/surface (never in a terminal), an iframe is a browser
// frame (never in a terminal, never native-themed), and data is JSON with no
// VM. A target that cannot run a form says so explicitly; it never renders a
// blank hole (spec: plugin-ui-extension, "目标降级").
import { platform } from "nsh";
import type { SlotForm, SlotTarget } from "./types";

/** The target this app is running as. The terminal is its own target (the
 * spec makes it first class); a browser is `web`; everything else packaged by
 * NSH is `native`. */
export function currentPluginTarget(): SlotTarget {
  if (platform.idiom === "terminal") return "tui";
  if (platform.os === "web") return "web";
  return "native";
}

export interface FormFacts {
  /** What runs the contribution. */
  runtime: string;
  /** The isolation boundary (chunk: none, by design). */
  isolation: "none" | "vm" | "browser";
  /** Whether it is themed by the host's tokens. */
  themed: boolean;
  /** Targets where the form can run at all. */
  targets: SlotTarget[];
}

export const FORM_FACTS: Record<SlotForm, FormFacts> = {
  chunk: { runtime: "NSH 动态导入（同宿主 VM）", isolation: "none", themed: true, targets: ["web", "native", "tui"] },
  package: { runtime: "NSH 签名包（独立 surface + HostLimits）", isolation: "vm", themed: true, targets: ["web", "native"] },
  iframe: { runtime: "iframe / WebView（插件自托管页面）", isolation: "browser", themed: false, targets: ["web", "native"] },
  data: { runtime: "宿主消费的 JSON（无 VM）", isolation: "none", themed: true, targets: ["web", "native", "tui"] },
};

export interface FormAvailability {
  available: boolean;
  code?: string;
  /** The sentence a slot shows (explicit degradation). */
  reason?: string;
}

/** Whether a form can run on a target in this client, and why not. */
export function formAvailability(form: SlotForm, target: SlotTarget): FormAvailability {
  const facts = FORM_FACTS[form];
  if (!facts) {
    return { available: false, code: "ERR_FORM_NOT_ALLOWED", reason: `未知的贡献形态「${form}」。` };
  }
  if (!facts.targets.includes(target)) {
    return {
      available: false,
      code: "ERR_TARGET_UNAVAILABLE",
      reason:
        target === "tui"
          ? `${form} 贡献在终端中不可用（需要图形界面）；可以在浏览器里打开。`
          : `${form} 贡献不支持 ${target} 目标。`,
    };
  }
  if (form === "package") {
    // The client does not ship the independent-surface runner yet. Saying so is
    // the honest capability state; a fake panel is not.
    return {
      available: false,
      code: "ERR_UNSUPPORTED",
      reason: "签名包表面（独立 VM）尚未在本客户端接线；已登记为 W12 的显式缺口。",
    };
  }
  return { available: true };
}

/** The slot's own target support, checked before a contribution's form. */
export function slotTargetAvailable(slotTargets: SlotTarget[], target: SlotTarget): FormAvailability {
  if (slotTargets.includes(target)) return { available: true };
  return {
    available: false,
    code: "ERR_TARGET_UNAVAILABLE",
    reason:
      target === "tui"
        ? "这个槽位在终端中不提供（需要图形界面）。"
        : `这个槽位不支持 ${target} 目标。`,
  };
}
