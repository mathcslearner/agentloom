/**
 * tail-run.ts — headless tailing of one run's event feed (Node example, M16.5).
 *
 * Connects to a running agentloom API, tails a run from `--last-seq` through to a
 * terminal event, verifies the feed is gap-free and dup-free by seq across any
 * reconnects, and exits 0 on success / non-zero on a gap or duplicate. This is
 * the exact client M17/M18 build on, exercised end to end.
 *
 * Usage:
 *   AGENTLOOM_API_KEY=sk_... \
 *     tsx examples/tail-run.ts --api http://localhost:8080 --run <run-id> [--last-seq N]
 *
 * A forced reconnect can be observed by restarting the API mid-run
 * (`docker compose restart api`) — the tailer re-mints a ticket, resumes from
 * its cursor, and continues without a gap.
 */
import { RunStream, TERMINAL_RUN_EVENTS } from "../src/index.js";
import type { EventEnvelope } from "../src/index.js";

function arg(name: string, fallback?: string): string | undefined {
  const i = process.argv.indexOf(`--${name}`);
  return i >= 0 && i + 1 < process.argv.length ? process.argv[i + 1] : fallback;
}

const baseUrl = arg("api", "http://localhost:8080")!;
const runId = arg("run");
const lastSeq = Number(arg("last-seq", "0"));
const apiKey = process.env.AGENTLOOM_API_KEY;

if (!runId) {
  console.error("usage: tsx examples/tail-run.ts --run <run-id> [--api URL] [--last-seq N]");
  process.exit(2);
}
if (!apiKey) {
  console.error("set AGENTLOOM_API_KEY to a read-scoped key");
  process.exit(2);
}

const consoleLogger = {
  debug: () => {},
  info: (m: string, meta?: Record<string, unknown>) => console.error(`[info] ${m}`, meta ?? ""),
  warn: (m: string, meta?: Record<string, unknown>) => console.error(`[warn] ${m}`, meta ?? ""),
  error: (m: string, meta?: Record<string, unknown>) => console.error(`[error] ${m}`, meta ?? ""),
};

let expected = lastSeq + 1;
let received = 0;
let reconnects = 0;
let failure: string | null = null;
let firstSeen = true;

const stream = new RunStream<{ id: string }>({
  baseUrl,
  runId,
  auth: { apiKey },
  lastSeq,
  closeOnTerminal: true,
  logger: consoleLogger,
  handlers: {
    onSnapshot: (run) => {
      if (!firstSeen) reconnects++;
      firstSeen = false;
      console.log(JSON.stringify({ frame: "snapshot", run_id: run.id }));
    },
    onEvent: (env: EventEnvelope) => {
      if (env.seq !== expected) {
        failure = `seq gap: expected ${expected}, got ${env.seq}`;
      }
      expected = env.seq + 1;
      received++;
      console.log(JSON.stringify({ frame: "event", seq: env.seq, type: env.type }));
    },
    onCaughtUp: (seq) => console.log(JSON.stringify({ frame: "caught_up", last_seq: seq })),
    onError: (err) => console.error(`[stream-error] ${err.message}`),
    onClosed: () => {
      const ok = failure === null;
      console.log(
        JSON.stringify({
          frame: "summary",
          ok,
          received,
          last_seq: stream.cursor,
          reconnects,
          ...(failure ? { failure } : {}),
        }),
      );
      process.exit(ok ? 0 : 1);
    },
  },
});

console.error(
  `tailing run ${runId} from seq ${lastSeq} (terminal on: ${TERMINAL_RUN_EVENTS.join(", ")})`,
);
stream.start();

// Safety timeout so a stuck run doesn't hang the example forever.
const timeoutMs = Number(arg("timeout-ms", "120000"));
setTimeout(() => {
  console.error("timed out waiting for a terminal event");
  console.log(JSON.stringify({ frame: "summary", ok: false, received, reconnects, failure: "timeout" }));
  process.exit(3);
}, timeoutMs).unref();
