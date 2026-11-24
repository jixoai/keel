// The desktop shell: single-instance wake-up, keel:// links, the tray icon and
// the global wake-up shortcut. Everything here is absent on the web and in a
// terminal (`isAvailable` is false), so the app keeps one code path.
//
// The shell never navigates by itself: it parses a link and hands a valid
// KeelLink to the app's handler. An invalid link is reported, not followed.

import { UrlScheme } from "nsh";
import { Application, GlobalShortcut, isAvailable, Tray, windows } from "nsh/desktop";
import { type KeelLink, parseKeelLink } from "./links";

export interface DesktopShellHandlers {
  openLink(link: KeelLink): void;
  invalidLink(raw: string): void;
}

const WAKE_SHORTCUT = "mod+shift+k";

const focusMain = () => {
  const win = windows()[0];
  if (!win) return;
  win.show();
  win.focus();
};

/** Installs the desktop-only listeners; returns a cleanup for a host that
 * embeds the app rather than running it standalone. */
export function installDesktopShell(handlers: DesktopShellHandlers): () => void {
  if (!isAvailable) return () => {};

  const dispatch = (raw: string) => {
    const link = parseKeelLink(raw);
    if (!link) handlers.invalidLink(raw);
    else handlers.openLink(link);
  };

  Application.setQuitMode("explicit");
  const offReopen = Application.on("reopen", focusMain);
  const offSecond = Application.on("secondInstance", (launch) => {
    focusMain();
    const link = launch.args.find((arg) => arg.startsWith("keel://"));
    if (link) dispatch(link);
  });
  const offLink = UrlScheme.on("open", ({ url }) => {
    focusMain();
    dispatch(url);
  });

  Tray.set({
    tooltip: "Keel",
    menu: [
      { label: "显示 Keel", onSelect: focusMain },
      { label: "设置", onSelect: () => handlers.openLink({ kind: "settings" }) },
      { type: "separator" },
      { role: "quit", label: "退出 Keel" },
    ],
    onClick: focusMain,
  });
  void GlobalShortcut.register(WAKE_SHORTCUT, focusMain).catch(() => {
    /* another app may hold it; the tray and the window still work */
  });

  return () => {
    offReopen();
    offSecond();
    offLink();
    GlobalShortcut.unregisterAll();
    Tray.set(null);
  };
}
