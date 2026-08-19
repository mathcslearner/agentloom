/**
 * tail-firehose.ts — headless tailing of the multi-run firehose (Node example).
 *
 * Subscribes to a filtered, cross-run event feed and prints each matched event.
 * Runs until interrupted.
 *
 * Usage:
 *   AGENTLOOM_API_KEY=sk_... \
 *     tsx examples/tail-firehose.ts --api http://localhost:8080 \
 *       [--types run_created,run_succeeded,run_failed] [--definition-name my_flow]
 */
import { FirehoseStream } from "../src/index.js";
import type { EventFilter } from "../src/index.js";

function arg(name: string, fallback?: string): string | undefined {
  const i = process.argv.indexOf(`--${name}`);
  return i >= 0 && i + 1 < process.argv.length ? process.argv[i + 1] : fallback;
}

const baseUrl = arg("api", "http://localhost:8080")!;
const apiKey = process.env.AGENTLOOM_API_KEY;
if (!apiKey) {
  console.error("set AGENTLOOM_API_KEY to a read-scoped key");
  process.exit(2);
}

const filter: EventFilter = {};
const types = arg("types");
if (types) filter.types = types.split(",");
const defName = arg("definition-name");
if (defName) filter.definition_name = defName;

const fh = new FirehoseStream({
  baseUrl,
  auth: { apiKey },
  handlers: {
    onEvent: (env, subs) =>
      console.log(
        JSON.stringify({ run_id: env.run_id, seq: env.seq, type: env.type, subscriptions: subs }),
      ),
    onSubscribed: (id, f) => console.error(`[subscribed] ${id}`, f),
    onError: (err) => console.error(`[error] ${err.message}`),
  },
});

fh.start();
fh.subscribe("cli", filter);
console.error("tailing firehose", filter, "(ctrl-c to stop)");

process.on("SIGINT", () => {
  fh.close();
  process.exit(0);
});
