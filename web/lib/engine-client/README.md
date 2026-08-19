# @agentloom/engine-client

The typed client for the agentloom event feed (ADR-018, M16.5). It is the exact
client the M17 builder and M18 dashboard build on, and it runs headless in Node.

## What it does

- **Generated event types.** `src/generated/events.ts` is generated from the
  backend's committed `docs/schema/events.v1.json` — the same schema the Go
  event structs emit (CI-diffed). `EventEnvelope` is a discriminated union:
  `switch (env.type)` narrows `env.payload` to the matching payload struct. CI
  fails if the committed TS is stale against the schema.
- **Ticket auth.** A browser cannot set an `Authorization` header on a WebSocket,
  so the client mints a short-lived signed ticket over REST and passes it as a
  `?ticket=` query parameter, re-minting on every (re)connect.
- **Deterministic recovery.** `snapshot → backfill from last_seq → live tail`,
  with seq dedupe, resume, and exponential-backoff reconnection — every recovery
  reduces to "read rows after `last_seq`", so dropped connections heal with zero
  gaps or dupes.

## Usage — one run

```ts
import { RunStream } from "@agentloom/engine-client";

const stream = new RunStream({
  baseUrl: "http://localhost:8080",
  runId,
  auth: { apiKey: process.env.AGENTLOOM_API_KEY! }, // or { mintTicket }
  handlers: {
    onSnapshot: (run) => render(run),
    onEvent: (env) => {
      switch (env.type) {
        case "step_succeeded":
          console.log(env.payload.step_id, env.payload.attempt); // narrowed
          break;
        case "cost_updated":
          console.log(env.payload.run_spent_nano_usd);
          break;
      }
    },
    onCaughtUp: (lastSeq) => console.log("live from", lastSeq),
  },
});
stream.start();
// later: stream.close();
```

## Usage — the multi-run firehose

```ts
import { FirehoseStream } from "@agentloom/engine-client";

const fh = new FirehoseStream({ baseUrl, auth: { apiKey } }).start();
fh.subscribe("run-list", { types: ["run_created", "run_succeeded", "run_failed"] });
fh.subscribe("one-run", { run_ids: [runId] }, { [runId]: 7 }); // resume from seq 7
```

The client re-issues every subscription with its tracked cursors on reconnect,
so runs resume without gaps.

## Auth modes

- `{ apiKey }` — the client mints tickets itself with a `read` bearer. Server /
  Node side; the key stays out of the browser.
- `{ mintTicket: () => Promise<string> }` — the caller supplies tickets, e.g. a
  browser calling its own server-side proxy so the API key never reaches the
  browser. This is the M17 dashboard path.

A non-browser client can alternatively send a `read` bearer on the WS upgrade
itself; provide a `webSocketFactory` that sets the `Authorization` header
(standard `WebSocket` cannot set headers, so this is the escape hatch).

## Node example (headless tailing)

```bash
AGENTLOOM_API_KEY=sk_... \
  pnpm exec tsx examples/tail-run.ts --api http://localhost:8080 --run <run-id>
```

Restart the API mid-run (`docker compose restart api`) to watch the tailer
re-mint a ticket and resume from its cursor with no gap. `make smoke-ws-tail`
(repo root) automates exactly that check.

## Development

```bash
pnpm generate    # docs/schema/events.v1.json -> src/generated/events.ts
pnpm typecheck
pnpm test
pnpm build
```

The module has **no UI/React imports** and no runtime dependencies (it uses the
global `WebSocket` and `fetch`; Node >= 22). Every transport seam — the socket,
the timers, `fetch` — is injectable, so the streams are unit-tested against an
in-memory fake transport and a fake clock.
