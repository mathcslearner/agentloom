// The client-side validation issue model (ADR-019 §"Validation parity"). Wire-
// compatible with the backend's `Issue` (internal/api/types.go): a stable
// machine `code` (empty for shape/decode-level findings, matching the backend's
// codeless *dag.DecodeError), a `severity`, a JSON `path` in the backend's
// grammar (`steps[3].config.model`), and a human `msg`. So a client verdict and
// a server verdict name the same problem in the same place, and a server 400's
// issues[] map onto the same nodes by path.

/** Issue severity — `error` blocks submit, `warning` does not. */
export type Severity = "error" | "warning";

/** A single validation finding. */
export interface Issue {
  /** Stable machine code; "" for shape/decode-level findings (no ValidationCode). */
  code: string;
  severity: Severity;
  /** JSON path in the backend grammar, e.g. `steps[3].config.messages[0].content`. */
  path: string;
  msg: string;
}

/** Construct an error-severity issue. */
export function err(code: string, path: string, msg: string): Issue {
  return { code, severity: "error", path, msg };
}

/** Construct a warning-severity issue. */
export function warn(code: string, path: string, msg: string): Issue {
  return { code, severity: "warning", path, msg };
}

/** True if any issue is error-severity (the accept/reject signal). */
export function hasErrors(issues: readonly Issue[]): boolean {
  return issues.some((i) => i.severity === "error");
}

/** The validation code vocabulary the client reproduces (a subset of the backend's). */
export const Code = {
  ConfigFieldRequired: "config_field_required",
  ConfigFieldConflict: "config_field_conflict",
  ConfigFieldInvalid: "config_field_invalid",
} as const;
