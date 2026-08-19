// A Go-duration parser mirroring time.ParseDuration, used by the config
// validator for `sleep.duration` and `human_approval.timeout` (ADR-013/017).
// Only the subset Go accepts: a signed sequence of decimal-number + unit
// (ns, us/µs, ms, s, m, h). Returns the duration in seconds, or null when the
// string is not a valid Go duration.

const UNIT_SECONDS: Record<string, number> = {
  ns: 1e-9,
  us: 1e-6,
  "µs": 1e-6,
  "μs": 1e-6,
  ms: 1e-3,
  s: 1,
  m: 60,
  h: 3600,
};

/** Parse a Go duration string into seconds, or null if invalid. */
export function parseGoDuration(s: string): number | null {
  if (s === "") return null;
  let str = s;
  let sign = 1;
  if (str[0] === "+" || str[0] === "-") {
    if (str[0] === "-") sign = -1;
    str = str.slice(1);
  }
  if (str === "0") return 0;
  if (str === "") return null;

  let total = 0;
  const re = /(\d*\.?\d+)(ns|us|µs|μs|ms|s|m|h)/y;
  let consumed = 0;
  let m: RegExpExecArray | null;
  while ((m = re.exec(str)) !== null) {
    const value = Number(m[1]);
    if (!Number.isFinite(value)) return null;
    const unit = UNIT_SECONDS[m[2]!];
    if (unit === undefined) return null;
    total += value * unit;
    consumed = re.lastIndex;
  }
  if (consumed !== str.length) return null; // trailing garbage or no units
  return sign * total;
}

/** 30 days, the maximum approval timeout (dag.MaxApprovalTimeout). */
export const MAX_APPROVAL_TIMEOUT_SECONDS = 720 * 3600;
