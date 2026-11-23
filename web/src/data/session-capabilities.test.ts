// The W12 session reads are gated by the capability gate, not by whether the
// current transport happens to implement the method. A fixture (or an older
// live transport) that carries the method must stay off when this client or
// the deployment does not advertise the feature; both halves must say yes.
import { expect, test } from "nsh/test";
import {
  resetCapabilitiesForTest,
  setClientCapabilities,
  setDeploymentCapabilities,
} from "./capabilities";
import { deliverablesAccess } from "./deliverables";
import { exportAvailable, searchAvailable } from "./session-query";

const clientOn = {
  "session.search": true,
  "session.export": true,
  "session.deliverables": true,
};

test("session capabilities: the client half alone does not switch the reads on", () => {
  resetCapabilitiesForTest();
  setClientCapabilities(clientOn);
  setDeploymentCapabilities({ features: {} });
  expect(searchAvailable()).toBe(false);
  expect(exportAvailable()).toBe(false);
  expect(deliverablesAccess.available()).toBe(false);
  resetCapabilitiesForTest();
});

test("session capabilities: both halves on switches the reads on", () => {
  resetCapabilitiesForTest();
  setClientCapabilities(clientOn);
  setDeploymentCapabilities({ features: clientOn });
  expect(searchAvailable()).toBe(true);
  expect(exportAvailable()).toBe(true);
  expect(deliverablesAccess.available()).toBe(true);
  resetCapabilitiesForTest();
});

test("session capabilities: the deployment half alone does not switch the reads on", () => {
  resetCapabilitiesForTest();
  setDeploymentCapabilities({ features: clientOn });
  expect(searchAvailable()).toBe(false);
  expect(exportAvailable()).toBe(false);
  expect(deliverablesAccess.available()).toBe(false);
  resetCapabilitiesForTest();
});
