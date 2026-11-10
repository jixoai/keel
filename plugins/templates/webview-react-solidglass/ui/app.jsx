// The plugin UI: a webview package rendered inside the host at
// /plugins/<id>/. It talks to the host ONLY through the keel.plugin.* bridge
// (state / call / approve / subscribe) and renders with the host's own theme
// tokens — no local design system, no external network, no host secret.
//
// Everything it shows comes from the host: the plugin's own state document and
// the host theme token document (web/theme.json) delivered by bridge/state.
import { useCallback, useEffect, useMemo, useState } from "react";
import { createRoot } from "react-dom/client";

/** pluginIDFromPath reads the plugin id the host mounted this page under. */
function pluginIDFromPath() {
  const match = window.location.pathname.match(/^\/plugins\/([^/]+)/);
  return match ? decodeURIComponent(match[1]) : "";
}

const PLUGIN_ID = pluginIDFromPath();

function bridgeURL(verb) {
  return "/v1/plugins/" + encodeURIComponent(PLUGIN_ID) + "/bridge/" + verb;
}

async function readJSON(response) {
  const text = await response.text();
  if (!text) return {};
  try {
    return JSON.parse(text);
  } catch {
    return { raw: text };
  }
}

/** applyTheme writes the host token document onto :root as CSS variables. */
function applyTheme(theme) {
  if (!theme || !theme.cssVars) return () => {};
  const root = document.documentElement;
  const dark = window.matchMedia("(prefers-color-scheme: dark)");
  const write = () => {
    const vars = Object.assign({}, theme.cssVars.theme || {}, dark.matches ? theme.cssVars.dark || {} : theme.cssVars.light || {});
    for (const [name, value] of Object.entries(vars)) {
      root.style.setProperty("--" + name, value);
    }
  };
  write();
  dark.addEventListener("change", write);
  return () => dark.removeEventListener("change", write);
}

/** toolNames is the plugin's own callable surface, from the host's projection. */
function toolNames(state) {
  const live = Array.isArray(state.tools) ? state.tools : [];
  if (live.length > 0) return live;
  const declared = (state.provides && state.provides.tools) || [];
  return declared;
}

function Chip({ children }) {
  return <span className="chip">{children}</span>;
}

function Row({ label, value }) {
  return (
    <div className="row">
      <span className="label">{label}</span>
      <span className="value">{value}</span>
    </div>
  );
}

function App() {
  const [state, setState] = useState(null);
  const [error, setError] = useState("");
  const [result, setResult] = useState(null);
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    let disposed = false;
    let cleanupTheme = () => {};
    fetch(bridgeURL("state"), { headers: { accept: "application/json" } })
      .then(async (response) => {
        const body = await readJSON(response);
        if (!response.ok) throw new Error(body.error || "bridge/state answered " + response.status);
        if (disposed) return;
        setState(body);
        if (body.provides && body.provides.ui && body.provides.ui.title) {
          document.title = body.provides.ui.title;
        }
        cleanupTheme = applyTheme(body.theme);
      })
      .catch((failure) => {
        if (!disposed) setError(String(failure.message || failure));
      });
    return () => {
      disposed = true;
      cleanupTheme();
    };
  }, []);

  const tools = useMemo(() => (state ? toolNames(state) : []), [state]);

  const callTool = useCallback(async () => {
    if (tools.length === 0) return;
    setBusy(true);
    setError("");
    try {
      const response = await fetch(bridgeURL("call"), {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({ tool: tools[0], args: { from: "plugin-ui", at: Date.now() } }),
      });
      const body = await readJSON(response);
      if (!response.ok) throw new Error(body.error || "bridge/call answered " + response.status);
      setResult(body);
    } catch (failure) {
      setError(String(failure.message || failure));
    } finally {
      setBusy(false);
    }
  }, [tools]);

  if (error && !state) {
    return (
      <main className="panel">
        <h1>不可用</h1>
        <p className="muted">{error}</p>
      </main>
    );
  }
  if (!state) {
    return (
      <main className="panel">
        <p className="muted">正在连接宿主…</p>
      </main>
    );
  }

  return (
    <main className="panel">
      <header className="head">
        <div>
          <h1>{(state.provides && state.provides.ui && state.provides.ui.title) || state.id}</h1>
          <p className="muted">{state.description || "插件面板"}</p>
        </div>
        <span className={"state state-" + state.state}>{state.state}</span>
      </header>

      <section className="card">
        <Row label="插件" value={state.id} />
        <Row label="版本" value={state.version || "-"} />
        <Row label="种类" value={state.kind || "-"} />
        <Row label="装配代" value={String(state.generation ?? "-")} />
        <Row label="清单版本" value={String(state.revision ?? "-")} />
      </section>

      <section className="card">
        <div className="tools">
          <span className="label">工具</span>
          <div className="chips">
            {tools.length === 0 ? <span className="muted">（未声明）</span> : null}
            {tools.map((tool) => (
              <Chip key={tool}>{tool}</Chip>
            ))}
          </div>
        </div>
        <div className="actions">
          <button className="button" onClick={callTool} disabled={busy || tools.length === 0}>
            {busy ? "调用中…" : tools.length > 0 ? "调用 " + tools[0] : "无工具"}
          </button>
          <span className="muted">
            桥接能力：state ✓ / call {state.bridge && state.bridge.call ? "✓" : "—"} / approve{" "}
            {state.bridge && state.bridge.approve ? "✓" : "—"} / subscribe {state.bridge && state.bridge.subscribe ? "✓" : "—"}
          </span>
        </div>
      </section>

      {error ? <p className="error">{error}</p> : null}
      {result ? (
        <section className="card">
          <span className="label">最近一次结果</span>
          <pre className="result">{JSON.stringify(result, null, 2)}</pre>
        </section>
      ) : null}
    </main>
  );
}

const container = document.getElementById("root");
if (container) {
  createRoot(container).render(<App />);
}
