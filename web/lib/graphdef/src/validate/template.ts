// A ref-level lint of the `${{ ... }}` templating (ticket 8.2), enough to
// reproduce the backend's template_ref_* verdicts without re-implementing the
// whole text/template rewrite. It scans a step config's string values for the
// three recognised reference roots — `steps.<id>`, `run.params.<key>`, and the
// map-body `item`/`item_index` — and reports each so the caller can classify it
// against the graph (checkGraphSemantics does the same on the backend via
// classifyUpstreamRef). Lenient references (a leading `get 'quoted.path'`) are
// skipped, matching the backend's "resolve to nil is the contract" rule, and
// anything the scanner does not recognise is left un-flagged (the client only
// under-reports; the backend stays the authority).
//
// Also here: the feedback-template scanner (internal/dag/feedback.go
// scanFeedbackTemplate) that validation.feedback.template is held to.

const OPEN = "${{";
const CLOSE = "}}";

/** One extracted config reference. */
export interface ConfigRef {
  /** The raw expression text (for the message). */
  raw: string;
  /** The JSON path of the templated string within the config (dag.ConfigPath), e.g. `prompt` / `messages[0].content`. */
  configPath: string;
  /** A `steps.<id>` reference's step id, or "". */
  stepID: string;
  /** A `run.params.<key>` reference's param key, or "". */
  paramKey: string;
  /** True for an `item` / `item_index` reference (only valid inside a map body). */
  itemRef: boolean;
}

/** The result of scanning a config value for template refs. */
export interface ScanResult {
  refs: ConfigRef[];
  /** A template syntax error message (unterminated `${{`), or null. */
  parseError: string | null;
}

// A lenient head: a `get '...'`/`get "..."` expression resolves to nil on miss.
function isLenient(expr: string): boolean {
  return /^\s*get\s+['"]/.test(expr);
}

function classify(expr: string, configPath: string): ConfigRef | null {
  // The head of a pipe expression carries the reference; pipe stages
  // (toJson/truncate/default) take literal args.
  const head = (expr.split("|")[0] ?? "").trim();
  let m = /^steps\.([a-z][a-z0-9_-]*)\b/.exec(head);
  if (m) return { raw: expr, configPath, stepID: m[1]!, paramKey: "", itemRef: false };
  m = /^run\.params\.([A-Za-z0-9_]+)\b/.exec(head);
  if (m) return { raw: expr, configPath, stepID: "", paramKey: m[1]!, itemRef: false };
  if (/^item(_index)?\b/.test(head)) return { raw: expr, configPath, stepID: "", paramKey: "", itemRef: true };
  return null;
}

function joinPath(path: string, key: string): string {
  return path === "" ? key : `${path}.${key}`;
}

/**
 * Extract every recognised `${{ ... }}` reference from a config value's string
 * leaves, tracking each string's config-relative JSON path (sorted-key order,
 * matching dag.tmplParser.walk) so a finding lands at the same path as the
 * backend's.
 */
export function scanConfigRefs(config: unknown): ScanResult {
  const refs: ConfigRef[] = [];
  let parseError: string | null = null;

  const scanString = (s: string, configPath: string): void => {
    let i = 0;
    for (;;) {
      const open = s.indexOf(OPEN, i);
      if (open < 0) return;
      const close = s.indexOf(CLOSE, open + OPEN.length);
      if (close < 0) {
        parseError ??= `unterminated ${JSON.stringify(OPEN)} in template`;
        return;
      }
      const expr = s.slice(open + OPEN.length, close).trim();
      if (!isLenient(expr)) {
        const ref = classify(expr, configPath);
        if (ref) refs.push(ref);
      }
      i = close + CLOSE.length;
    }
  };

  const walk = (v: unknown, path: string): void => {
    if (typeof v === "string") {
      if (v.includes(OPEN)) scanString(v, path);
    } else if (Array.isArray(v)) {
      v.forEach((x, i) => walk(x, `${path}[${i}]`));
    } else if (v !== null && typeof v === "object") {
      for (const k of Object.keys(v as Record<string, unknown>).sort()) {
        walk((v as Record<string, unknown>)[k], joinPath(path, k));
      }
    }
  };
  walk(config, "");
  return { refs, parseError };
}

// The fixed token vocabulary a feedback template may reference (feedback.go).
const FEEDBACK_TOKENS = new Set([
  "feedback.prior_output",
  "feedback.issues",
  "feedback.attempt",
  "feedback.max_attempts",
]);

/** Validate validation.feedback.template (dag.CheckFeedbackTemplate); error string or null. */
export function checkFeedbackTemplate(tmpl: string): string | null {
  if (tmpl.trim() === "") return "must not be empty when set";
  let s = tmpl;
  for (;;) {
    const open = s.indexOf(OPEN);
    if (open < 0) return null;
    const rest = s.slice(open + OPEN.length);
    const end = rest.indexOf(CLOSE);
    if (end < 0) return `unterminated ${JSON.stringify(OPEN)} in feedback template`;
    const token = rest.slice(0, end).trim();
    if (!FEEDBACK_TOKENS.has(token)) {
      return `unknown feedback token ${JSON.stringify(token)} (available: ${[...FEEDBACK_TOKENS].join(", ")})`;
    }
    s = rest.slice(end + CLOSE.length);
  }
}

/** Whether a string carries a template expression (dag.HasTemplate). */
export function hasTemplate(s: unknown): boolean {
  return typeof s === "string" && s.includes(OPEN);
}
