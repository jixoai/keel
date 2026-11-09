// The Keel web host: what a packaged web build adds to its page.
//
// `nsh package --target web` copies this module into the site and runs
// `createHost(player)` before the app starts. The functions the app may call
// are granted by `capabilities.host` in nsh.json (`keel.*`):
//
//   keel.notify   { title, body, tag }  system notification; resolves with
//                                       the permission state ("granted",
//                                       "denied", "default", "unsupported")
//   keel.info     {}                    build/runtime diagnostics
//   keel.route    {}                    the page's route ({ path, search }):
//                                       the app has no `location` of its own,
//                                       so `/settings` deep links read it here
//   keel.navigate { path }              pushState: keeps the page URL in step
//                                       with the app's own route
//
// Events: `keel.network` { online: boolean } whenever the browser's network
// state changes; `keel.route` { path, search } on browser back/forward, so the
// back button follows the app's routes.
export async function createHost(player) {
  const route = () => {
    if (typeof location === "undefined") return { path: "/", search: "" };
    return { path: location.pathname || "/", search: location.search || "" };
  };

  const navigate = (args) => {
    const path = String(args?.path ?? "/");
    if (typeof history !== "undefined") {
      try {
        history.pushState(null, "", path);
      } catch {
        /* the page refused the entry (a cross-origin frame): the route stays in-app */
      }
    }
    return route();
  };

  const notify = async (args) => {
    if (typeof Notification === "undefined") return "unsupported";
    let permission = Notification.permission;
    if (permission === "default") {
      try {
        permission = await Notification.requestPermission();
      } catch {
        return "default";
      }
    }
    if (permission !== "granted") return permission;
    try {
      const note = new Notification(String(args?.title ?? "Keel"), {
        body: String(args?.body ?? ""),
        tag: args?.tag ? String(args.tag) : undefined,
      });
      note.onclick = () => {
        try {
          window.focus();
        } catch {
          /* the page may refuse focus */
        }
        note.close();
      };
      return "granted";
    } catch {
      return "denied";
    }
  };

  const info = () => ({
    version: String(globalThis.__KEEL_VERSION__ ?? "0.1.0"),
    online: navigator.onLine,
    userAgent: navigator.userAgent,
    origin: location.origin,
  });

  const call = async (name, args) => {
    switch (name) {
      case "keel.notify":
        return notify(args);
      case "keel.info":
        return info();
      case "keel.route":
        return route();
      case "keel.navigate":
        return navigate(args);
      default:
        throw new Error(`no host function ${name}`);
    }
  };

  const sendNetwork = () => {
    try {
      player.hostEvent("keel.network", { online: navigator.onLine });
    } catch {
      /* an event that is not JSON must not break the page */
    }
  };
  globalThis.addEventListener("online", sendNetwork);
  globalThis.addEventListener("offline", sendNetwork);
  sendNetwork();

  const sendRoute = () => {
    try {
      player.hostEvent("keel.route", route());
    } catch {
      /* same: never let an event break the page */
    }
  };
  if (typeof globalThis.addEventListener === "function") {
    globalThis.addEventListener("popstate", sendRoute);
  }

  return { call };
}
