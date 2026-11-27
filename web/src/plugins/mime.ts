// MIME narrowing for attachment preview contributions (change
// keel-plugin-attachment, task 3.1).
//
// The host's matching rule and the manifest's validation rule live together so
// a contribution cannot be accepted with one grammar and matched with another.
// The grammar mirrors `internal/pluginpkg` (`MimePatternProblem`): `type/subtype`
// or `type/*`, lowercase, one slash, no wildcard type, RFC 6838 token
// characters. A malformed or empty pattern is never a wildcard.

/** One contribution may narrow with at most this many patterns. */
export const MIME_PATTERNS_MAX = 32;
/** One pattern's byte bound (the same bound the server enforces). */
export const MIME_PATTERN_MAX_BYTES = 128;

const MIME_TOKEN = /^[a-z0-9][a-z0-9!#$&^_.+-]*$/;

/** Normalise a MIME for comparison: lowercase, parameters dropped, trimmed.
 * A malformed value still comes back as its lowercased self so an exact
 * pattern can never accidentally match it through whitespace/case. */
export function normalizeMime(value: string | undefined | null): string {
  return String(value ?? "")
    .split(";", 1)[0]!
    .trim()
    .toLowerCase();
}

/** Why a pattern is not `type/subtype` or `type/*`; empty means well-formed. */
export function mimePatternProblem(pattern: string): string {
  if (pattern.trim() === "") return "MIME 模式不能为空。";
  if (pattern.length > MIME_PATTERN_MAX_BYTES) {
    return `MIME 模式最多 ${MIME_PATTERN_MAX_BYTES} 字节。`;
  }
  if (pattern !== pattern.toLowerCase()) return "MIME 模式必须小写。";
  const slash = pattern.indexOf("/");
  if (slash <= 0 || slash === pattern.length - 1 || pattern.split("/").length !== 2) {
    return "MIME 模式必须是 type/subtype 或 type/*。";
  }
  const type = pattern.slice(0, slash);
  const subtype = pattern.slice(slash + 1);
  if (type === "*") return "通配符只允许出现在 subtype（type/*）。";
  if (!MIME_TOKEN.test(type)) return "MIME type 含有不支持的字符。";
  if (subtype === "*") return "";
  if (!MIME_TOKEN.test(subtype)) return "MIME subtype 含有不支持的字符。";
  return "";
}

/** Does one attachment MIME match a contribution's declared narrowing?
 *
 * - `patterns` absent or empty is the wildcard **for an attachment claim**
 *   (`part=attachment`); it is never a wildcard for other part kinds, because
 *   the caller only consults this after checking the part.
 * - Every pattern must be well-formed; a malformed pattern (an empty string, a
 *   `*` type, upper case) matches nothing. The registry rejects such manifests
 *   before this point, but the matcher stays fail-closed.
 */
export function mimeMatches(patterns: readonly string[] | undefined, mime: string): boolean {
  if (!patterns || patterns.length === 0) return true;
  const value = normalizeMime(mime);
  if (!value) return false;
  for (const pattern of patterns) {
    if (mimePatternProblem(pattern) !== "") continue;
    if (pattern === value) return true;
    const wildcard = pattern.indexOf("/*");
    if (wildcard > 0 && pattern.length === wildcard + 2 && value.startsWith(pattern.slice(0, wildcard + 1))) {
      return true;
    }
  }
  return false;
}
