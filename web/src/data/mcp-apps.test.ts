// The MCP Apps catalogue/binding readers (change feat-mcp-apps-events, P2).
// Security-bearing fields are the point: a server never chooses where the
// iframe points.
import { expect, test } from "nsh/test";
import {
  appBindingOf,
  appDocumentSource,
  appDocumentUrl,
  callToolResultFor,
  contentTextOf,
  MCP_APP_RPC_INTERNAL_ERROR,
  MCP_APP_RPC_MESSAGE_MAX,
  MCP_APP_RPC_PENDING,
  mcpAppContextBody,
  mcpAppHostCapabilities,
  mcpAppMessageBody,
  mcpAppMessagePrefill,
  mcpAppOpenLinkBody,
  mcpAppOpenLinkTarget,
  mcpAppResourceReadBody,
  mcpAppRpcErrorOf,
  mcpAppRpcErrorOfResponse,
  mcpAppToolCallBody,
  parseMcpAppCatalogue,
  themeVariablesFor,
} from "./mcp-apps";

const APP_ID = "mcpapp_0123456789abcdef";
const HASH = "a".repeat(64);
const DOCUMENT_URL = `/v1/mcp/apps/${APP_ID}/view?v=${HASH}`;

const row = (overrides: Record<string, unknown> = {}) => ({
  appId: APP_ID,
  server: "weather",
  title: "天气面板",
  resourceUri: "ui://weather/dashboard",
  mimeType: "text/html;profile=mcp-app",
  visibility: ["model", "app"],
  tools: [{ tool: "dashboard", visibility: ["model", "app"] }],
  csp: "default-src 'none'",
  permissions: { camera: false },
  hash: HASH,
  documentUrl: DOCUMENT_URL,
  ...overrides,
});

test("目录：合法 app 被解析，主题只保留字符串变量", () => {
  const catalogue = parseMcpAppCatalogue({
    generation: 4,
    apps: [row()],
    theme: { dark: { "--color-text-primary": "#fff", bad: 3 }, light: {} },
    limits: { documentBytes: 2097152 },
    sandboxOrigin: "https://mcp-app.example.com",
  });
  expect(catalogue.generation).toBe(4);
  expect(catalogue.apps.length).toBe(1);
  expect(catalogue.apps[0].title).toBe("天气面板");
  expect(catalogue.apps[0].permissions).toEqual({ camera: false });
  expect(catalogue.theme).toEqual({ dark: { "--color-text-primary": "#fff" } });
  expect(catalogue.sandboxOrigin).toBe("https://mcp-app.example.com");
});

test("目录：错误 mime/跨源 URL/错 appId/错 hash 的行一律丢弃", () => {
  const catalogue = parseMcpAppCatalogue({
    apps: [
      row({ mimeType: "application/json" }),
      row({ documentUrl: `https://evil.example/v1/mcp/apps/${APP_ID}/view?v=${HASH}` }),
      row({ documentUrl: `${DOCUMENT_URL}&x=1` }),
      row({ appId: "weather" }),
      row({ hash: "short" }),
      row({ resourceUri: "https://evil.example/app" }),
    ],
  });
  expect(catalogue.apps.length).toBe(0);
});

test("绑定：details.app 的派生身份被读取；无 app/错形状返回 null", () => {
  const binding = appBindingOf({
    details: { server: "weather", tool: "dashboard", app: { appId: APP_ID, resourceUri: "ui://weather/dashboard", visibility: ["model", "app"] } },
  });
  expect(binding).toEqual({ appId: APP_ID, resourceUri: "ui://weather/dashboard", visibility: ["model", "app"] });
  expect(appBindingOf({ details: { server: "weather" } })).toBe(null);
  expect(appBindingOf({ app: { appId: "not-derived", resourceUri: "ui://x/y" } })).toBe(null);
  expect(appBindingOf({ app: { appId: APP_ID, resourceUri: "file:///etc/passwd" } })).toBe(null);
});

test("文档地址：只接受目录自己生成的同源 hash URL", () => {
  const app = parseMcpAppCatalogue({ apps: [row()] }).apps[0];
  expect(appDocumentUrl(app)).toBe(DOCUMENT_URL);
  expect(appDocumentUrl({ ...app, documentUrl: "https://mcp-app.example.com" + DOCUMENT_URL })).toBe(null);
  expect(appDocumentUrl({ ...app, documentUrl: "/v1/mcp/apps/other/view?v=" + HASH })).toBe(null);
});

test("主题：只取当前 appearance 的变量；空/缺失不发明值", () => {
  expect(themeVariablesFor({ dark: { a: "1" }, light: { a: "2" } }, "dark")).toEqual({ a: "1" });
  expect(themeVariablesFor({ dark: { a: " " } }, "dark")).toBe(undefined);
  expect(themeVariablesFor(undefined, "light")).toBe(undefined);
});

test("CallToolResult：content 始终是数组，structuredContent 原样带出", () => {
  expect(callToolResultFor(undefined)).toBe(null);
  expect(callToolResultFor({ content: { type: "text", text: "x" }, isError: true })).toEqual({
    content: [{ type: "text", text: "x" }],
    isError: true,
  });
  expect(callToolResultFor({ content: [], structuredContent: { a: 1 } })).toEqual({
    content: [],
    structuredContent: { a: 1 },
    isError: false,
  });
});

test("加载地址：web 必须用页面来源的绝对 URL（NSH 拒绝相对 src）；桌面用沙箱域", () => {
  const app = parseMcpAppCatalogue({ apps: [row()] }).apps[0];
  expect(appDocumentSource(app, { web: true })).toEqual({
    error: "无法读取页面来源，交互界面不能加载",
  });
  expect(appDocumentSource(app, { web: true, origin: "https://keel.example.com" })).toEqual({
    url: `https://keel.example.com${DOCUMENT_URL}`,
  });
  expect(appDocumentSource(app, { web: true, origin: "https://keel.example.com/settings" })).toEqual({
    url: `https://keel.example.com${DOCUMENT_URL}`,
  });
  expect(
    appDocumentSource(app, {
      web: true,
      origin: "https://keel.example.com",
      instance: "abcdef0123456789",
      reload: 3,
    }),
  ).toEqual({
    url: `https://keel.example.com${DOCUMENT_URL}&instance=abcdef0123456789&reload=3`,
  });
  expect(appDocumentSource(app, { web: true, origin: "https://keel.example.com", instance: "short" })).toEqual({
    error: "实例标识形状非法",
  });
  expect(appDocumentSource(app, { web: true, origin: "javascript:alert(1)" })).toEqual({
    error: "页面来源只接受 http(s)",
  });
  expect(appDocumentSource(app, { web: true, origin: "https://user:pass@keel.example.com" })).toEqual({
    error: "页面来源不得携带凭据",
  });
  expect(appDocumentSource(app, { web: false })).toEqual({
    error: "此部署未配置沙箱域，桌面客户端不挂载交互界面",
  });
  expect(appDocumentSource(app, { web: false, sandboxOrigin: "https://mcp-app.example.com" })).toEqual({
    url: `https://mcp-app.example.com${DOCUMENT_URL}`,
  });
  expect(appDocumentSource(app, { web: false, sandboxOrigin: "javascript:alert(1)" })).toEqual({
    error: "沙箱域只接受 http(s)",
  });
  expect(appDocumentSource(app, { web: false, sandboxOrigin: "https://user:pass@mcp-app.example.com" })).toEqual({
    error: "沙箱域不得携带凭据",
  });
});

test("文本：CallToolResult.content 的 text 块按段落拼接", () => {
  expect(
    contentTextOf([
      { type: "text", text: "第一段" },
      { type: "image", data: "x" },
      { type: "text", text: "第二段" },
    ]),
  ).toBe("第一段\n\n第二段");
  expect(contentTextOf(undefined)).toBe("");
});

// ---- host bridge helpers (F2) --------------------------------------------

test("代理 body：sessionId 有值才发；message 固定 role=user/delivery=queue", () => {
  expect(mcpAppToolCallBody("s-1", "refresh", { q: 1 })).toEqual({
    sessionId: "s-1",
    name: "refresh",
    arguments: { q: 1 },
  });
  expect(mcpAppToolCallBody(null, "refresh", undefined)).toEqual({ name: "refresh", arguments: {} });
  expect(mcpAppResourceReadBody("s-1", "ui://weather/dashboard")).toEqual({
    sessionId: "s-1",
    uri: "ui://weather/dashboard",
  });
  expect(mcpAppMessageBody("s-1", [{ type: "text", text: "hi" }], "r-1")).toEqual({
    sessionId: "s-1",
    role: "user",
    content: [{ type: "text", text: "hi" }],
    delivery: "queue",
    requestId: "r-1",
  });
  expect(mcpAppOpenLinkBody(undefined, "https://example.com/a")).toEqual({
    url: "https://example.com/a",
  });
  expect(mcpAppContextBody("s-1", { a: 1 }, undefined, "r-2")).toEqual({
    sessionId: "s-1",
    requestId: "r-2",
    content: { a: 1 },
  });
});

test("错误映射：优先透传 host 的 JSON-RPC code；无 code 用 -32603 且有界", () => {
  expect(mcpAppRpcErrorOf({ error: { code: -32602, message: "bad params" } })).toEqual({
    code: -32602,
    message: "bad params",
  });
  expect(mcpAppRpcErrorOf(Object.assign(new Error("no tool"), { code: -32000 }))).toEqual({
    code: -32000,
    message: "no tool",
  });
  expect(mcpAppRpcErrorOf(new Error("boom"))).toEqual({
    code: MCP_APP_RPC_INTERNAL_ERROR,
    message: "boom",
  });
  expect(mcpAppRpcErrorOf("plain").code).toBe(MCP_APP_RPC_INTERNAL_ERROR);
  const long = mcpAppRpcErrorOf(new Error("x".repeat(MCP_APP_RPC_MESSAGE_MAX * 4)));
  expect(long.message.length).toBeLessThanOrEqual(MCP_APP_RPC_MESSAGE_MAX);
  expect(mcpAppRpcErrorOf(new Error("line\nbreak")).message).toBe("line break");
});

test("HTTP 错误映射：JSON-RPC 形状同 code；202 不是成功；普通 error 用 -32603", () => {
  expect(
    mcpAppRpcErrorOfResponse(400, { jsonrpc: "2.0", error: { code: -32602, message: "role must be user" } }),
  ).toEqual({ code: -32602, message: "role must be user" });
  expect(mcpAppRpcErrorOfResponse(403, { error: "tool is not callable" })).toEqual({
    code: MCP_APP_RPC_INTERNAL_ERROR,
    message: "tool is not callable",
  });
  expect(mcpAppRpcErrorOfResponse(202, { decisionId: "d-1", pending: true })).toEqual({
    code: MCP_APP_RPC_PENDING,
    message: "交互操作仍在等待用户确认",
  });
  expect(mcpAppRpcErrorOfResponse(502, null)).toEqual({
    code: MCP_APP_RPC_INTERNAL_ERROR,
    message: "HTTP 502",
  });
});

test("ui/message：只有 edit 分支给出预填文本；send/discard 不预填", () => {
  expect(mcpAppMessagePrefill({ result: "edit", prefilled: { text: "改好的话" } })).toBe("改好的话");
  expect(mcpAppMessagePrefill({ result: "send" })).toBe(undefined);
  expect(mcpAppMessagePrefill({ result: "discard" })).toBe(undefined);
  expect(mcpAppMessagePrefill({ result: "edit", prefilled: { text: "   " } })).toBe(undefined);
});

test("ui/open-link：只有 opened=true 且带目标 URL 才算打开", () => {
  expect(mcpAppOpenLinkTarget({ opened: true, url: "https://example.com/a" })).toEqual({
    opened: true,
    url: "https://example.com/a",
  });
  expect(mcpAppOpenLinkTarget({ opened: false, reason: "denied" })).toEqual({ opened: false });
  expect(mcpAppOpenLinkTarget({ opened: true })).toEqual({ opened: false });
});

test("hostCapabilities：全开声明五项 + logging + sandbox，全关只留 logging", () => {
  const app = parseMcpAppCatalogue({
    apps: [row({ cspDomains: { connectDomains: ["https://api.example.com"] } })],
  }).apps[0];
  const all = mcpAppHostCapabilities(
    {
      serverTools: true,
      serverResources: true,
      message: true,
      openLinks: true,
      updateModelContext: true,
    },
    app,
  );
  expect(all).toEqual({
    logging: {},
    serverTools: {},
    serverResources: {},
    message: {},
    openLinks: {},
    updateModelContext: {},
    sandbox: { csp: { connectDomains: ["https://api.example.com"] } },
  });
  const none = mcpAppHostCapabilities(
    {
      serverTools: false,
      serverResources: false,
      message: false,
      openLinks: false,
      updateModelContext: false,
    },
    { cspDomains: undefined },
  );
  expect(none).toEqual({ logging: {} });
});
