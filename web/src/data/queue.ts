// The 202 admission's queue block, read once for every transport that needs it
// (the HTTP input command and, later, any BFF bridge). Keeping the parse here
// means a missing/unknown field degrades to "no queue fact" instead of a made-up
// position or a zero ETA.
import type { QueueFacts } from "./contract";

/** parseQueueFacts reads the `queued` object of a 202 response body. It returns
 * undefined when the body has no queue block or no positive 1-based position;
 * etaSeconds stays null unless the server marked the ETA known and sent a
 * finite number. */
export function parseQueueFacts(body: unknown): QueueFacts | undefined {
  if (!body || typeof body !== "object") return undefined;
  const queued = (body as { queued?: unknown }).queued;
  if (!queued || typeof queued !== "object") return undefined;
  const record = queued as Record<string, unknown>;
  const position = typeof record.position === "number" ? record.position : 0;
  if (!Number.isFinite(position) || position <= 0) return undefined;
  const etaKnown = record.etaKnown === true;
  const etaSeconds = etaKnown && typeof record.etaSeconds === "number" && Number.isFinite(record.etaSeconds)
    ? record.etaSeconds
    : null;
  return {
    position,
    etaKnown,
    etaSeconds,
    ...(typeof record.groupId === "string" && record.groupId ? { groupId: record.groupId } : {}),
  };
}
