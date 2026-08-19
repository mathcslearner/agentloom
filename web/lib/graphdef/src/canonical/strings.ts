// Go-compatible JSON scalar encoding (ADR-019 §"Canonical export", ticket 17.6).
//
// The canonical export must be byte-for-byte identical to the backend's
// `dag.Encode` (internal/dag/encode.go), which is `encoding/json` with
// `SetEscapeHTML(false)`. JSON.stringify differs in three ways that matter here:
//   1. it escapes `<`, `>`, `&` — Go with SetEscapeHTML(false) does not;
//   2. it uses the `\b`/`\f` shortcuts — Go writes them as \u0008/\u000c;
//   3. it does NOT escape U+2028/U+2029 — Go's json always does.
// So we hand-roll the exact Go rules rather than lean on JSON.stringify.

const HEX = "0123456789abcdef";

/** The canonical, double-quoted JSON encoding of a string, matching Go's
 *  `encoding/json` with HTML escaping disabled (SetEscapeHTML(false)). */
export function goString(s: string): string {
  let out = '"';
  for (let i = 0; i < s.length; i += 1) {
    const c = s.charCodeAt(i);
    switch (c) {
      case 0x22: // "
        out += '\\"';
        break;
      case 0x5c: // \
        out += "\\\\";
        break;
      case 0x0a: // \n
        out += "\\n";
        break;
      case 0x0d: // \r
        out += "\\r";
        break;
      case 0x09: // \t
        out += "\\t";
        break;
      default:
        if (c < 0x20) {
          // Every other control character (including 0x08 and 0x0c) is a
          // \u00XX escape — Go does not use the \b/\f shortcuts.
          out += "\\u00" + HEX[(c >> 4) & 0xf] + HEX[c & 0xf];
        } else if (c === 0x2028 || c === 0x2029) {
          // Go's json escapes the JS line/paragraph separators unconditionally.
          out += "\\u" + HEX[(c >> 12) & 0xf] + HEX[(c >> 8) & 0xf] + HEX[(c >> 4) & 0xf] + HEX[c & 0xf];
        } else if (c >= 0xd800 && c <= 0xdfff) {
          // A well-formed surrogate pair passes through as its UTF-8 rune; a
          // lone surrogate is invalid UTF-8, which Go replaces with U+FFFD.
          const isHigh = c <= 0xdbff;
          const next = i + 1 < s.length ? s.charCodeAt(i + 1) : 0;
          if (isHigh && next >= 0xdc00 && next <= 0xdfff) {
            out += s.charAt(i) + s.charAt(i + 1);
            i += 1;
          } else {
            out += "�";
          }
        } else {
          out += s.charAt(i);
        }
    }
  }
  return out + '"';
}

/** The canonical JSON encoding of a finite number, matching Go's json number
 *  formatting for the value ranges this schema carries (small decimals and
 *  integers). `-0` renders as `0`, as Go's encoder emits. */
export function goNumber(n: number): string {
  if (!Number.isFinite(n)) return "0"; // NaN/±Inf are rejected upstream; never emit invalid JSON
  if (Object.is(n, -0)) return "0";
  return String(n);
}
