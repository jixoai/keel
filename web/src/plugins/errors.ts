// Typed plugin errors (change keel-harness-v2, W12).
//
// A host `call` rejection keeps only its message across the NSH bridge, so the
// bridge's expected failures travel as documents (`{ok:false, code, message}`)
// and the client rethrows this class. Every place that renders a failure shows
// the code, not only a sentence.
export type PluginErrorCode =
  | "ERR_SLOT_UNKNOWN"
  | "ERR_SLOT_CONFLICT"
  | "ERR_SLOT_INVALID"
  | "ERR_CONTRIBUTION_INVALID"
  | "ERR_CONTRIBUTION_CONFLICT"
  | "ERR_SLOT_FULL"
  | "ERR_FORM_NOT_ALLOWED"
  | "ERR_TARGET_UNAVAILABLE"
  | "ERR_CHUNK_LOAD"
  | "ERR_ACCESS_DENIED"
  | "ETIMEDOUT"
  | "ERR_UNSUPPORTED"
  | "ERR_UNAVAILABLE"
  | "ERR_NOT_FOUND"
  | "ERR_PLUGIN";

export class PluginFault extends Error {
  readonly code: PluginErrorCode;
  readonly field?: string;

  constructor(code: PluginErrorCode, message: string, field?: string) {
    super(message);
    this.name = "PluginFault";
    this.code = code;
    this.field = field;
  }
}

/** Narrow anything thrown into a typed fault, never inventing a code. */
export function asPluginFault(error: unknown, fallback: PluginErrorCode = "ERR_PLUGIN"): PluginFault {
  if (error instanceof PluginFault) return error;
  const message = error instanceof Error ? error.message : typeof error === "string" ? error : "插件调用失败";
  return new PluginFault(fallback, message);
}

/** The HTTP status → stable code mapping of the bridge endpoints. The server's
 * error body carries the sentence; the status carries the class. */
export function codeForStatus(status: number): PluginErrorCode {
  switch (status) {
    case 401:
    case 403:
      return "ERR_ACCESS_DENIED";
    case 404:
      return "ERR_NOT_FOUND";
    case 408:
    case 504:
      return "ETIMEDOUT" as PluginErrorCode;
    case 501:
    case 503:
      return "ERR_UNAVAILABLE";
    default:
      return "ERR_PLUGIN";
  }
}
