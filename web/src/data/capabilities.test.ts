// The capability gate's contract (change feat-mcp-apps-events, P2).
// A feature is on only when both the client registered it and the deployment
// serves it; an unregistered feature is off, never "trust the server".
import { expect, test } from "nsh/test";
import {
  can,
  deploymentCan,
  deploymentCapabilities,
  mcpAppsCapability,
  parseDeploymentCapabilities,
  resetCapabilitiesForTest,
  setClientCapabilities,
  setDeploymentCapabilities,
} from "./capabilities";

const discovery = {
  features: { "mcp.apps": true, "mcp.apps.sandboxOrigin": true, "sessions.list": true },
  mcpApps: { sandboxOrigin: "https://mcp-app.example.com", generation: 7, apps: 2 },
};

test("can: 客户端未注册的特性一律 false（即使部署位为真）", () => {
  resetCapabilitiesForTest();
  setDeploymentCapabilities(discovery);
  setClientCapabilities({ "mcp.apps": true });
  expect(can("mcp.apps")).toBe(true);
  expect(can("sessions.list")).toBe(false);
  expect(can("mcp.events")).toBe(false);
  expect(deploymentCan("sessions.list")).toBe(true);
});

test("can: 客户端显式为 false 时不回落到部署位", () => {
  resetCapabilitiesForTest();
  setDeploymentCapabilities(discovery);
  setClientCapabilities({ "mcp.apps": false });
  expect(can("mcp.apps")).toBe(false);
  expect(deploymentCan("mcp.apps")).toBe(true);
});

test("can: 未探测/离线时全部关闭", () => {
  resetCapabilitiesForTest();
  setClientCapabilities({ "mcp.apps": true });
  expect(can("mcp.apps")).toBe(false);
  setDeploymentCapabilities(null);
  expect(can("mcp.apps")).toBe(false);
});

test("发现文档：解析 features 与 mcpApps；非布尔/非字符串被丢弃", () => {
  resetCapabilitiesForTest();
  const parsed = parseDeploymentCapabilities({
    features: { "mcp.apps": true, noise: "yes", nope: 1 },
    mcpApps: { sandboxOrigin: "  https://mcp-app.example.com  ", generation: 3.9, apps: -2 },
  });
  expect(parsed.features).toEqual({ "mcp.apps": true });
  expect(parsed.mcpApps).toEqual({ sandboxOrigin: "https://mcp-app.example.com", generation: 3, apps: 0 });
  expect(mcpAppsCapability().sandboxOrigin).toBe("");
  setDeploymentCapabilities(discovery);
  expect(mcpAppsCapability()).toEqual({
    sandboxOrigin: "https://mcp-app.example.com",
    generation: 7,
    apps: 2,
  });
  expect(deploymentCapabilities().features["mcp.apps.sandboxOrigin"]).toBe(true);
});
