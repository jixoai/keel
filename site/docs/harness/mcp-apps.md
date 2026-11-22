# MCP Apps (interactive tool results)

> **Implemented** (P1–P5; P6 acceptance is in progress). In MCP, keel is a **host/client**:
> it connects to servers, fetches the `ui://` HTML a server declares, renders it in a sandbox,
> and proxies the app's actions back to that server. This page is for users and MCP server
> authors; the runtime details and HTTP contract live in the MCP section of
> [`docs/HOST.md`](https://github.com/jixoai/keel/blob/main/docs/HOST.md).

MCP Apps is the official MCP extension (SEP-1865, extension identifier
`io.modelcontextprotocol/ui`, Stable 2026-01-26). Without it a tool can only return text.
With it, a tool result can be an interactive surface: a table, a form, a chart, a confirmation
button. The HTML is still **supplied by the server**; keel only hosts it, sandboxes it,
bridges it and audits it.

## 1. What it does

One tool call can carry one app:

```
model calls show_dashboard
  → server returns the ordinary CallToolResult (text is still there)
  → the result's details carry the app binding
  → the client mounts an iframe inside the tool result card
  → the iframe uses the postMessage bridge to call the app's own tools, send a message or open a link
```

Three constraints define the shape:

1. **Text is always present.** The app is one rendering of a result card; when it fails or is
   unsupported the client falls back to readable text.
2. **The app never reaches the server directly.** Every outbound action goes through the host,
   which checks ownership, asks for confirmation and writes audit rows.
3. **The app runs in an opaque origin.** The web iframe has no `allow-same-origin`, so it
   cannot read the host DOM, cookies or storage; the document response also carries a strict CSP.

## 2. How a server declares an app

### 2.1 The tool declares the surface

A tool points at a `ui://` resource with `_meta.ui.resourceUri`:

```json
{
  "name": "show_dashboard",
  "description": "Show the account dashboard",
  "_meta": {
    "ui": {
      "resourceUri": "ui://dashboard/main",
      "visibility": ["model", "app"]
    }
  }
}
```

- `visibility` defaults to `["model","app"]`. A tool without `"model"` **does not** appear on
  the model tool surface; a tool without `"app"` cannot be called by an app.
- Older servers may use the flat key `_meta["ui/resourceUri"]`; keel still reads it, but the
  structured `_meta.ui` wins. Both forms together create one app, not two.
- Several tools may reference the same `ui://` resource; the catalogue lists it once and
  lists every referencing tool.

### 2.2 The resource declares CSP and permissions

The app document returned by `resources/read` carries `_meta.ui`:

```json
{
  "uri": "ui://dashboard/main",
  "mimeType": "text/html;profile=mcp-app",
  "_meta": {
    "ui": {
      "csp": {
        "connectDomains": ["https://api.example.com"],
        "resourceDomains": ["https://cdn.example.com"],
        "frameDomains": [],
        "baseUriDomains": []
      },
      "permissions": { "clipboardWrite": true },
      "prefersBorder": true
    }
  }
}
```

Rules:

- `mimeType` must be `text/html;profile=mcp-app`; any other type is not a renderable app.
- One document is at most 2 MiB; one `_meta` block is at most 64 KiB; the content is `text` or
  base64 `blob`, never both.
- keel builds the CSP. A server contributes **domain lists only**; it cannot introduce a
  directive or a source such as `*`/`data:`. Without `csp`, the strict default applies: no
  external connection and no frames.
- `permissions` is parsed and published in the catalogue but **not granted** today (see §8).
- `prefersBorder` is a display preference; `domain` is parsed but not published in the
  catalogue today (see §8).

## 3. How the host renders it

### 3.1 Catalogue and document

After connecting, the host fetches and validates every declared document and publishes a
catalogue:

```bash
# a Bot-bound key; a browser uses its account session cookie instead
curl -sS -H "Authorization: Bearer $KEEL_AGENT_TOKEN" https://<host>/v1/mcp/apps
```

Each app row carries `appId`, `server`, `title`, `resourceUri`, `tools`, `visibility`, `csp`,
`permissions`, `prefersBorder`, `hash`, `documentUrl` and diagnostics. `appId` is derived by
the host from `(server, resourceUri)`, never a server-controlled string. The document URL
carries the content hash, so changed content gets a new URL.

Clients **must not** build a document URL from a tool result: the catalogue is the single
machine-readable source of the binding.

### 3.2 Inline and fullscreen

- Inline: inside the tool result card, flexible height (80–480 px); the app can send
  `ui/notifications/size-changed`.
- Fullscreen: the kit Dialog. Entering/leaving is negotiated with
  `ui/request-display-mode`; the host only switches to a mode the View declared.
- `pip` is not a host mode: requesting it returns the current actual mode.

### 3.3 Handshake and data flow

```
View                                   Host
  │  ui/initialize ───────────────────▶ │
  │ ◀───────────────── result ───────── │  protocolVersion / hostInfo / hostContext
  │  ui/notifications/initialized ────▶ │
  │ ◀──────────── tool-input ────────── │  exactly once
  │ ◀──────────── tool-result ───────── │  exactly once, when the result is available
```

- Before `initialized`, the host sends **no** `ui/notifications/*`.
- The handshake deadline is 5 seconds; on timeout the iframe is destroyed and the text
  fallback stays.
- `hostContext` carries theme, `styles.variables`, displayMode, availableDisplayModes,
  `containerDimensions`, locale, timeZone and platform; absent sections are omitted rather
  than sent as empty values.
- A theme change sends a partial `host-context-changed`; it does not reload the document.
- Before unmount the host sends `ui/resource-teardown` and waits at most 1 second (the web
  unmount path currently destroys immediately; see §8).

### 3.4 Theme

The host maps `web/theme.json` tokens onto the apps standard CSS variables
(`--color-background-primary` and friends) and sends them in
`hostContext.styles.variables`. A View that declares no variables keeps its own defaults; the
host does not inject `@font-face`.

## 4. Actions, confirmation and audit

Every outbound action from a View goes through the host HTTP surface. `appId` is the only
identity the client supplies; the server is derived from the catalogue, so a body cannot name
another server.

| Action | Host behaviour | User confirmation |
| --- | --- | --- |
| `tools/call` | Only tools of this app's server with `visibility` containing `"app"`; returns a standard `CallToolResult` | Destructive tools (`destructiveHint` or a change-family name) ask every time |
| `resources/read` | Only this server; URI must be `ui://` or `http(s)://` | No |
| `ui/message` | `role` is `user`, `delivery` is `queue`; send writes the ledger, edit only prefills the composer, discard writes nothing | Default send/edit/discard |
| `ui/open-link` | Only http(s); credentials and non-ASCII hosts are refused; returns the URL for the front end to open with `noopener,noreferrer` | Default open/cancel |
| `ui/update-model-context` | Overwrites this app's session context; the next attempt merges it as `<mcp_app_context>` into the last user message; no ledger write | No |

- Confirmation waits 120 seconds by default (10 minutes max). One session may hold at most 16
  unanswered app confirmations; beyond that the bridge answers `-32000`.
- If the user denies a destructive call, no upstream request is made.
- The `mcp_app_audit` table records the action, result and a bounded summary. It contains
  **no message body, credential, token or full URL query**.

## 5. Degradation and failures

| Situation | What the user sees |
| --- | --- |
| Server declares no `_meta.ui` | Plain text, exactly as without apps |
| Client lacks `mcp.apps`, or the host is the TUI | App title + document path + text result; no WebView |
| Document fetch/type/over 2 MiB/cache failure | Text result + reason + retry; the app is not in the catalogue |
| Re-fetch fails with an older version present | The old version keeps serving; no blank area |
| Handshake exceeds 5 seconds | Iframe destroyed; text + reason + retry |
| Unknown method, frame over 64 KiB, over 200 frames/s | Protocol violation: instance destroyed, text fallback, audit row |
| CSP blocks an external domain | The app remains, but the request is blocked; the host does not widen the policy |
| App requests a tool it may not call | `403` + `refused` audit; no upstream request |
| User denies a destructive call/message/link | Nothing executes; the refusal is audited |

No cell shows a blank area, a fake UI or a fabricated success.

## 6. Capability bits and configuration

### 6.1 Capability bits

`GET /v1/capabilities`, under `features`:

| Bit | Meaning |
| --- | --- |
| `mcp.apps` | The host has an app catalogue/document surface; false means no iframe |
| `mcp.apps.tools` / `mcp.apps.resources` | The bridge may proxy tools/resources |
| `mcp.apps.message` | The bridge may send a confirmed message |
| `mcp.apps.links` | The bridge may open a confirmed link |
| `mcp.apps.context` | `ui/update-model-context` is actually persisted |
| `mcp.apps.sandboxOrigin` | A separate document base origin is configured for desktop/mobile WebViews; **not** a double-iframe sandbox |
| `mcp.events` | The notification/subscription surface is wired (`subscriptions/listen` and friends) |

### 6.2 Environment

| Variable | Meaning |
| --- | --- |
| `KEEL_MCP_CONFIG` / `--mcp` | Global `mcp.json` |
| `KEEL_MCP_PROJECT` / `--mcp-project` | Project `mcp.json` (same-name entries override the global one) |
| `KEEL_MCP_PROTOCOL_ERA` | `auto` (default) / `legacy` / `modern`; an invalid value falls back to legacy |
| `KEEL_MCP_SAMPLING` | `off` / `session` (default) / `on`; MRTR sampling switch |
| `KEEL_MCP_APP_ORIGIN` | Optional absolute http(s) origin; the base for native WebViews, not used by web |

`mcp.json` uses the shared `mcpServers` shape:

```json
{
  "mcpServers": {
    "dashboard": {
      "command": "npx",
      "args": ["-y", "@example/mcp-dashboard"],
      "enabled": true
    }
  }
}
```

Restart `keel serve` after editing `mcp.json`. MCP connection configuration is process-level;
run one process per tenant for multi-tenant deployments.

### 6.3 Key endpoints

| Endpoint | Purpose |
| --- | --- |
| `GET /v1/mcp/servers` | Connection state, `era`, `protocolVersion`, `catalogueError`, tool/app counts |
| `GET /v1/mcp/apps` | App catalogue, theme, limits |
| `GET /v1/mcp/apps/{appId}/view?v=<hash>` | App document (CSP/nosniff/no-referrer/immutable) |
| `POST /v1/mcp/apps/{appId}/tools/call` | Proxy a tool call |
| `POST /v1/mcp/apps/{appId}/resources/read` | Proxy a resource read |
| `POST /v1/mcp/apps/{appId}/ui/message` | Confirmed message |
| `POST /v1/mcp/apps/{appId}/ui/open-link` | Confirmed link |
| `POST /v1/mcp/apps/{appId}/context` | Record app context |
| `GET /v1/mcp/apps/audit` | Read the audit trail |

Every `/v1` route is behind the auth middleware: the account cookie or Bot key determines the
tenant, and `keel serve` injects `X-Tenant-ID` itself. Clients do not supply that header; a
client-supplied one is stripped.

### 6.4 Limits

| Item | Value |
| --- | --- |
| One document | 2 MiB |
| Document cache | 64 MiB (byte LRU) |
| `_meta` | 64 KiB |
| Diagnostics per app | 32 |
| `structuredContent` in details | 256 KiB (truncation marker beyond that) |
| Bridge frame/request body | 64 KiB |
| Bridge rate/concurrency | 200 requests/s, 4 concurrent per app |
| Handshake timeout | 5 seconds |
| Teardown wait | at most 1 second |
| Navigation reload budget | 10 per instance |
| MRTR rounds | 5 |
| Sampling | 3 per call, `maxTokens` capped at 4096 |

## 7. Troubleshooting

| Symptom | Check first |
| --- | --- |
| No app appears | `mcp.apps` in `/v1/capabilities`; `connected` and `catalogueError` in `/v1/mcp/servers`; whether `/v1/mcp/apps` is empty |
| The app is in the catalogue but no frame mounts | Whether the tool result has `details.app`; whether the client is the TUI; whether the deployment gate is off |
| `/view` returns 404 | appId shape, stale hash, tenant mismatch; unknown/foreign apps always look like 404 |
| `fetch` inside the app is blocked | Whether the server declared the domain in `_meta.ui.csp.connectDomains`; the host does not widen it |
| The frame disappears after 5 seconds | Whether the View sent `ui/initialize` and `initialized`; unknown methods or oversized frames also destroy it |
| `tools/call` returns 403 | Whether the tool belongs to this server and `visibility` contains `"app"` |
| A request stays at 202 | A destructive call/message/link is waiting on a user decision; check the approval queue and the 16-open cap |
| 429 | The app exceeded 200 requests/s or 4 concurrent requests; refusal audits are sampled per second |
| The terminal shows only text | Expected: the TUI does not mount WebViews; it shows title/document path/text |

Metrics: `mcp_servers{state}`, `mcp_surface_generation`, `mcp_app_fetch_total{result}`,
`mcp_app_bridge_frames_total{method,direction}`, `mcp_listen_reconnects_total`,
`mcp_mrtr_rounds`. Log keywords: `mcp app audit`, `mcp catalogue audit`, `mcp server log`,
`mcp listen`.

## 8. Current limitations

1. **Browser permissions are not granted.** `camera`/`microphone`/`geolocation`/
   `clipboardWrite` need NSH's per-permission `allow` control. Today `hostCapabilities` does
   not declare them and the iframe has no `allow`; an app must feature-detect and degrade.
   This is the upstream P3.6 dependency.
2. **The second-origin double iframe is deferred.** `KEEL_MCP_APP_ORIGIN` is only the absolute
   document base for desktop/mobile WebViews; web always uses the same-origin opaque frame.
   The spec's sandbox proxy page plus inner View is not implemented.
3. **The client capability declaration is not implemented.** `initialize` does not advertise
   `io.modelcontextprotocol/ui` under `extensions`/`experimental`, and sends no `mimeTypes`.
   A server cannot rely on capability negotiation; declare `_meta.ui` unconditionally.
4. **`_meta.ui.domain` is not published.** It is parsed and validated, but
   `/v1/mcp/apps` rows have no such field.
5. **Teardown is not blocking.** The web unmount path sends `ui/resource-teardown` and then
   destroys immediately; the 1-second bounded wait is covered only by bridge unit tests.
6. **SDK 2.x-only surface.** `ui/download-file` answers `-32601`;
   `ui/notifications/request-teardown` is treated as an unknown notification and destroys the
   instance; `tool-input-partial` is never sent; `pip` is unsupported.

These limitations affect the richer interactive surface, not the readability of the tool
result or the correctness of the tool call itself.
