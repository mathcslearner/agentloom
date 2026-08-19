/**
 * GENERATED FILE — do not edit by hand.
 *
 * Emitted from api/openapi.yaml by scripts/gen-openapi-types.ts (`pnpm generate`).
 * CI regenerates this and fails on any diff, so it cannot drift from the spec.
 */
export interface paths {
    "/healthz": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * Liveness probe
         * @description Reports liveness: `200` while Postgres answers a ping, `503` otherwise. Anonymous by design — probes need no secret.
         */
        get: operations["getHealth"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/runs": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * List runs
         * @description Scope `read`, rate-limit class `read`. One keyset page, newest
         *     first — order `(created_at DESC, id DESC)` — with optional
         *     filters. Feed `next_cursor` back verbatim as `?cursor=` for the
         *     next page; its absence means this was the last page. Keyset
         *     pagination is stable under concurrent inserts: new runs sort
         *     before any already-issued cursor position, so a walk never skips
         *     or repeats a row.
         */
        get: operations["listRuns"];
        put?: never;
        /**
         * Submit a run
         * @description Scope `submit`, rate-limit class `submit`. Instantiates a run from
         *     an inline definition document or a stored definition reference
         *     (exactly one of `definition` / `definition_id`). The run, its
         *     per-run graph copy, entry-ready steps, and their dispatch outbox
         *     rows are written in one transaction; workers pick the run up
         *     asynchronously.
         *
         *     Idempotent submission rides the `Idempotency-Key` header:
         *     resubmitting the same key with the same payload (definition
         *     snapshot + canonicalized params + definition ref) returns the
         *     original run as `200` with `reused: true`; the same key with a
         *     different payload is refused with `409 idempotency_key_conflict`.
         *     Keys are global, not per-API-key.
         *
         *     An inline definition that fails decoding or validation is a `400`
         *     with code `invalid_definition` and the path-qualified `issues`
         *     list. A `definition_id` that resolves to nothing is a `400` with
         *     code `definition_not_found`.
         */
        post: operations["submitRun"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/runs/{run_id}": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * Get one run
         * @description Scope `read`, rate-limit class `read`. The run row, every step
         *     with its full attempt history, every edge with its resolution,
         *     and the run's dead-letter records (how a client discovers
         *     requeueable steps — empty for healthy runs). Reads are not one
         *     snapshot: a run mid-flight may show a step newer than its rollup
         *     counters; the next poll heals it.
         */
        get: operations["getRun"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/runs/{run_id}/cost": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * Get a run's cost breakdown
         * @description Scope `read`, rate-limit class `read`. The run's cumulative cost
         *     (ADR-012) — cumulative spend and cache savings — plus per-step and
         *     per-resource (per-model / per-tool) breakdowns and the full
         *     per-attempt ledger. Money is integer nano-USD (1 USD = 1e9); the
         *     `*_usd` strings are the derived human-readable rendering. A run with
         *     no cost-bearing attempts returns a zero summary and empty
         *     breakdowns.
         */
        get: operations["getRunCost"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/runs/{run_id}/cancel": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        put?: never;
        /**
         * Cancel a run
         * @description Scope `submit`, rate-limit class `submit`. Requests cooperative
         *     cancellation (reason `manual`). The response reports what settled
         *     in the request transaction — claimless pending/ready/retrying
         *     steps are cancelled immediately; in-flight steps converge
         *     afterwards as their workers notice (the run rests at `cancelling`
         *     until then, `cancelled` when `finalized` is true). Cancelling a
         *     run already in a terminal state is a `409` with code `conflict`.
         */
        post: operations["cancelRun"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/runs/{run_id}/park": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        put?: never;
        /**
         * Park a run
         * @description Scope `submit`, rate-limit class `submit`. Pauses dispatch
         *     (reason `manual`): workers refuse new claims on the run's steps
         *     while in-flight steps keep executing and settle normally. Parking
         *     a run that is not `running` is a `409` with code `conflict`.
         */
        post: operations["parkRun"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/runs/{run_id}/unpark": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        put?: never;
        /**
         * Unpark a run
         * @description Scope `submit`, rate-limit class `submit`. Resumes dispatch,
         *     re-dispatching every ready step whose queue delivery was consumed
         *     while the run was parked (`dispatched`). Unparking a run that is
         *     not `parked` is a `409` with code `conflict`.
         */
        post: operations["unparkRun"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/runs/{run_id}/budget": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        /**
         * Raise a run's spend budget
         * @description Scope `submit`, rate-limit class `submit`. Sets the run's spend
         *     budget (`budget_usd`, positive US dollars). Combined with `unpark`,
         *     this is the documented resume path for a run parked with reason
         *     `budget_exceeded`: raise the budget, then unpark. Raising the budget
         *     does not itself dispatch work (ADR-002). The budget is immutable on a
         *     terminal run — a `409` with code `conflict`.
         */
        patch: operations["setRunBudget"];
        trace?: never;
    };
    "/v1/runs/{run_id}/steps/{step_id}/requeue": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        put?: never;
        /**
         * Requeue a dead-lettered step
         * @description Scope `submit`, rate-limit class `submit`. Resets a dead-lettered
         *     step to `ready` with its retry budget re-armed (counted failures
         *     restart from the dead-letter baseline; attempt history stays
         *     immutable), re-opens the run when it rested at `failed`
         *     (`run_resumed`), and revives written-off descendants (`revived`).
         *     Requeueing a step that is not dead-lettered — or any step of a
         *     cancelled run — is a `409` with code `conflict`. A missing run is
         *     `404 run_not_found`; an existing run without that step is
         *     `404 step_not_found`.
         */
        post: operations["requeueStep"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/runs/{run_id}/steps/{step_id}/logs": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * Get one attempt's captured executor logs
         * @description Scope `read`, rate-limit class `read`. One ascending-seq keyset
         *     page of the log lines the step's executor emitted through its
         *     step logger, captured durably per attempt (retries, reclaims,
         *     and takeovers each get their own attempt and so their own log
         *     stream). Lines carry the attempt's `trace_id` for joining to
         *     traces. `attempt` defaults to the step's latest; an attempt with
         *     no lines — never reached, level-filtered at capture, or simply
         *     quiet — answers an empty page, not a `404`. Storage is a
         *     size-capped ring with a bounded capture buffer, both dropping
         *     oldest: `truncated`/`dropped_lines` report what was lost, and
         *     the retained window is the newest lines. Poll for follow-mode;
         *     there is no streaming channel in v1. A missing run is
         *     `404 run_not_found`; an existing run without that step is
         *     `404 step_not_found`.
         */
        get: operations["getStepLogs"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/runs/{run_id}/blackboard": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * Get the run's blackboard entries
         * @description Scope `read`, rate-limit class `read`. The run-scoped blackboard
         *     (ADR-014): versioned key/value shared memory steps read and write
         *     during the run. By default each key's head (latest version) is
         *     returned, ordered by key; `history=true` returns every version,
         *     ordered by `(key, version)`. Filter by `key` (repeatable) and by
         *     `tag` (repeatable — an entry must carry every listed tag). Each
         *     entry carries its `token_count` under a `token_counter` fingerprint
         *     and its author step. Results page by an opaque keyset `cursor`. A
         *     missing run is `404 run_not_found`.
         */
        get: operations["getRunBlackboard"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/runs/{run_id}/graph": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * Get the run's versioned graph with provenance
         * @description Scope `read`, rate-limit class `read`. The run's current versioned
         *     graph (ADR-015): every node and edge with its provenance — whether it
         *     was authored in the definition or injected by a planner / map / loop
         *     expansion, the step that injected it, the expansion nesting depth, the
         *     graph version at which it was introduced, and the time it was added.
         *     `graph_version` is the current (highest) version; the authored graph is
         *     version 1 and each expansion bumps it (so `graph_version` equals the
         *     run's expansion count + 1). `expansions` is the ordered per-version
         *     delta feed reconstructed from the run's `graph_expanded` events — the
         *     contract the dashboard uses to animate expansion. A client reconstructs
         *     any version N by keeping the nodes and edges whose `graph_version` is at
         *     most N. A run that never expanded returns its authored graph with an
         *     empty `expansions` list. A missing run is `404 run_not_found`.
         */
        get: operations["getRunGraph"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/runs/{run_id}/ws-ticket": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        put?: never;
        /**
         * Mint a short-lived ticket for the run event WebSocket
         * @description Scope `read`, rate-limit class `read`. Mints a short-lived, signed,
         *     opaque ticket scoped to this one run and the `read` scope (ticket 16.3,
         *     ADR-018). A browser cannot set an `Authorization` header on a WebSocket,
         *     so it passes this ticket to `GET /v1/runs/{run_id}/ws` as a `ticket`
         *     query parameter instead of a long-lived bearer key (which would leak
         *     into logs, proxies, and history). The ticket expires quickly
         *     (`expires_at`); the client re-mints when it expires. A missing run is
         *     `404 run_not_found`; `503 stream_unavailable` when event streaming is not
         *     enabled on this server.
         */
        post: operations["mintRunWSTicket"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/runs/{run_id}/ws": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * Stream a run's live event feed over WebSocket
         * @description Rate-limit class `read`. Upgrades to a WebSocket and streams the run's
         *     normalized event feed (ticket 16.3, ADR-018). Authentication is EITHER a
         *     `ticket` query parameter (minted at `POST .../ws-ticket`, the browser
         *     path) OR a `read`-scoped bearer key (for non-browser clients); a failure
         *     is the uniform `401`. An optional `last_seq` query parameter resumes the
         *     stream after that seq.
         *
         *     Protocol (JSON text frames, discriminated by `type`): the server sends
         *     one `snapshot` frame (the `GET /v1/runs/{run_id}` body), then `event`
         *     frames backfilled from `last_seq`, then a `caught_up` frame, then live
         *     `event` frames as the run progresses. Clients dedupe and order by
         *     `(run_id, seq)` and resume after a disconnect by reconnecting with
         *     `last_seq` set to the highest seq seen — recovery is deterministic
         *     because every mechanism reduces to a DB read after `last_seq`. A client
         *     that falls too far behind is closed with application close code `4001`
         *     (slow consumer) and resumes with its `last_seq`. `503 stream_unavailable`
         *     when event streaming is not enabled on this server.
         */
        get: operations["streamRunEvents"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/events/ws-ticket": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        put?: never;
        /**
         * Mint a short-lived ticket for the multi-run firehose WebSocket
         * @description Scope `read`, rate-limit class `read`. Mints a short-lived, signed,
         *     opaque ticket scoped to the firehose audience and the `read` scope
         *     (ticket 16.4, ADR-018). Unlike the run ticket it is not bound to a run —
         *     the firehose is cross-run and narrowed by the client's `subscribe`
         *     filters. The ticket expires quickly (`expires_at`); the client re-mints
         *     when it expires. `503 stream_unavailable` when event streaming is not
         *     enabled on this server.
         */
        post: operations["mintFirehoseWSTicket"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/events/ws": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * Stream a filtered, multi-run event feed over WebSocket
         * @description Rate-limit class `read`. Upgrades to a WebSocket and streams a filtered,
         *     cross-run event feed for the dashboard's run list (ticket 16.4,
         *     ADR-018). Authentication is EITHER a `ticket` query parameter (minted at
         *     `POST /v1/events/ws-ticket`, the browser path) OR a `read`-scoped bearer
         *     key (for non-browser clients); a failure is the uniform `401`.
         *
         *     Protocol (JSON text frames, discriminated by `type`). The client manages
         *     subscriptions with control messages: `subscribe` (`{type, id, filter,
         *     cursors}`) opens or replaces one filtered subscription; `unsubscribe`
         *     (`{type, id}`) cancels it. The server replies with a `subscribed` ack,
         *     then `event` frames backfilled from any `cursors`, a `caught_up` frame,
         *     then live `event` frames — each tagged with the `subscriptions` ids it
         *     matched. A malformed or over-limit control message yields an in-band
         *     `error` frame and leaves the connection open. Filters (`run_ids`,
         *     `types`, `definition_id`, `definition_name`) are ANDed; clients dedupe
         *     and order by `(run_id, seq)` and resume runs via `cursors`. A client that
         *     falls too far behind is closed with application close code `4001` (slow
         *     consumer) and resumes. `503 stream_unavailable` when event streaming is
         *     not enabled on this server.
         */
        get: operations["streamEvents"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/definitions": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * List definitions (latest per name)
         * @description Scope `read`, rate-limit class `read`. One keyset page of each
         *     name's newest version, in name order. Feed `next_cursor` back
         *     verbatim as `?cursor=` for the next page.
         */
        get: operations["listDefinitions"];
        put?: never;
        /**
         * Register a definition
         * @description Scope `submit`, rate-limit class `submit`. Registers a validated
         *     definition under its spec `name` at version 1; the stored spec is
         *     the canonical encoding. An existing name is a `409` with code
         *     `conflict` pointing at the versions route — appending versions is
         *     deliberate, never accidental. A definition that fails decoding or
         *     validation is a `400` with code `invalid_definition` and the
         *     `issues` list.
         */
        post: operations["createDefinition"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/definitions/{definition_id}": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * Get one stored definition
         * @description Scope `read`, rate-limit class `read`. One registry row with its stored canonical spec.
         */
        get: operations["getDefinition"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/definitions/{name}/versions": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * List every version of one name
         * @description Scope `read`, rate-limit class `read`. Every version of one definition name, oldest first. An unknown name is a `404` — an empty list would be indistinguishable from a typo.
         */
        get: operations["listDefinitionVersions"];
        put?: never;
        /**
         * Append the next version of an existing name
         * @description Scope `submit`, rate-limit class `submit`. Appends the next
         *     version of an existing name; version numbers are allocated
         *     serially, so concurrent appenders get consecutive versions. The
         *     body's spec `name` must match the path — a mismatch is a `400`,
         *     not a rename. An unknown name is a `404` pointing at
         *     `POST /v1/definitions`. An optional `If-Match` header (the version
         *     the client opened) is an optimistic-concurrency precondition: if the
         *     name's latest version has advanced since, the append is refused with
         *     `409 version_conflict` (a stale save) instead of silently forking.
         */
        post: operations["createDefinitionVersion"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/approvals": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * List human-approval requests
         * @description Scope `read`, rate-limit class `read`. One keyset page of human-approval
         *     records (ADR-017, ticket 15.3), oldest-first — the pending-approval
         *     inbox. Filter by `status` (pending / approved / rejected / expired /
         *     cancelled) and/or `run_id`. Pagination is stable: a page's `next_cursor`
         *     feeds the next request verbatim.
         */
        get: operations["listApprovals"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/approvals/{approvalID}:decide": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        put?: never;
        /**
         * Decide a pending approval
         * @description Scope `approve`, rate-limit class `submit`. Resolves one pending
         *     human-approval request (ADR-017, ticket 15.3) through the single
         *     compare-and-swap arbiter: approve (or a reject routed via
         *     `on_reject: route`) succeeds the parked step with the decision output
         *     `{approval_id, decision, payload, edited, comment, decided_by,
         *     decided_at, source}` and dispatches its successors on the fleet's drain
         *     cadence; a reject under `on_reject: fail` dead-letters the step
         *     (permanent) and applies the run's `on_failure` disposition. An
         *     `edited_payload` is accepted only on approve and only when the gate
         *     permits edits, and is validated against the gate's edit schema. The
         *     actor key id, timestamp, and comment are recorded immutably. The run
         *     must be running or parked; a decision on a run that has since failed or
         *     finished is a 409. A double-decide (or a decision racing a timeout
         *     expiry) is a 409 `approval_not_pending`.
         */
        post: operations["decideApproval"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/plugins": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * List plugins
         * @description Scope `read`, rate-limit class `read`. The plugin catalog
         *     compiled into the API binary (ticket 8.1, ADR-009): every
         *     registered plugin's manifest — kind, name, semver version,
         *     capability flags — with its generated config JSON Schema embedded
         *     verbatim (`config_schema`), the machine-usable form UI config
         *     panels consume. Sorted by kind (executor, tool, retriever,
         *     model_provider, validator) then name. Under the in-process
         *     compilation model the catalog is a build-time property, so the
         *     listing describes the fleet as long as API and workers ship from
         *     the same build with matching test-executor knobs. Model providers
         *     (ticket 8.4) additionally require an API key to be constructed, so
         *     the catalog lists exactly the providers the deployment has
         *     configured — set the same provider keys on API and workers.
         */
        get: operations["listPlugins"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/cache/bust": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        put?: never;
        /**
         * Bust response-cache entries
         * @description Scope `admin`, rate-limit class `admin`. Removes response-cache
         *     entries (ticket 9.6, ADR-011) by namespace: an empty body busts
         *     every entry, `plugin_kind` alone busts one kind (all model
         *     providers, all tools, or all retrievers), and `plugin_kind` with
         *     `plugin_name` busts one concrete plugin. Deletion is `SCAN`-batched
         *     and non-blocking (`UNLINK`); the response reports how many keys were
         *     removed. Semantics are point-in-time — an entry a live worker writes
         *     after the scan passed its slot survives. A single run's entries
         *     cannot be busted (run scope mixes the run id into the entry hash, not
         *     the key; its only bound is the TTL). The action is audit-logged with
         *     the actor key id. Returns `503` when the cache is not enabled on this
         *     API (the cache is an opt-in extra, never a boot dependency).
         */
        post: operations["bustCache"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/cache/stats": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * Response-cache statistics
         * @description Scope `admin`, rate-limit class `admin`. Per-plugin cumulative cache
         *     counters (ticket 9.6, ADR-011): hits, misses, stores, and the
         *     derived hit rate for each concrete plugin the fleet has cached
         *     against. The counters are kept in Redis by the worker fleet's cache
         *     store, so the API serves them without a worker (ADR-002); they
         *     reconcile against the fleet's `engine_cache_*` Prometheus counters on
         *     the normal path. Returns `503` when the cache is not enabled on this
         *     API.
         */
        get: operations["cacheStats"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/keys": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * List API keys
         * @description Scope `admin`, rate-limit class `admin`. Every key, newest first, revoked and expired included — lookup prefixes only, never hashes or plaintext.
         */
        get: operations["listKeys"];
        put?: never;
        /**
         * Mint an API key
         * @description Scope `admin`, rate-limit class `admin`. Mints a bearer key and
         *     returns the plaintext — in this response once and recoverable
         *     nowhere else (only the SHA-256 hash and an 11-character lookup
         *     prefix are stored). Expiry is requested as a TTL (positive Go
         *     duration, e.g. `"720h"`) and resolved against the server's clock;
         *     empty means the key never expires. The first key is minted with
         *     the env-provided root credential (`AGENTLOOM_API_ROOT_KEY`,
         *     implicit admin) — set it, mint a stored admin key, unset it.
         */
        post: operations["createKey"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/keys/{key_id}": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        put?: never;
        post?: never;
        /**
         * Revoke an API key
         * @description Scope `admin`, rate-limit class `admin`. Soft revocation, first wins and idempotent — revoking an already-revoked key is a `204` no-op keeping the original `revoked_at`.
         */
        delete: operations["revokeKey"];
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
}
export type webhooks = Record<string, never>;
export interface components {
    schemas: {
        /** @description A workflow definition document (ADR-003, contract version 1). The schema is generated from the Go structs by `make generate`; this spec references the generated file rather than duplicating it. */
        WorkflowDefinition: components["schemas"]["Definition"];
        /** @description The envelope every non-2xx response carries. */
        Error: {
            error: components["schemas"]["ErrorDetail"];
        };
        /** @description The error payload inside the envelope. */
        ErrorDetail: {
            code: components["schemas"]["ErrorCode"];
            /** @description Human-readable explanation; not machine-stable. */
            message: string;
            /** @description Path-qualified definition problems (decode errors and M1 validation findings, warnings included), present only with code `invalid_definition`. */
            issues?: components["schemas"]["Issue"][];
        };
        /**
         * @description Stable machine-readable error code; renaming one is a breaking change.
         * @enum {string}
         */
        ErrorCode: "invalid_request" | "invalid_definition" | "definition_not_found" | "run_not_found" | "step_not_found" | "key_not_found" | "unauthorized" | "forbidden" | "rate_limited" | "conflict" | "idempotency_key_conflict" | "version_conflict" | "not_found" | "method_not_allowed" | "internal" | "cache_unavailable" | "approval_not_found" | "approval_not_pending" | "approval_decision_invalid" | "stream_unavailable" | "bad_message" | "filter_invalid" | "subscription_limit" | "unknown_subscription";
        /** @description One path-qualified definition problem. */
        Issue: {
            /** @description The dag validation code; empty for codec-level errors. */
            code?: string;
            /** @enum {string} */
            severity: "error" | "warning";
            /** @description JSON path of the offender; empty means the document. */
            path?: string;
            msg: string;
        };
        /**
         * @description Run states. `parked` and `cancelling` are non-terminal; `succeeded`, `failed`, and `cancelled` are terminal (a `failed` run can be re-opened by a dead-letter requeue).
         * @enum {string}
         */
        RunStatus: "running" | "succeeded" | "failed" | "parked" | "cancelling" | "cancelled";
        /**
         * @description Step states. `dead_lettered` is the terminal failure state (`failed` appears only on rows predating migration 0005); `cancelled` steps were written off and may be revived by a requeue; `collected` is a map instance whose terminal failure was tolerated under `on_item_failure: collect_errors` (ADR-015) — the run did not fail on it and the generated gather collected an error marker; `awaiting_human` is a `human_approval` step parked without a lease awaiting a decision (ADR-017) — non-terminal, the run stays running.
         * @enum {string}
         */
        StepStatus: "pending" | "ready" | "running" | "succeeded" | "failed" | "skipped" | "retrying" | "dead_lettered" | "cancelled" | "collected" | "awaiting_human";
        /**
         * @description How an attempt ended: `succeeded`; a judged failure class (`transient`, `permanent`, `timeout` — ADR-006); `validation_failed` (the executor succeeded but the output failed its validation chain, ADR-013 — a genuine ADR-006 class, still excluded from the transport retry budget); `cancelled` by run-level control flow; or an administrative outcome outside the taxonomy — `lost` (lease-expiry takeover), `throttled` (a fleet-wide rate-limit denial deferred the attempt, ADR-010), or `budget_exceeded` (the claim's projected spend would exceed the run budget and the run parked, ADR-012) — none of which is counted against the retry budget.
         * @enum {string}
         */
        AttemptOutcome: "succeeded" | "transient" | "permanent" | "timeout" | "cancelled" | "lost" | "throttled" | "budget_exceeded" | "validation_failed";
        /**
         * @description API key scopes (ADR-007). `admin` implies all; `approve` is reserved until M15 and accepted at key creation.
         * @enum {string}
         */
        Scope: "submit" | "read" | "approve" | "admin";
        /** @description The liveness probe's answer. */
        HealthStatus: {
            /** @enum {string} */
            status: "ok" | "degraded";
        };
        /** @description Exactly one of `definition` (inline document) and `definition_id` (stored definition ref) must be set. */
        SubmitRunRequest: {
            definition?: components["schemas"]["Definition"];
            /**
             * Format: uuid
             * @description A stored definition's UUID (not a name).
             */
            definition_id?: string;
            /** @description Run parameters, stored opaquely and exposed to step config rendering. Any JSON value. */
            params?: unknown;
        };
        /** @description The submitted (or replayed) run. */
        SubmitRunResponse: {
            /** Format: uuid */
            run_id: string;
            status: components["schemas"]["RunStatus"];
            /** @description The step ids made ready at instantiation. */
            entry_steps: string[];
            /** @description True when the Idempotency-Key matched an earlier submission and this is the original run (status `200`). Absent on creation. */
            reused?: boolean;
        };
        /** @description The run row's client-facing projection. */
        RunView: {
            /** Format: uuid */
            id: string;
            /**
             * Format: uuid
             * @description The stored definition this run was submitted from; absent for inline submissions.
             */
            definition_id?: string;
            status: components["schemas"]["RunStatus"];
            /**
             * @description The run's materialized failure policy (ADR-006).
             * @enum {string}
             */
            on_failure: "fail_fast" | "continue_independent_branches";
            steps_total: number;
            steps_succeeded: number;
            steps_failed: number;
            steps_skipped: number;
            steps_cancelled: number;
            /** @description Map instances tolerated under collect_errors (ADR-015) — terminal failures the run did not fail on. */
            steps_collected: number;
            /**
             * @description Why the run is parked; only on parked runs. `budget_exceeded` (M10) and `awaiting_human` (M15) are reserved.
             * @enum {string}
             */
            park_reason?: "manual" | "budget_exceeded" | "awaiting_human";
            /**
             * @description Why the run was cancelled; only on cancelling/cancelled runs.
             * @enum {string}
             */
            cancel_reason?: "manual" | "deadline_exceeded";
            /** Format: date-time */
            created_at: string;
            /** Format: date-time */
            started_at?: string;
            /** Format: date-time */
            finished_at?: string;
            /**
             * Format: date-time
             * @description The materialized max_wall_clock deadline; absent when the definition sets none.
             */
            deadline_at?: string;
            /**
             * Format: int64
             * @description The run's highest event sequence number as of this read (ADR-018, ticket 18.1) — equal to max(events.seq). The dashboard's as-of / resume cursor: patch derived run state from the live feed only for events with a higher seq, and subscribe/backfill the WS stream from this value so a REST row and a WS snapshot resume from an exact point.
             */
            event_seq: number;
            cost: components["schemas"]["CostSummaryView"];
        };
        /**
         * @description A run's cumulative cost and spend budget (ADR-012). Money is integer
         *     nano-USD (1 USD = 1e9) — the exact source of truth; the `*_usd` strings
         *     are the derived human-readable USD rendering. Budget fields are absent
         *     on an unbudgeted run; `on_budget_exceeded` is always present.
         */
        CostSummaryView: {
            /**
             * Format: int64
             * @description Cumulative spend in nano-USD.
             */
            spent_nano_usd: number;
            /**
             * Format: int64
             * @description Cumulative cache savings in nano-USD (ADR-011 counterfactual).
             */
            saved_nano_usd: number;
            /** @description spent_nano_usd rendered as a USD decimal string. */
            spent_usd: string;
            /** @description saved_nano_usd rendered as a USD decimal string. */
            saved_usd: string;
            /**
             * Format: int64
             * @description The run's spend budget in nano-USD; absent when unbudgeted (ticket 10.3).
             */
            budget_nano_usd?: number;
            /** @description budget_nano_usd rendered as a USD decimal string; absent when unbudgeted. */
            budget_usd?: string;
            /**
             * @description Action when a claim's projected spend would exceed the budget.
             * @enum {string}
             */
            on_budget_exceeded: "park" | "fail";
        };
        /** @description One run step with its attempt history. */
        StepView: {
            id: string;
            /** @description The step type from the definition (llm, tool, branch, join, ...). */
            type: string;
            status: components["schemas"]["StepStatus"];
            /** @description Dependencies still unresolved before the step can become ready. */
            remaining_deps: number;
            fired_deps: number;
            attempt_count: number;
            /** @description The successful attempt's output document. Any JSON value. */
            output?: unknown;
            /** @description The last failure document. Any JSON value. */
            error?: unknown;
            /** @description Count of the step's transport-retry failures (attempt outcomes transient/timeout — ADR-006). Disjoint from validation_failures: the transport and semantic retry budgets are separate. */
            transport_failures: number;
            /** @description Count of the step's output-validation failures (attempt outcome validation_failed — ADR-013, ticket 11.4). Disjoint from transport_failures. */
            validation_failures: number;
            /** Format: date-time */
            started_at?: string;
            /** Format: date-time */
            finished_at?: string;
            attempts?: components["schemas"]["AttemptView"][];
            validation?: components["schemas"]["ValidationSummary"];
        };
        /** @description A step's output-validation roll-up (ticket 11.6, ADR-013): a compact summary derived at read time from the step's attempt verdicts. Present only when at least one attempt carried a verdict; absent for every unvalidated step. */
        ValidationSummary: {
            /** @description How many of the step's attempts carried a verdict. */
            attempts: number;
            /** @description Verdicts with overall status pass. */
            passed: number;
            /** @description Verdicts with overall status fail. */
            failed: number;
            /** @description Attempt number of the most recent verdict. */
            last_attempt: number;
            /**
             * @description Overall status of the most recent verdict.
             * @enum {string}
             */
            last_status: "pass" | "fail";
            /**
             * Format: double
             * @description The most recent verdict's overall score (the chain minimum), when one was reported; absent otherwise.
             */
            last_score?: number;
            /** @description Number of issues on the most recent verdict. */
            last_issue_count: number;
            /** @description Per-validator roll-up in the latest verdict's chain order. */
            validators?: components["schemas"]["ValidatorSummary"][];
        };
        /** @description One validator's roll-up across a step's attempts (ticket 11.6). */
        ValidatorSummary: {
            /** @description The validator's plugin name. */
            name: string;
            passed: number;
            failed: number;
            /** @description Results where a cheaper validator already failed (cheap-first ordering). */
            skipped: number;
            /** @description Results where a cost-bearing validator errored under on_error=skip. */
            errored: number;
            /**
             * @description This validator's status on the step's most recent verdict.
             * @enum {string}
             */
            last_status: "pass" | "fail" | "skipped" | "error";
            /**
             * Format: double
             * @description This validator's most recent reported score; absent when it reports none (every deterministic validator).
             */
            last_score?: number;
        };
        /** @description One execution try of a step. */
        AttemptView: {
            /** @description 1-based durable attempt number. */
            attempt: number;
            /**
             * Format: uuid
             * @description The fencing token the attempt held.
             */
            claim_id: string;
            outcome?: components["schemas"]["AttemptOutcome"];
            /** @description The attempt's failure document. Any JSON value. */
            error?: unknown;
            /** @description Token accounting for a successful llm attempt (ticket 8.6); absent for every other step type and outcome. Feeds M10's cost ledger. On a response-cache hit (ticket 9.5) the attempt carries the counterfactual "would-have-cost" usage marked cache_hit=true — the tokens were not spent, so this records the savings rather than real usage. */
            usage?: {
                /** Format: int64 */
                input_tokens?: number;
                /** Format: int64 */
                output_tokens?: number;
                /** @description True when the attempt was served from the response cache (ticket 9.5); the token counts are counterfactual, not spend. Absent on every real (non-cached) attempt. */
                cache_hit?: boolean;
            };
            verdict?: components["schemas"]["Verdict"];
            repair?: components["schemas"]["RepairProvenance"];
            feedback?: components["schemas"]["Feedback"];
            /** Format: date-time */
            started_at?: string;
            /** Format: date-time */
            finished_at?: string;
        };
        /** @description The semantic-retry critique an attempt was given (ticket 11.4, ADR-013). Present on a feedback-augmented re-attempt of a step with a semantic policy (validation.max_attempts >= 2); absent on a first attempt. The `text` is the critique folded into the attempt's request. */
        Feedback: {
            /** @description Feedback record version (1). */
            schema_version: number;
            /** @description 1-based semantic attempt number this critique is for (2 on the first re-attempt). */
            semantic_attempt: number;
            /** @description The step's semantic-retry budget (validation.max_attempts). */
            max_attempts: number;
            /** @description The durable attempt number whose rejected output produced this critique. */
            prior_attempt: number;
            /** @description The rendered critique folded into this attempt's request. Empty for a step type whose executor injects no text (it still re-attempts, identically). */
            text?: string;
        };
        /** @description Structured-output provenance for an attempt of an llm step that declared an output_format (ticket 11.3, ADR-013): how the completion was shaped into structured JSON before the validate stage. Absent for every step without an output_format. */
        RepairProvenance: {
            /** @description Repair record version (1). */
            schema_version: number;
            /**
             * @description native = the provider emitted structured output directly; raw = the completion text was already valid JSON; repaired = a deterministic pass fixed it; unrepairable = no pass produced valid JSON (left to the semantic-retry loop).
             * @enum {string}
             */
            status: "native" | "raw" | "repaired" | "unrepairable";
            /** @description The repair passes that changed the text, in order (structure only). */
            steps?: string[];
            /** @description The model's pre-repair text, present only when status is `repaired`. */
            raw_text?: string;
        };
        /** @description The output-validation chain verdict for an attempt whose step carried a validation chain (ticket 11.1, ADR-013). Present on a succeeded attempt (a passing verdict) and on a validation_failed attempt (a failing verdict with issues). Absent for unvalidated steps. */
        Verdict: {
            /** @description Verdict schema version (1). */
            schema_version: number;
            /**
             * @description The chain's overall judgment.
             * @enum {string}
             */
            status: "pass" | "fail";
            /** @description Overall [0,1] score — the minimum of the per-validator reported scores. Absent when no validator reported a score. */
            score?: number;
            /** @description Every failing validator's issues, in chain order. */
            issues?: components["schemas"]["VerdictIssue"][];
            /** @description Per-validator breakdown, one entry per configured validator. */
            results?: components["schemas"]["ValidatorResult"][];
        };
        /** @description One problem a validator found with the output. */
        VerdictIssue: {
            /** @description The validator that raised the issue. */
            validator: string;
            /** @description Machine-readable issue code (e.g. type_mismatch, target_not_found). */
            code: string;
            /** @description RFC 6901 JSON pointer into the validated value; empty means the whole value. */
            path?: string;
            /** @description Human description of the problem — structure only, never instance values. */
            message: string;
        };
        /** @description One validator's contribution to a chain verdict. */
        ValidatorResult: {
            name: string;
            /**
             * @description This validator's judgment. `skipped` = a cost-bearing validator the chain did not run because a cheaper one already failed. `error` = a cost-bearing validator (llm_judge) that errored under `on_error: skip`, so the chain did not fail but the judgment could not be rendered.
             * @enum {string}
             */
            status: "pass" | "fail" | "skipped" | "error";
            score?: number;
            issue_count: number;
            /** @description A cost-bearing validator's (llm_judge) explanation of its judgment, on a pass and a fail alike. Absent for deterministic validators. */
            rationale?: string;
            /** @description A cost-bearing validator's token accounting; present only when the validator made a metered provider call. The engine ledgers this as overhead on the serving step (ADR-012 rule 4). */
            usage?: components["schemas"]["ValidatorUsage"];
            /** @description The suppressed failure message when status is `error` — a judge that errored under `on_error: skip`. */
            error?: string;
            /** Format: int64 */
            duration_ms?: number;
        };
        /** @description A cost-bearing validator's token accounting (ticket 11.5): the resource its call bills to and the judge model's token counts. Carries no output or rubric text. */
        ValidatorUsage: {
            /** @description The <provider>:<model> resource the judge call bills to. */
            resource: string;
            model: string;
            /** Format: int64 */
            input_tokens: number;
            /** Format: int64 */
            output_tokens: number;
        };
        /** @description One run edge with its resolution — enough to render the graph and see which paths fired. */
        EdgeView: {
            from: string;
            to: string;
            /** @enum {string} */
            type: "normal" | "loop";
            /** @enum {string} */
            resolution: "unresolved" | "fired" | "skipped";
        };
        /** @description One DLQ record. `seq` counts the step's deaths; `attempts_at_death` is the baseline a requeue re-arms the retry budget from. */
        DeadLetterView: {
            step_id: string;
            seq: number;
            /**
             * @description Why the step dead-lettered: a retryable class ran out of budget, a never-retryable failure, or the queue's poison threshold (delivery crash loop — `class` absent).
             * @enum {string}
             */
            source: "retries_exhausted" | "permanent" | "poison";
            /**
             * @description The judged failure class; absent for poison.
             * @enum {string}
             */
            class?: "transient" | "permanent" | "timeout";
            /** @description The failure document at death. Any JSON value. */
            error?: unknown;
            attempts_at_death: number;
            /** Format: date-time */
            created_at: string;
        };
        /** @description The full run view — run row, steps with attempts, edges, DLQ records, approval records. */
        RunResponse: {
            run: components["schemas"]["RunView"];
            steps: components["schemas"]["StepView"][];
            edges: components["schemas"]["EdgeView"][];
            /** @description The run's DLQ records — how a client discovers requeueable steps. Absent for healthy runs. */
            dead_letters?: components["schemas"]["DeadLetterView"][];
            /** @description The run's human-approval records (ADR-017) — pending gates awaiting a decision, plus any decided/cancelled. Absent for runs without an approval gate. */
            approvals?: components["schemas"]["ApprovalView"][];
        };
        /** @description One human-approval record (ADR-017, ticket 15.2): the rendered content shown to an approver and, once decided (15.3), the immutable decision. In 15.2 only `pending` and `cancelled` statuses appear. */
        ApprovalView: {
            /** Format: uuid */
            id: string;
            /**
             * Format: uuid
             * @description The approval's run. Present on the GET /v1/approvals list; omitted on the run-status view (already run-scoped).
             */
            run_id?: string;
            step_id: string;
            attempt: number;
            /** @enum {string} */
            status: "pending" | "approved" | "rejected" | "expired" | "cancelled";
            /** @description Rendered headline shown to the approver. */
            title: string;
            description?: string;
            /** @description The proposed action shown for review. Any JSON value. */
            payload?: unknown;
            allowed_decisions: ("approve" | "reject")[];
            allow_edit?: boolean;
            /** @description JSON Schema constraining an edited payload; absent if any JSON edit is accepted. */
            edit_schema?: unknown;
            /**
             * Format: date-time
             * @description When the timeout policy fires (ADR-017); absent = wait indefinitely.
             */
            timeout_at?: string;
            /**
             * @description The recorded decision; present once decided (15.3).
             * @enum {string}
             */
            decision?: "approve" | "reject";
            /** @description The approver's edited payload, if any. Any JSON value. */
            edited_payload?: unknown;
            comment?: string;
            /** @description The deciding key id, or `system:timeout` for a timeout decision. */
            decided_by?: string;
            /** Format: date-time */
            decided_at?: string;
            /** @enum {string} */
            decision_source?: "human" | "timeout";
            /**
             * Format: date-time
             * @description Set once a timeout policy fired (ticket 15.4). A reject/approve policy sets status `expired`; a `park` policy leaves status `pending` (still decidable) with this marking that the run was parked.
             */
            expired_at?: string;
            /** Format: date-time */
            created_at: string;
        };
        /** @description One keyset page of approvals (ADR-017, ticket 15.3), oldest-first — the pending-approval inbox. `next_cursor` is the opaque cursor for the next page; absent on the last page. */
        ApprovalListResponse: {
            approvals: components["schemas"]["ApprovalView"][];
            /** @description Opaque cursor for the next page; absent when the page is the last. */
            next_cursor?: string;
        };
        /** @description A decision on a pending approval (ADR-017, ticket 15.3): the decision, an optional edited payload (approve only, constrained by the gate's edit schema), and an optional comment recorded in the audit trail. */
        DecideApprovalRequest: {
            /** @enum {string} */
            decision: "approve" | "reject";
            /** @description Replaces the original payload as the step's output payload (approve only, and only when the gate permits edits). Any JSON value. */
            edited_payload?: unknown;
            /** @description Optional approver note, recorded immutably. */
            comment?: string;
        };
        /** @description The decision result (ticket 15.3): the decided approval (carrying the immutable decision record), the run rollup after settlement, and the successors this decision made ready. The settled step's status is visible via GET /v1/runs/{id}. */
        DecideApprovalResponse: {
            approval: components["schemas"]["ApprovalView"];
            run: components["schemas"]["RunView"];
            readied_steps: string[];
        };
        /** @description A run's current versioned graph with provenance (ADR-015, ticket 13.6): the definition-authored graph is version 1, and each planner / map / loop expansion bumps the version. Every node and edge carries the version at which it was introduced; `expansions` is the ordered per-version delta feed reconstructed from the run's `graph_expanded` events. A client reconstructs any version N by keeping the nodes and edges whose `graph_version` is at most N. */
        RunGraphResponse: {
            /** Format: uuid */
            run_id: string;
            /** @description The current (highest) version = expansion count + 1. */
            graph_version: number;
            steps_total: number;
            nodes: components["schemas"]["GraphNodeView"][];
            edges: components["schemas"]["GraphEdgeView"][];
            /** @description The ordered per-version expansion deltas; empty for a run that never expanded. */
            expansions: components["schemas"]["GraphExpansionView"][];
            /**
             * Format: int64
             * @description The run's highest event sequence number as of this read (`runs.next_seq` = `max(events.seq)`; ticket 18.2) — the dashboard's as-of / resume cursor. A live `graph_expanded` event is folded over this graph only when its `seq` exceeds this value, so a graph read and the event feed reconcile without double-applying an expansion this response already reflects.
             */
            event_seq: number;
        };
        /** @description A node's or edge's provenance. `kind` is `definition` for an authored row, or the expansion kind (`planner` / `map` / `loop`) with `step` naming the step whose completion injected the row. */
        GraphOriginView: {
            /** @enum {string} */
            kind: "definition" | "planner" | "map" | "loop";
            /** @description The injecting step id; absent for an authored (definition) row. */
            step?: string;
        };
        /** @description One graph node with its live status and provenance. */
        GraphNodeView: {
            id: string;
            type: string;
            status: string;
            /** @description Expansion nesting depth; 0 for an authored node. */
            depth: number;
            /** @description The version at which the node was introduced. */
            graph_version: number;
            origin: components["schemas"]["GraphOriginView"];
            /**
             * Format: date-time
             * @description Run creation time for authored nodes; the expansion event time for injected ones.
             */
            added_at: string;
            position?: components["schemas"]["GraphPositionView"];
        };
        /** @description An authored node's canvas position hint (ticket 18.2), lifted from the run's definition snapshot `ui.nodes.<id>.position`. Present only for an authored node whose definition carried a `ui` hint; injected nodes have none and are laid out incrementally by the dashboard. */
        GraphPositionView: {
            x: number;
            y: number;
        };
        /** @description One graph edge with its resolution and provenance. */
        GraphEdgeView: {
            from: string;
            to: string;
            /** @enum {string} */
            type: "normal" | "loop";
            /** @description The edge's CEL guard, when conditioned; absent otherwise. */
            when?: string;
            /**
             * @description The human-approval routing marker (ticket 15.3) on an edge leaving an approval gate; absent otherwise. The dashboard renders such an edge from the matching source port (ticket 18.2).
             * @enum {string}
             */
            decision?: "approve" | "reject";
            /** @enum {string} */
            resolution: "unresolved" | "fired" | "skipped";
            graph_version: number;
            origin: components["schemas"]["GraphOriginView"];
        };
        /** @description An edge introduced by an expansion, by endpoints and type. */
        GraphEdgeRef: {
            from: string;
            to: string;
            /** @enum {string} */
            type: "normal" | "loop";
        };
        /** @description One expansion's delta, reconstructed from a `graph_expanded` event. `version` is the version this expansion produced (`from_version` + 1); `readied` are the injected steps made immediately runnable and `widened` are the pre-existing pending steps whose dependency counts it grew. */
        GraphExpansionView: {
            version: number;
            from_version: number;
            origin_step: string;
            /** @enum {string} */
            origin_kind: "planner" | "map" | "loop";
            depth: number;
            /** Format: date-time */
            added_at: string;
            added_steps: string[];
            added_edges: components["schemas"]["GraphEdgeRef"][];
            readied?: string[];
            widened?: string[];
        };
        /** @description A run's cost breakdown (ADR-012): cumulative summary, per-step and per-resource (per-model / per-tool) roll-ups, and the full per-attempt ledger. */
        RunCostResponse: {
            /** Format: uuid */
            run_id: string;
            summary: components["schemas"]["CostSummaryView"];
            by_step: components["schemas"]["CostByStepView"][];
            by_resource: components["schemas"]["CostByResourceView"][];
            entries: components["schemas"]["CostEntryView"][];
        };
        /** @description One step's spend/savings roll-up. */
        CostByStepView: {
            step_id: string;
            /**
             * Format: int64
             * @description Number of ledger rows attributed to the step.
             */
            entries: number;
            /** Format: int64 */
            spent_nano_usd: number;
            /** Format: int64 */
            saved_nano_usd: number;
            /**
             * Format: int64
             * @description The slice of spend attributed to validation machinery — an llm_judge's provider call (ADR-012 rule 4). Zero for steps with no cost-bearing validators.
             */
            overhead_nano_usd: number;
            spent_usd: string;
            saved_usd: string;
            overhead_usd: string;
        };
        /** @description One model's or tool's spend/savings roll-up. `resource` is the model name (`mock:sim-1`) or `tool:<name>`; token sums are zero for tool rows. */
        CostByResourceView: {
            resource: string;
            /** Format: int64 */
            entries: number;
            /** Format: int64 */
            input_tokens: number;
            /** Format: int64 */
            output_tokens: number;
            /** Format: int64 */
            spent_nano_usd: number;
            /** Format: int64 */
            saved_nano_usd: number;
            spent_usd: string;
            saved_usd: string;
        };
        /** @description One cost_ledger row: what a single attempt cost, at what rate, with what provenance. Cache-hit rows carry spend 0 and a saved figure. */
        CostEntryView: {
            step_id: string;
            /** @description The 1-based attempt number this charge is for. */
            attempt: number;
            /** @description The charge kind — `attempt` for the productive call; overhead kinds (M11/M12) reserved. */
            entry: string;
            /** @description The cost resource — model name or `tool:<name>`. */
            resource: string;
            /** @description The attempt's token accounting; absent for tool rows. Any JSON object. */
            usage?: unknown;
            /** @description The resolved rate snapshot that priced the row. Any JSON object. */
            rate: unknown;
            /**
             * @description How the rate resolved.
             * @enum {string}
             */
            rate_source: "exact" | "wildcard" | "fallback";
            /** @description The attempt was served from the response cache — spend 0, with a counterfactual saved figure. */
            cache_hit: boolean;
            /** @description A judge/summarization charge (ADR-012 rule 4); always false in M10. */
            overhead: boolean;
            /** Format: int64 */
            spent_nano_usd: number;
            /** Format: int64 */
            saved_nano_usd: number;
            /** Format: date-time */
            created_at: string;
        };
        /** @description One keyset page of runs, newest first. */
        ListRunsResponse: {
            runs: components["schemas"]["RunView"][];
            /** @description Opaque cursor fetching the next page of runs; absent on the last page. */
            next_cursor?: string;
        };
        /** @description What the cancel request settled immediately. */
        CancelRunResponse: {
            run: components["schemas"]["RunView"];
            /** @description Claimless steps the request's sweep cancelled immediately. */
            cancelled_steps: string[];
            /** @description True when nothing was in flight and the run went straight to `cancelled`; false means it rests at `cancelling` until in-flight steps settle. */
            finalized: boolean;
        };
        /** @description The parked run. */
        ParkRunResponse: {
            run: components["schemas"]["RunView"];
        };
        /** @description The resumed run and what was re-dispatched. */
        UnparkRunResponse: {
            run: components["schemas"]["RunView"];
            /** @description Ready steps re-dispatched because their deliveries were consumed while parked. */
            dispatched: string[];
        };
        /** @description The new run spend budget in US dollars (ticket 10.3). */
        SetBudgetRequest: {
            /** @description The new spend budget in US dollars; must be positive. */
            budget_usd: number;
        };
        /** @description The run whose spend budget was raised, with its refreshed cost summary. */
        SetBudgetResponse: {
            run: components["schemas"]["RunView"];
        };
        /** @description The requeued step, back in ready with its budget re-armed. */
        RequeueStepResponse: {
            /** Format: uuid */
            run_id: string;
            step_id: string;
            status: components["schemas"]["StepStatus"];
            /** @description True when the requeue re-opened a failed run. */
            run_resumed: boolean;
            /** @description Written-off descendant steps brought back to pending. */
            revived?: string[];
            /** @description Steps re-dispatched by the requeue (the requeued step plus any stranded ready steps). */
            dispatched: string[];
        };
        /**
         * @description Step-log severity, canonicalized at capture.
         * @enum {string}
         */
        LogLevel: "debug" | "info" | "warn" | "error";
        /** @description One captured executor log line. */
        StepLogLineView: {
            /**
             * Format: int64
             * @description The line's position in the attempt's log stream, 1-based and monotonic; gaps mark lines lost to the capture buffer or the ring cap.
             */
            seq: number;
            level: components["schemas"]["LogLevel"];
            /** @description The log message, truncated with an explicit marker when oversized. */
            message: string;
            /** @description The executor call-site attributes as one JSON object; absent when the line had none. */
            fields?: Record<string, never>;
            /** @description The attempt's trace id (hex) for joining to traces; absent when tracing was off at capture. */
            trace_id?: string;
            /** Format: date-time */
            logged_at: string;
        };
        /** @description One ascending-seq keyset page of one attempt's captured log lines. */
        StepLogsResponse: {
            /** Format: uuid */
            run_id: string;
            step_id: string;
            /** @description The attempt served; 0 when the step has never been attempted. */
            attempt: number;
            lines: components["schemas"]["StepLogLineView"][];
            /** @description True when the attempt lost lines to the ring cap or capture buffer; the retained window is the newest lines. */
            truncated: boolean;
            /**
             * Format: int64
             * @description How many lines were lost (derived as max seq minus stored rows). Absent when zero.
             */
            dropped_lines?: number;
            /** @description Opaque cursor fetching the next page of lines; absent on the last page. */
            next_cursor?: string;
        };
        /** @description One version of one run-scoped blackboard key. */
        BlackboardEntryView: {
            key: string;
            version: number;
            /** @description The stored JSON value (any JSON — string, object, array, number). */
            value: unknown;
            /** @description The value's token size under token_counter. */
            token_count: number;
            /** @description The token counter fingerprint that produced token_count (e.g. "openai/o200k_base@1"). */
            token_counter: string;
            /** @description The entry's labels; may include the reserved "pinned" tag. */
            tags: string[];
            /** @description The step that wrote this version; absent for a non-step writer. */
            author_step_id?: string;
            /** @description The 1-based attempt of the authoring step; absent for a non-step writer. */
            author_attempt?: number;
            /** Format: date-time */
            created_at: string;
        };
        /** @description One keyset page of a run's blackboard — each key's head, or every version when history is true. */
        BlackboardResponse: {
            /** Format: uuid */
            run_id: string;
            /** @description True when entries are the full version history; false when they are per-key heads. */
            history: boolean;
            entries: components["schemas"]["BlackboardEntryView"][];
            /** @description Opaque cursor fetching the next page; absent on the last page. */
            next_cursor?: string;
        };
        /** @description The body of definition create and new-version requests. */
        CreateDefinitionRequest: {
            definition: components["schemas"]["Definition"];
        };
        /** @description One registry row's summary — everything but the spec. */
        DefinitionView: {
            /** Format: uuid */
            id: string;
            name: string;
            version: number;
            /** Format: date-time */
            created_at: string;
        };
        /** @description A registry row with its stored canonical spec. */
        DefinitionResponse: components["schemas"]["DefinitionView"] & {
            spec: components["schemas"]["Definition"];
        };
        /** @description One keyset page of definitions (latest per name), in name order. */
        ListDefinitionsResponse: {
            definitions: components["schemas"]["DefinitionView"][];
            /** @description Opaque cursor fetching the next page of definitions; absent on the last page. */
            next_cursor?: string;
        };
        /** @description Every stored version of one definition name. */
        DefinitionVersionsResponse: {
            /** @description Every version of the name, oldest first. */
            versions: components["schemas"]["DefinitionView"][];
        };
        /** @description The key-mint request (ADR-007). */
        CreateKeyRequest: {
            /** @description Human label shown in listings. */
            name: string;
            scopes: components["schemas"]["Scope"][];
            /** @description Optional positive Go duration (e.g. "720h") resolved to an absolute expiry against the server's clock. Empty means the key never expires. */
            ttl?: string;
        };
        /** @description One API key's client-facing projection: the 11-character lookup prefix is the only key material it ever carries. */
        KeyView: {
            /** Format: uuid */
            id: string;
            /** @description The first 11 characters of the plaintext key — for matching, not authentication. */
            prefix: string;
            name: string;
            scopes: components["schemas"]["Scope"][];
            /** Format: date-time */
            created_at: string;
            /** Format: date-time */
            expires_at?: string;
            /** Format: date-time */
            revoked_at?: string;
        };
        /** @description The minted key. `key` is the plaintext credential, shown in this response once and recoverable nowhere else. */
        CreateKeyResponse: components["schemas"]["KeyView"] & {
            /** @description The plaintext bearer key (sk_ + 43 base64url chars). */
            key: string;
        };
        /** @description Every API key, newest first, revoked and expired included. */
        ListKeysResponse: {
            keys: components["schemas"]["KeyView"][];
        };
        /**
         * @description The plugin kind (ADR-009's closed vocabulary).
         * @enum {string}
         */
        PluginKind: "executor" | "tool" | "retriever" | "model_provider" | "validator";
        /** @description ADR-009's capability flags. All three are always present — a flag's absence and its falseness are the same statement. */
        PluginCapabilities: {
            /** @description The plugin performs externally observable side effects; its executions journal, and its outputs are never cached. */
            side_effectful: boolean;
            /** @description The output is a function of (config, input) and caching is semantically sound — M9's response-cache eligibility. */
            cacheable: boolean;
            /** @description Executing the plugin spends money; M10 meters it. */
            cost_bearing: boolean;
        };
        /** @description One plugin's manifest, as registered at boot. */
        PluginInfo: {
            kind: components["schemas"]["PluginKind"];
            /** @description The plugin's name within its kind (for executors, the step type). Lowercase snake_case, at most 64 characters. */
            name: string;
            /** @description Semver version string. Feeds M9's cache keys, so it bumps on any behavior change that should invalidate cached outputs; dev stubs carry a 0.x `-stub` pre-release. */
            version: string;
            /** @description Optional one-line human description. */
            description?: string;
            capabilities: components["schemas"]["PluginCapabilities"];
            /** @description The plugin's generated config JSON Schema (draft 2020-12), embedded verbatim — generated from the same Go structs the engine decodes with, so it cannot drift. Absent when the plugin takes no config. */
            config_schema?: {
                [key: string]: unknown;
            };
        };
        /** @description The plugin catalog, sorted by kind then name. */
        ListPluginsResponse: {
            plugins: components["schemas"]["PluginInfo"][];
        };
        /** @description The namespace selector for a cache bust. Omit both fields to bust every entry; `plugin_kind` alone busts one kind; `plugin_kind` with `plugin_name` busts one concrete plugin. `plugin_name` without `plugin_kind` is a 400. */
        CacheBustRequest: {
            /**
             * @description The cacheable plugin kind to bust.
             * @enum {string}
             */
            plugin_kind?: "model_provider" | "tool" | "retriever";
            /** @description The concrete plugin within the kind; requires plugin_kind. */
            plugin_name?: string;
        };
        /** @description The outcome of a bust. */
        CacheBustResponse: {
            /**
             * Format: int64
             * @description Number of cache entries removed. Point-in-time — entries written concurrently after the scan passed their slot are not counted.
             */
            deleted: number;
        };
        /** @description Per-plugin cumulative cache counters, sorted by kind then name. */
        CacheStatsResponse: {
            plugins: components["schemas"]["CachePluginStat"][];
        };
        /** @description One concrete plugin's cache counters with the derived hit rate. */
        CachePluginStat: {
            /** @enum {string} */
            kind: "model_provider" | "tool" | "retriever";
            name: string;
            /** Format: int64 */
            hits: number;
            /** Format: int64 */
            misses: number;
            /** Format: int64 */
            stores: number;
            /**
             * Format: double
             * @description hits / (hits + misses); 0 when there were no lookups.
             */
            hit_rate: number;
        };
        /** @description A short-lived signed ticket for the run event WebSocket (ticket 16.3). Opaque — passed back verbatim as the `ticket` query parameter. */
        WSTicketResponse: {
            /** @description The opaque signed ticket. */
            ticket: string;
            /**
             * Format: date-time
             * @description When the ticket stops being accepted.
             */
            expires_at: string;
        };
        /** @description The first WebSocket frame (ticket 16.3): the run's current state, the same body as GET /v1/runs/{id}. Event frames follow. */
        WSSnapshotFrame: {
            /**
             * @description discriminator enum property added by openapi-typescript
             * @enum {string}
             */
            type: "WSSnapshotFrame";
            run: components["schemas"]["RunResponse"];
        };
        /** @description One normalized event envelope (ticket 16.1/16.3), backfilled then live. Clients dedupe and order by (run_id, seq). The `event` schema is the generated event feed envelope (docs/schema/events.v1.json). On the firehose (16.4), `subscriptions` names the subscription ids this envelope matched; it is absent on the single-subscription run WebSocket. */
        WSEventFrame: {
            /**
             * @description discriminator enum property added by openapi-typescript
             * @enum {string}
             */
            type: "WSEventFrame";
            event: components["schemas"]["Envelope"];
            /** @description Firehose only — the matched subscription ids. */
            subscriptions?: string[];
        };
        /** @description Firehose (ticket 16.4): acknowledges a `subscribe`, echoing the effective filter. */
        WSSubscribedFrame: {
            /**
             * @description discriminator enum property added by openapi-typescript
             * @enum {string}
             */
            type: "WSSubscribedFrame";
            id: string;
            filter: components["schemas"]["WSFilter"];
        };
        /** @description Firehose (ticket 16.4): acknowledges an `unsubscribe`. */
        WSUnsubscribedFrame: {
            /**
             * @description discriminator enum property added by openapi-typescript
             * @enum {string}
             */
            type: "WSUnsubscribedFrame";
            id: string;
        };
        /** @description Firehose (ticket 16.4): marks the end of a subscription's cursor backfill. `cursors` reports the highest seq delivered per resumed run. */
        WSFirehoseCaughtUpFrame: {
            /**
             * @description discriminator enum property added by openapi-typescript
             * @enum {string}
             */
            type: "WSFirehoseCaughtUpFrame";
            id: string;
            cursors: {
                [key: string]: number;
            };
        };
        /** @description Firehose subscription filter (ticket 16.4). An empty filter matches every event. Fields are ANDed; values within run_ids/types are ORed. */
        WSFilter: {
            run_ids?: string[];
            types?: string[];
            /** Format: uuid */
            definition_id?: string;
            definition_name?: string;
        };
        /** @description Marks the end of the backfill and the start of the live tail (ticket 16.3). `last_seq` is the client's resume cursor at this point. */
        WSCaughtUpFrame: {
            /**
             * @description discriminator enum property added by openapi-typescript
             * @enum {string}
             */
            type: "WSCaughtUpFrame";
            /** Format: int64 */
            last_seq: number;
        };
        /** @description Sent before a non-normal WebSocket close (run WS, ticket 16.3) or in-band as a control-message rejection that leaves the connection open (firehose, ticket 16.4), so the client has a machine-readable reason. `id`, when present, names the subscription the error concerns. */
        WSErrorFrame: {
            /**
             * @description discriminator enum property added by openapi-typescript
             * @enum {string}
             */
            type: "WSErrorFrame";
            code: string;
            message: string;
            id?: string;
        };
        /** @enum {string} */
        FailurePolicy: "fail_fast" | "continue_independent_branches";
        /** @enum {string} */
        BudgetPolicy: "park" | "fail";
        ExpansionPolicy: {
            max_added_steps?: number;
            max_total_steps?: number;
            max_expansions?: number;
            max_depth?: number;
        };
        LLMMessage: {
            role?: string;
            content?: string;
        };
        ModelFallback: {
            model?: string;
            at_budget_fraction?: number;
        };
        /** @enum {string} */
        OutputFormatType: "json" | "json_schema";
        /** @enum {string} */
        OutputFormatMode: "auto" | "repair_only";
        OutputFormat: {
            type?: components["schemas"]["OutputFormatType"];
            schema?: unknown;
            mode?: components["schemas"]["OutputFormatMode"];
        };
        LLMConfig: {
            model?: string;
            system?: string;
            prompt?: string;
            messages?: components["schemas"]["LLMMessage"][];
            max_tokens?: number;
            temperature?: number;
            model_fallbacks?: components["schemas"]["ModelFallback"][];
            output_format?: components["schemas"]["OutputFormat"];
        };
        ToolConfig: {
            tool?: string;
            input?: unknown;
        };
        RetrieveConfig: {
            retriever?: string;
            query?: string;
            top_k?: number;
        };
        /** @enum {string} */
        ItemFailurePolicy: "fail_fast" | "collect_errors";
        MapConfig: {
            items?: unknown;
            body?: string;
            max_items?: number;
            on_item_failure?: components["schemas"]["ItemFailurePolicy"];
        };
        GatherConfig: {
            items?: unknown;
        };
        PlannerConfig: {
            model?: string;
            prompt?: string;
            messages?: components["schemas"]["LLMMessage"][];
            max_tokens?: number;
            temperature?: number;
            max_added_steps?: number;
        };
        AgentConfig: {
            agent?: string;
            system?: string;
            model?: string;
            prompt?: string;
            messages?: components["schemas"]["LLMMessage"][];
            max_tokens?: number;
            temperature?: number;
            model_fallbacks?: components["schemas"]["ModelFallback"][];
            output_format?: components["schemas"]["OutputFormat"];
            tools?: string[];
            role?: string;
        };
        /** @enum {string} */
        ApprovalDecision: "approve" | "reject";
        /** @enum {string} */
        ApprovalTimeoutPolicy: "reject" | "approve" | "park";
        /** @enum {string} */
        ApprovalRejectPolicy: "fail" | "route";
        HumanApprovalConfig: {
            title?: string;
            description?: string;
            payload?: unknown;
            allowed_decisions?: components["schemas"]["ApprovalDecision"][];
            allow_edit?: boolean;
            edit_schema?: unknown;
            timeout?: string;
            on_timeout?: components["schemas"]["ApprovalTimeoutPolicy"];
            on_reject?: components["schemas"]["ApprovalRejectPolicy"];
        };
        /** @enum {string} */
        JoinMode: "all" | "any";
        JoinConfig: {
            mode?: components["schemas"]["JoinMode"];
        };
        BranchConfig: {
            input?: unknown;
        };
        NoopConfig: Record<string, never>;
        EchoConfig: {
            input?: unknown;
        };
        SleepConfig: {
            duration?: string;
        };
        FailNTimesConfig: {
            n?: number;
        };
        CounterConfig: {
            path?: string;
        };
        EffectfulEchoConfig: {
            path?: string;
            input?: unknown;
            fail_times?: number;
        };
        BlackboardWriteConfig: {
            key?: string;
            value?: unknown;
            tags?: string[];
            expected_version?: number;
            read_key?: string;
        };
        /** @enum {string} */
        StepType: "llm" | "tool" | "retrieve" | "map" | "gather" | "planner" | "agent" | "human_approval" | "join" | "branch" | "noop" | "echo" | "sleep" | "fail_n_times" | "counter" | "effectful_echo" | "blackboard_write";
        BackoffSpec: {
            initial?: string;
            cap?: string;
            multiplier?: number;
        };
        /** @enum {string} */
        JitterMode: "full" | "none";
        /** @enum {string} */
        ErrorClass: "transient" | "permanent" | "timeout" | "cancelled" | "validation_failed";
        RetryPolicy: {
            max_attempts?: number;
            backoff?: components["schemas"]["BackoffSpec"];
            jitter?: components["schemas"]["JitterMode"];
            retry_on?: components["schemas"]["ErrorClass"][];
        };
        /** @enum {string} */
        CacheMode: "off" | "read_write" | "read_only";
        /** @enum {string} */
        CacheScope: "global" | "run";
        CachePolicy: {
            mode?: components["schemas"]["CacheMode"];
            ttl?: string;
            scope?: components["schemas"]["CacheScope"];
        };
        StepBudget: {
            max_usd?: number;
            max_tokens?: number;
        };
        ValidatorSpec: {
            name: string;
            config?: unknown;
            target?: string;
        };
        FeedbackPolicy: {
            template?: string;
            max_output_chars?: number;
        };
        ValidationPolicy: {
            validators?: components["schemas"]["ValidatorSpec"][];
            max_attempts?: number;
            feedback?: components["schemas"]["FeedbackPolicy"];
        };
        BlackboardWrite: {
            key: string;
            from?: string;
            tags?: string[];
            pinned?: boolean;
        };
        BlackboardPolicy: {
            write?: components["schemas"]["BlackboardWrite"][];
        };
        /** @enum {string} */
        ContextSourceKind: "step_output" | "blackboard" | "retrieval" | "literal" | "thread";
        /** @enum {string} */
        ContextMissingPolicy: "error" | "skip";
        ContextSource: {
            kind: components["schemas"]["ContextSourceKind"];
            name?: string;
            step?: string;
            path?: string;
            key?: string;
            role?: string;
            tags?: string[];
            retriever?: string;
            query?: string;
            top_k?: number;
            text?: string;
            max_tokens?: number;
            pinned?: boolean;
            priority?: number;
            on_missing?: components["schemas"]["ContextMissingPolicy"];
        };
        CompactionStrategy: {
            strategy: string;
            n?: number;
            min_tokens?: number;
            model?: string;
            key?: string;
            max_tokens?: number;
            timeout?: string;
        };
        ContextSpec: {
            sources?: components["schemas"]["ContextSource"][];
            budget_tokens?: number;
            compaction?: components["schemas"]["CompactionStrategy"][];
        };
        Step: {
            id: string;
            type: components["schemas"]["StepType"];
            /** @description Typed per step type; Step's oneOf variants bind each type to its config shape. */
            config?: Record<string, never>;
            retry?: components["schemas"]["RetryPolicy"];
            timeout?: string;
            cache?: components["schemas"]["CachePolicy"];
            budget?: components["schemas"]["StepBudget"];
            validation?: components["schemas"]["ValidationPolicy"];
            blackboard?: components["schemas"]["BlackboardPolicy"];
            context?: components["schemas"]["ContextSpec"];
        } & ({
            /** @constant */
            type?: "llm";
            config?: components["schemas"]["LLMConfig"];
        } | {
            /** @constant */
            type?: "tool";
            config?: components["schemas"]["ToolConfig"];
        } | {
            /** @constant */
            type?: "retrieve";
            config?: components["schemas"]["RetrieveConfig"];
        } | {
            /** @constant */
            type?: "map";
            config?: components["schemas"]["MapConfig"];
        } | {
            /** @constant */
            type?: "gather";
            config?: components["schemas"]["GatherConfig"];
        } | {
            /** @constant */
            type?: "planner";
            config?: components["schemas"]["PlannerConfig"];
        } | {
            /** @constant */
            type?: "agent";
            config?: components["schemas"]["AgentConfig"];
        } | {
            /** @constant */
            type?: "human_approval";
            config?: components["schemas"]["HumanApprovalConfig"];
        } | {
            /** @constant */
            type?: "join";
            config?: components["schemas"]["JoinConfig"];
        } | {
            /** @constant */
            type?: "branch";
            config?: components["schemas"]["BranchConfig"];
        } | {
            /** @constant */
            type?: "noop";
            config?: components["schemas"]["NoopConfig"];
        } | {
            /** @constant */
            type?: "echo";
            config?: components["schemas"]["EchoConfig"];
        } | {
            /** @constant */
            type?: "sleep";
            config?: components["schemas"]["SleepConfig"];
        } | {
            /** @constant */
            type?: "fail_n_times";
            config?: components["schemas"]["FailNTimesConfig"];
        } | {
            /** @constant */
            type?: "counter";
            config?: components["schemas"]["CounterConfig"];
        } | {
            /** @constant */
            type?: "effectful_echo";
            config?: components["schemas"]["EffectfulEchoConfig"];
        } | {
            /** @constant */
            type?: "blackboard_write";
            config?: components["schemas"]["BlackboardWriteConfig"];
        });
        /** @enum {string} */
        EdgeType: "normal" | "loop";
        /** @enum {string} */
        ExhaustPolicy: "proceed" | "fail";
        NoProgressPolicy: {
            step?: string;
            path?: string;
            policy?: components["schemas"]["ExhaustPolicy"];
        };
        Edge: {
            from: string;
            to: string;
            when?: string;
            type?: components["schemas"]["EdgeType"];
            condition?: string;
            max_iterations?: number;
            on_exhausted?: components["schemas"]["ExhaustPolicy"];
            no_progress?: components["schemas"]["NoProgressPolicy"];
            decision?: components["schemas"]["ApprovalDecision"];
        };
        Template: {
            steps: components["schemas"]["Step"][];
            edges?: components["schemas"]["Edge"][];
        };
        AgentDef: {
            role?: string;
            system?: string;
            model?: string;
            model_fallbacks?: components["schemas"]["ModelFallback"][];
            tools?: string[];
            max_tokens?: number;
            temperature?: number;
            output_format?: components["schemas"]["OutputFormat"];
            validation?: components["schemas"]["ValidationPolicy"];
            context?: components["schemas"]["ContextSpec"];
        };
        /** @enum {string} */
        ParamType: "string" | "number" | "boolean" | "object" | "array";
        ParamSpec: {
            type: components["schemas"]["ParamType"];
            required?: boolean;
        };
        Definition: {
            schema_version: number;
            name: string;
            description?: string;
            on_failure?: components["schemas"]["FailurePolicy"];
            max_wall_clock?: string;
            budget_usd?: number;
            on_budget_exceeded?: components["schemas"]["BudgetPolicy"];
            expansion?: components["schemas"]["ExpansionPolicy"];
            templates?: {
                [key: string]: components["schemas"]["Template"];
            };
            agents?: {
                [key: string]: components["schemas"]["AgentDef"];
            };
            params?: {
                [key: string]: components["schemas"]["ParamSpec"];
            };
            steps: components["schemas"]["Step"][];
            edges: components["schemas"]["Edge"][];
            /** @description Engine-opaque builder state: never validated or interpreted, round-tripped byte-for-byte. */
            ui?: Record<string, never>;
        };
        RunCreated: {
            name: string;
            definition_id?: string;
            steps_total: number;
        };
        StepReady: {
            step_id: string;
        };
        StepClaimed: {
            step_id: string;
            claim_id: string;
            attempt: number;
        };
        StepSucceeded: {
            step_id: string;
            attempt: number;
        };
        StepFailed: {
            step_id: string;
            attempt: number;
        };
        StepSkipped: {
            step_id: string;
        };
        StepReclaimed: {
            step_id: string;
            claim_id: string;
            attempt: number;
        };
        StepRetryScheduled: {
            step_id: string;
            attempt: number;
            class: string;
            /** Format: date-time */
            next_attempt_at: string;
        };
        StepThrottled: {
            step_id: string;
            attempt: number;
            resource: string;
            bucket: string;
            retry_after: string;
            /** Format: date-time */
            next_attempt_at: string;
        };
        StepSemanticRetry: {
            step_id: string;
            attempt: number;
            semantic_attempt: number;
            max_attempts: number;
            issue_count: number;
            /** Format: date-time */
            next_attempt_at: string;
        };
        StepDeadLettered: {
            step_id: string;
            source: string;
            class?: string;
            attempts: number;
            seq: number;
        };
        StepCancelled: {
            step_id: string;
            reason: string;
        };
        StepCollected: {
            step_id: string;
            class?: string;
            attempts: number;
        };
        StepRequeued: {
            step_id: string;
        };
        StepRevived: {
            step_id: string;
            reason: string;
        };
        RunSucceeded: Record<string, never>;
        RunFailed: Record<string, never>;
        RunResumed: Record<string, never>;
        RunParked: {
            reason: string;
        };
        RunUnparked: Record<string, never>;
        RunCancelling: {
            reason: string;
        };
        RunCancelled: Record<string, never>;
        CostUpdated: {
            step_id: string;
            attempt: number;
            entry: string;
            resource: string;
            cache_hit?: boolean;
            overhead?: boolean;
            cost_nano_usd?: number;
            saved_nano_usd?: number;
            run_spent_nano_usd: number;
            run_saved_nano_usd: number;
            budget_nano_usd?: number;
        };
        Rate: {
            input_per_mtok: number;
            output_per_mtok: number;
        };
        CostUnknownModel: {
            model: string;
            fallback: components["schemas"]["Rate"];
        };
        BudgetExceeded: {
            step_id: string;
            attempt: number;
            resource?: string;
            limit: string;
            action: string;
            spent_nano_usd?: number;
            estimate_nano_usd?: number;
            projected_nano_usd?: number;
            budget_nano_usd?: number;
            projected_tokens?: number;
            max_tokens?: number;
        };
        RunBudgetUpdated: {
            previous_nano_usd: number;
            budget_nano_usd: number;
        };
        ModelDowngraded: {
            step_id: string;
            attempt: number;
            from_model: string;
            to_model: string;
            from_resource: string;
            to_resource: string;
            trigger: string;
            limit?: string;
            threshold_fraction?: number;
            spent_nano_usd?: number;
            budget_nano_usd?: number;
            from_estimate_nano_usd?: number;
            to_estimate_nano_usd?: number;
        };
        BlackboardUpdated: {
            key: string;
            version: number;
            tags: string[];
            token_count: number;
            author_step_id?: string;
            author_attempt?: number;
        };
        ContextSourceRecord: {
            index: number;
            kind: string;
            name: string;
            ref?: string;
            status: string;
            reason?: string;
            tokens: number;
            pinned?: boolean;
        };
        ContextAssembled: {
            step_id: string;
            attempt: number;
            counter_id: string;
            sources: components["schemas"]["ContextSourceRecord"][];
            context_tokens: number;
            preflight_tokens: number;
            budget_tokens?: number;
            budget_source?: string;
            context_window?: number;
            raw_context_tokens?: number;
            raw_preflight_tokens?: number;
            revisions?: number;
            summaries?: number;
        };
        ContextRevisionActionRecord: {
            source_index: number;
            name: string;
            action: string;
            tokens_before: number;
            tokens_after: number;
        };
        ContextRevisionSummaryRecord: {
            key: string;
            version: number;
            parent_version?: number;
            model: string;
            resource: string;
            span_names?: string[];
            span_tokens: number;
            summary_tokens: number;
            cache_hit?: boolean;
            input_tokens: number;
            output_tokens: number;
        };
        ContextRevision: {
            step_id: string;
            attempt: number;
            index: number;
            strategy: string;
            n?: number;
            min_tokens?: number;
            budget: number;
            tokens_before: number;
            tokens_after: number;
            changed: boolean;
            actions?: components["schemas"]["ContextRevisionActionRecord"][];
            summaries?: components["schemas"]["ContextRevisionSummaryRecord"][];
            error?: string;
            kept?: string[];
        };
        "$defs-Step": {
            id: string;
            type: components["schemas"]["StepType"];
            config?: unknown;
            retry?: components["schemas"]["RetryPolicy"];
            timeout?: string;
            cache?: components["schemas"]["CachePolicy"];
            budget?: components["schemas"]["StepBudget"];
            validation?: components["schemas"]["ValidationPolicy"];
            blackboard?: components["schemas"]["BlackboardPolicy"];
            context?: components["schemas"]["ContextSpec"];
        };
        PlanOutput: {
            schema_version: number;
            steps: components["schemas"]["$defs-Step"][];
            edges?: components["schemas"]["Edge"][];
        };
        GraphExpanded: {
            origin_step: string;
            origin_kind: string;
            from_version: number;
            to_version: number;
            depth: number;
            delta: components["schemas"]["PlanOutput"];
            readied?: string[];
            widened?: string[];
        };
        LoopExhausted: {
            loop_source_step: string;
            loop_source_instance: string;
            body_entry: string;
            iteration: number;
            max_iterations: number;
            condition: string;
            policy: string;
            action: string;
        };
        LoopNoProgress: {
            loop_source_step: string;
            loop_source_instance: string;
            compared_step: string;
            path?: string;
            iteration: number;
            prev_instance: string;
            cur_instance: string;
            hash: string;
            policy: string;
            action: string;
        };
        GuardTripped: {
            guard: string;
            step_id?: string;
            current: number;
            cap: number;
            unit: string;
            action: string;
        };
        ApprovalRequested: {
            approval_id: string;
            step_id: string;
            attempt: number;
            title: string;
            allowed_decisions: string[];
            allow_edit?: boolean;
            /** Format: date-time */
            timeout_at?: string;
        };
        ApprovalCancelled: {
            approval_id: string;
            step_id: string;
            reason: string;
        };
        ApprovalDecided: {
            approval_id: string;
            step_id: string;
            attempt: number;
            decision: string;
            edited?: boolean;
            comment?: string;
            decided_by: string;
            source: string;
        };
        ApprovalExpired: {
            approval_id: string;
            step_id: string;
            attempt: number;
            policy: string;
            decision?: string;
            action: string;
            /** Format: date-time */
            timeout_at?: string;
        };
        ApprovalNotified: {
            approval_id: string;
            step_id: string;
            target_host: string;
            attempts: number;
            status_code: number;
        };
        ApprovalNotificationFailed: {
            approval_id: string;
            step_id: string;
            target_host: string;
            attempts: number;
            reason: string;
        };
        UUID: number[];
        Envelope: {
            schema_version: number;
            run_id: components["schemas"]["UUID"];
            seq: number;
            /** @enum {string} */
            type: "run_created" | "step_ready" | "step_claimed" | "step_succeeded" | "step_failed" | "step_skipped" | "step_reclaimed" | "step_retry_scheduled" | "step_throttled" | "step_semantic_retry_scheduled" | "step_dead_lettered" | "step_cancelled" | "step_collected" | "step_requeued" | "step_revived" | "run_succeeded" | "run_failed" | "run_resumed" | "run_parked" | "run_unparked" | "run_cancelling" | "run_cancelled" | "cost_updated" | "cost_unknown_model" | "budget_exceeded" | "run_budget_updated" | "model_downgraded" | "blackboard_updated" | "context_assembled" | "context_revision" | "graph_expanded" | "loop_exhausted" | "loop_no_progress" | "guard_tripped" | "approval_requested" | "approval_cancelled" | "approval_decided" | "approval_expired" | "approval_notified" | "approval_notification_failed";
            /** Format: date-time */
            ts: string;
            step_id?: string;
            payload: Record<string, never>;
        } & ({
            /** @constant */
            type?: "run_created";
            payload?: components["schemas"]["RunCreated"];
        } | {
            /** @constant */
            type?: "step_ready";
            payload?: components["schemas"]["StepReady"];
        } | {
            /** @constant */
            type?: "step_claimed";
            payload?: components["schemas"]["StepClaimed"];
        } | {
            /** @constant */
            type?: "step_succeeded";
            payload?: components["schemas"]["StepSucceeded"];
        } | {
            /** @constant */
            type?: "step_failed";
            payload?: components["schemas"]["StepFailed"];
        } | {
            /** @constant */
            type?: "step_skipped";
            payload?: components["schemas"]["StepSkipped"];
        } | {
            /** @constant */
            type?: "step_reclaimed";
            payload?: components["schemas"]["StepReclaimed"];
        } | {
            /** @constant */
            type?: "step_retry_scheduled";
            payload?: components["schemas"]["StepRetryScheduled"];
        } | {
            /** @constant */
            type?: "step_throttled";
            payload?: components["schemas"]["StepThrottled"];
        } | {
            /** @constant */
            type?: "step_semantic_retry_scheduled";
            payload?: components["schemas"]["StepSemanticRetry"];
        } | {
            /** @constant */
            type?: "step_dead_lettered";
            payload?: components["schemas"]["StepDeadLettered"];
        } | {
            /** @constant */
            type?: "step_cancelled";
            payload?: components["schemas"]["StepCancelled"];
        } | {
            /** @constant */
            type?: "step_collected";
            payload?: components["schemas"]["StepCollected"];
        } | {
            /** @constant */
            type?: "step_requeued";
            payload?: components["schemas"]["StepRequeued"];
        } | {
            /** @constant */
            type?: "step_revived";
            payload?: components["schemas"]["StepRevived"];
        } | {
            /** @constant */
            type?: "run_succeeded";
            payload?: components["schemas"]["RunSucceeded"];
        } | {
            /** @constant */
            type?: "run_failed";
            payload?: components["schemas"]["RunFailed"];
        } | {
            /** @constant */
            type?: "run_resumed";
            payload?: components["schemas"]["RunResumed"];
        } | {
            /** @constant */
            type?: "run_parked";
            payload?: components["schemas"]["RunParked"];
        } | {
            /** @constant */
            type?: "run_unparked";
            payload?: components["schemas"]["RunUnparked"];
        } | {
            /** @constant */
            type?: "run_cancelling";
            payload?: components["schemas"]["RunCancelling"];
        } | {
            /** @constant */
            type?: "run_cancelled";
            payload?: components["schemas"]["RunCancelled"];
        } | {
            /** @constant */
            type?: "cost_updated";
            payload?: components["schemas"]["CostUpdated"];
        } | {
            /** @constant */
            type?: "cost_unknown_model";
            payload?: components["schemas"]["CostUnknownModel"];
        } | {
            /** @constant */
            type?: "budget_exceeded";
            payload?: components["schemas"]["BudgetExceeded"];
        } | {
            /** @constant */
            type?: "run_budget_updated";
            payload?: components["schemas"]["RunBudgetUpdated"];
        } | {
            /** @constant */
            type?: "model_downgraded";
            payload?: components["schemas"]["ModelDowngraded"];
        } | {
            /** @constant */
            type?: "blackboard_updated";
            payload?: components["schemas"]["BlackboardUpdated"];
        } | {
            /** @constant */
            type?: "context_assembled";
            payload?: components["schemas"]["ContextAssembled"];
        } | {
            /** @constant */
            type?: "context_revision";
            payload?: components["schemas"]["ContextRevision"];
        } | {
            /** @constant */
            type?: "graph_expanded";
            payload?: components["schemas"]["GraphExpanded"];
        } | {
            /** @constant */
            type?: "loop_exhausted";
            payload?: components["schemas"]["LoopExhausted"];
        } | {
            /** @constant */
            type?: "loop_no_progress";
            payload?: components["schemas"]["LoopNoProgress"];
        } | {
            /** @constant */
            type?: "guard_tripped";
            payload?: components["schemas"]["GuardTripped"];
        } | {
            /** @constant */
            type?: "approval_requested";
            payload?: components["schemas"]["ApprovalRequested"];
        } | {
            /** @constant */
            type?: "approval_cancelled";
            payload?: components["schemas"]["ApprovalCancelled"];
        } | {
            /** @constant */
            type?: "approval_decided";
            payload?: components["schemas"]["ApprovalDecided"];
        } | {
            /** @constant */
            type?: "approval_expired";
            payload?: components["schemas"]["ApprovalExpired"];
        } | {
            /** @constant */
            type?: "approval_notified";
            payload?: components["schemas"]["ApprovalNotified"];
        } | {
            /** @constant */
            type?: "approval_notification_failed";
            payload?: components["schemas"]["ApprovalNotificationFailed"];
        });
    };
    responses: {
        /** @description The request is malformed (code `invalid_request`), the submitted definition failed decoding or validation (code `invalid_definition`, with `issues`), or a referenced stored definition does not exist (code `definition_not_found`, on submit-by-ref). */
        BadRequest: {
            headers: {
                [name: string]: unknown;
            };
            content: {
                "application/json": components["schemas"]["Error"];
            };
        };
        /** @description Missing or invalid credential (code `unauthorized`). Every credential failure — absent header, bad shape, unknown key, revoked, expired — collapses to this one indistinguishable response. */
        Unauthorized: {
            headers: {
                "WWW-Authenticate": components["headers"]["WWWAuthenticate"];
                [name: string]: unknown;
            };
            content: {
                /**
                 * @example {
                 *       "error": {
                 *         "code": "unauthorized",
                 *         "message": "missing or invalid credentials"
                 *       }
                 *     }
                 */
                "application/json": components["schemas"]["Error"];
            };
        };
        /** @description The credential is valid but lacks the route's scope (code `forbidden`); the message names the missing scope. */
        Forbidden: {
            headers: {
                [name: string]: unknown;
            };
            content: {
                /**
                 * @example {
                 *       "error": {
                 *         "code": "forbidden",
                 *         "message": "missing scope: submit"
                 *       }
                 *     }
                 */
                "application/json": components["schemas"]["Error"];
            };
        };
        /** @description The addressed entity does not exist — code `run_not_found`, `step_not_found`, `definition_not_found`, or `key_not_found` depending on the route. (Requests outside the route table answer `404` with code `not_found`.) */
        NotFound: {
            headers: {
                [name: string]: unknown;
            };
            content: {
                /**
                 * @example {
                 *       "error": {
                 *         "code": "run_not_found",
                 *         "message": "no run with id 018f3b1c-1111-7000-8000-000000000000"
                 *       }
                 *     }
                 */
                "application/json": components["schemas"]["Error"];
            };
        };
        /** @description The request is well-formed but the entity's current state refuses it (code `conflict`) — cancelling a finished run, unparking a running run, requeueing a step that is not dead-lettered, registering a definition name that already exists. The message names the actual state. */
        Conflict: {
            headers: {
                [name: string]: unknown;
            };
            content: {
                /**
                 * @example {
                 *       "error": {
                 *         "code": "conflict",
                 *         "message": "run 018f3b1c-1111-7000-8000-000000000000 is succeeded, not running"
                 *       }
                 *     }
                 */
                "application/json": components["schemas"]["Error"];
            };
        };
        /** @description The `Idempotency-Key` was seen before with a different payload (code `idempotency_key_conflict`); the replay is refused instead of returning the original run. */
        IdempotencyConflict: {
            headers: {
                [name: string]: unknown;
            };
            content: {
                /**
                 * @example {
                 *       "error": {
                 *         "code": "idempotency_key_conflict",
                 *         "message": "Idempotency-Key was already used by run 018f3b1c-1111-7000-8000-000000000000 with a different payload"
                 *       }
                 *     }
                 */
                "application/json": components["schemas"]["Error"];
            };
        };
        /** @description Over a rate limit (code `rate_limited`). `Retry-After` comes from whichever bucket denied (per-key class bucket or the global safety bucket); the `X-RateLimit-*` headers always describe the caller's class bucket. */
        RateLimited: {
            headers: {
                "Retry-After": components["headers"]["RetryAfter"];
                "X-RateLimit-Limit": components["headers"]["XRateLimitLimit"];
                "X-RateLimit-Remaining": components["headers"]["XRateLimitRemaining"];
                "X-RateLimit-Reset": components["headers"]["XRateLimitReset"];
                [name: string]: unknown;
            };
            content: {
                /**
                 * @example {
                 *       "error": {
                 *         "code": "rate_limited",
                 *         "message": "rate limit exceeded"
                 *       }
                 *     }
                 */
                "application/json": components["schemas"]["Error"];
            };
        };
        /** @description The request was fine, the server was not (code `internal`). */
        Internal: {
            headers: {
                [name: string]: unknown;
            };
            content: {
                /**
                 * @example {
                 *       "error": {
                 *         "code": "internal",
                 *         "message": "creating run failed"
                 *       }
                 *     }
                 */
                "application/json": components["schemas"]["Error"];
            };
        };
        /** @description The response-cache ops surface is not enabled on this API (code `cache_unavailable`) — caching is disabled or the API was built without the cache store. The cache is an opt-in extra, never a boot dependency (ADR-002); enable it to use bust/stats. */
        CacheUnavailable: {
            headers: {
                [name: string]: unknown;
            };
            content: {
                /**
                 * @example {
                 *       "error": {
                 *         "code": "cache_unavailable",
                 *         "message": "response cache is not enabled on this API"
                 *       }
                 *     }
                 */
                "application/json": components["schemas"]["Error"];
            };
        };
        /** @description Event streaming is not enabled on this API instance (code `stream_unavailable`, ticket 16.3). The durable feed stays available via `GET /v1/runs/{id}` and the events backfill; only the live WebSocket is off. Configure a WS ticket secret to enable it. */
        StreamUnavailable: {
            headers: {
                [name: string]: unknown;
            };
            content: {
                /**
                 * @example {
                 *       "error": {
                 *         "code": "stream_unavailable",
                 *         "message": "event streaming is not enabled on this server"
                 *       }
                 *     }
                 */
                "application/json": components["schemas"]["Error"];
            };
        };
        /** @description The decision is not permitted by the gate (code `approval_decision_invalid`, ADR-017, ticket 15.3) — a decision outside the allowed set, an edit where none is permitted, an edit on a non-approve decision, or an edited payload that violates the edit schema. The `issues` carry the schema violations (RFC 6901 pointers). */
        DecisionInvalid: {
            headers: {
                [name: string]: unknown;
            };
            content: {
                /**
                 * @example {
                 *       "error": {
                 *         "code": "approval_decision_invalid",
                 *         "message": "edited payload does not satisfy the edit schema",
                 *         "issues": [
                 *           {
                 *             "severity": "error",
                 *             "path": "/text",
                 *             "msg": "got number, want string"
                 *           }
                 *         ]
                 *       }
                 *     }
                 */
                "application/json": components["schemas"]["Error"];
            };
        };
    };
    parameters: {
        /** @description The run's UUID. */
        RunID: string;
        /** @description The step's id within the run's graph (the definition's step id). */
        StepID: string;
        /** @description The definition name (the spec's `name` field). */
        DefinitionName: string;
        /** @description Client-chosen idempotency token, at most 200 bytes (longer is a `400`). Same key + same payload replays the original run (`200`, `reused: true`); same key + different payload is a `409` with code `idempotency_key_conflict`. Tokens are global across API keys. */
        IdempotencyKey: string;
        /** @description Optimistic-concurrency precondition on a definition-version append (ticket 17.6): the version number the client opened the definition at, as a positive integer. If the name's latest version has advanced past it, the append is refused with `409 version_conflict`. Absent skips the check. */
        IfMatchVersion: number;
        /** @description Page size; an integer in [1, 200]. */
        PageLimit: number;
        /** @description Opaque keyset cursor from the previous page's `next_cursor`; pass it back verbatim. A cursor from one list endpoint is not valid on another. Garbage is a `400`. */
        PageCursor: string;
    };
    requestBodies: never;
    headers: {
        /** @description Whole seconds (rounded up) until the denying bucket refills enough to admit this request. */
        RetryAfter: number;
        /** @description The caller's class-bucket capacity. */
        XRateLimitLimit: number;
        /** @description Whole tokens remaining in the caller's class bucket. */
        XRateLimitRemaining: number;
        /** @description Whole seconds until the caller's class bucket is full again. */
        XRateLimitReset: number;
        /** @description Always `Bearer` — the API's only challenge scheme. */
        WWWAuthenticate: string;
    };
    pathItems: never;
}
export type $defs = Record<string, never>;
export interface operations {
    getHealth: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description The API is serving and Postgres answers. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    /**
                     * @example {
                     *       "status": "ok"
                     *     }
                     */
                    "application/json": components["schemas"]["HealthStatus"];
                };
            };
            /** @description Postgres did not answer the ping. */
            503: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    /**
                     * @example {
                     *       "status": "degraded"
                     *     }
                     */
                    "application/json": components["schemas"]["HealthStatus"];
                };
            };
        };
    };
    listRuns: {
        parameters: {
            query?: {
                /** @description Only runs in this status. */
                status?: components["schemas"]["RunStatus"];
                /** @description Only runs instantiated from this stored definition. */
                definition_id?: string;
                /** @description Only runs created strictly after this instant (RFC 3339). */
                created_after?: string;
                /** @description Only runs created strictly before this instant (RFC 3339). */
                created_before?: string;
                /** @description Page size; an integer in [1, 200]. */
                limit?: components["parameters"]["PageLimit"];
                /** @description Opaque keyset cursor from the previous page's `next_cursor`; pass it back verbatim. A cursor from one list endpoint is not valid on another. Garbage is a `400`. */
                cursor?: components["parameters"]["PageCursor"];
            };
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description One page of runs. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ListRunsResponse"];
                };
            };
            400: components["responses"]["BadRequest"];
            401: components["responses"]["Unauthorized"];
            403: components["responses"]["Forbidden"];
            429: components["responses"]["RateLimited"];
            500: components["responses"]["Internal"];
        };
    };
    submitRun: {
        parameters: {
            query?: never;
            header?: {
                /** @description Client-chosen idempotency token, at most 200 bytes (longer is a `400`). Same key + same payload replays the original run (`200`, `reused: true`); same key + different payload is a `409` with code `idempotency_key_conflict`. Tokens are global across API keys. */
                "Idempotency-Key"?: components["parameters"]["IdempotencyKey"];
            };
            path?: never;
            cookie?: never;
        };
        /** @description The definition (inline or by ref) and opaque run parameters. */
        requestBody: {
            content: {
                "application/json": components["schemas"]["SubmitRunRequest"];
            };
        };
        responses: {
            /** @description The `Idempotency-Key` matched an earlier submission with the same payload; this is the original run, replayed. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    /**
                     * @example {
                     *       "run_id": "018f3b1c-1111-7000-8000-000000000000",
                     *       "status": "succeeded",
                     *       "entry_steps": [
                     *         "greet"
                     *       ],
                     *       "reused": true
                     *     }
                     */
                    "application/json": components["schemas"]["SubmitRunResponse"];
                };
            };
            /** @description The run was created. */
            201: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    /**
                     * @example {
                     *       "run_id": "018f3b1c-1111-7000-8000-000000000000",
                     *       "status": "running",
                     *       "entry_steps": [
                     *         "greet"
                     *       ]
                     *     }
                     */
                    "application/json": components["schemas"]["SubmitRunResponse"];
                };
            };
            400: components["responses"]["BadRequest"];
            401: components["responses"]["Unauthorized"];
            403: components["responses"]["Forbidden"];
            409: components["responses"]["IdempotencyConflict"];
            429: components["responses"]["RateLimited"];
            500: components["responses"]["Internal"];
        };
    };
    getRun: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** @description The run's UUID. */
                run_id: components["parameters"]["RunID"];
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description The run in full. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["RunResponse"];
                };
            };
            400: components["responses"]["BadRequest"];
            401: components["responses"]["Unauthorized"];
            403: components["responses"]["Forbidden"];
            404: components["responses"]["NotFound"];
            429: components["responses"]["RateLimited"];
            500: components["responses"]["Internal"];
        };
    };
    getRunCost: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** @description The run's UUID. */
                run_id: components["parameters"]["RunID"];
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description The run's cost breakdown. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["RunCostResponse"];
                };
            };
            400: components["responses"]["BadRequest"];
            401: components["responses"]["Unauthorized"];
            403: components["responses"]["Forbidden"];
            404: components["responses"]["NotFound"];
            429: components["responses"]["RateLimited"];
            500: components["responses"]["Internal"];
        };
    };
    cancelRun: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** @description The run's UUID. */
                run_id: components["parameters"]["RunID"];
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description Cancellation requested (or finalized, when nothing was in flight). */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["CancelRunResponse"];
                };
            };
            400: components["responses"]["BadRequest"];
            401: components["responses"]["Unauthorized"];
            403: components["responses"]["Forbidden"];
            404: components["responses"]["NotFound"];
            409: components["responses"]["Conflict"];
            429: components["responses"]["RateLimited"];
            500: components["responses"]["Internal"];
        };
    };
    parkRun: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** @description The run's UUID. */
                run_id: components["parameters"]["RunID"];
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description The run is parked. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ParkRunResponse"];
                };
            };
            400: components["responses"]["BadRequest"];
            401: components["responses"]["Unauthorized"];
            403: components["responses"]["Forbidden"];
            404: components["responses"]["NotFound"];
            409: components["responses"]["Conflict"];
            429: components["responses"]["RateLimited"];
            500: components["responses"]["Internal"];
        };
    };
    unparkRun: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** @description The run's UUID. */
                run_id: components["parameters"]["RunID"];
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description The run is running again. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["UnparkRunResponse"];
                };
            };
            400: components["responses"]["BadRequest"];
            401: components["responses"]["Unauthorized"];
            403: components["responses"]["Forbidden"];
            404: components["responses"]["NotFound"];
            409: components["responses"]["Conflict"];
            429: components["responses"]["RateLimited"];
            500: components["responses"]["Internal"];
        };
    };
    setRunBudget: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** @description The run's UUID. */
                run_id: components["parameters"]["RunID"];
            };
            cookie?: never;
        };
        /** @description The new spend budget in US dollars. */
        requestBody: {
            content: {
                "application/json": components["schemas"]["SetBudgetRequest"];
            };
        };
        responses: {
            /** @description The run with its updated cost/budget summary. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["SetBudgetResponse"];
                };
            };
            400: components["responses"]["BadRequest"];
            401: components["responses"]["Unauthorized"];
            403: components["responses"]["Forbidden"];
            404: components["responses"]["NotFound"];
            409: components["responses"]["Conflict"];
            429: components["responses"]["RateLimited"];
            500: components["responses"]["Internal"];
        };
    };
    requeueStep: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** @description The run's UUID. */
                run_id: components["parameters"]["RunID"];
                /** @description The step's id within the run's graph (the definition's step id). */
                step_id: components["parameters"]["StepID"];
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description The step is back in ready and dispatched. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["RequeueStepResponse"];
                };
            };
            400: components["responses"]["BadRequest"];
            401: components["responses"]["Unauthorized"];
            403: components["responses"]["Forbidden"];
            404: components["responses"]["NotFound"];
            409: components["responses"]["Conflict"];
            429: components["responses"]["RateLimited"];
            500: components["responses"]["Internal"];
        };
    };
    getStepLogs: {
        parameters: {
            query?: {
                /** @description The 1-based attempt whose logs to read; default the step's latest. */
                attempt?: number;
                /** @description Minimum severity; lines below it are filtered out. */
                level?: components["schemas"]["LogLevel"];
                /** @description Page size; an integer in [1, 1000]. */
                limit?: number;
                /** @description Opaque keyset cursor from the previous page's `next_cursor`; pass it back verbatim. A cursor from one list endpoint is not valid on another. Garbage is a `400`. */
                cursor?: components["parameters"]["PageCursor"];
            };
            header?: never;
            path: {
                /** @description The run's UUID. */
                run_id: components["parameters"]["RunID"];
                /** @description The step's id within the run's graph (the definition's step id). */
                step_id: components["parameters"]["StepID"];
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description One page of log lines. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["StepLogsResponse"];
                };
            };
            400: components["responses"]["BadRequest"];
            401: components["responses"]["Unauthorized"];
            403: components["responses"]["Forbidden"];
            404: components["responses"]["NotFound"];
            429: components["responses"]["RateLimited"];
            500: components["responses"]["Internal"];
        };
    };
    getRunBlackboard: {
        parameters: {
            query?: {
                /** @description Restrict to these keys (repeatable). */
                key?: string[];
                /** @description Keep only entries carrying every listed tag (repeatable; AND). */
                tag?: string[];
                /** @description When true, return every version, not just each key's head. */
                history?: boolean;
                /** @description Maximum entries per page; an integer in [1, 1000]. */
                limit?: number;
                /** @description Opaque keyset cursor from the previous page's `next_cursor`; pass it back verbatim. A cursor from one list endpoint is not valid on another. Garbage is a `400`. */
                cursor?: components["parameters"]["PageCursor"];
            };
            header?: never;
            path: {
                /** @description The run's UUID. */
                run_id: components["parameters"]["RunID"];
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description One page of blackboard entries. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["BlackboardResponse"];
                };
            };
            400: components["responses"]["BadRequest"];
            401: components["responses"]["Unauthorized"];
            403: components["responses"]["Forbidden"];
            404: components["responses"]["NotFound"];
            429: components["responses"]["RateLimited"];
            500: components["responses"]["Internal"];
        };
    };
    getRunGraph: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** @description The run's UUID. */
                run_id: components["parameters"]["RunID"];
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description The run's versioned graph. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["RunGraphResponse"];
                };
            };
            400: components["responses"]["BadRequest"];
            401: components["responses"]["Unauthorized"];
            403: components["responses"]["Forbidden"];
            404: components["responses"]["NotFound"];
            429: components["responses"]["RateLimited"];
            500: components["responses"]["Internal"];
        };
    };
    mintRunWSTicket: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** @description The run's UUID. */
                run_id: components["parameters"]["RunID"];
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description A signed ticket for the run WebSocket. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["WSTicketResponse"];
                };
            };
            400: components["responses"]["BadRequest"];
            401: components["responses"]["Unauthorized"];
            403: components["responses"]["Forbidden"];
            404: components["responses"]["NotFound"];
            429: components["responses"]["RateLimited"];
            500: components["responses"]["Internal"];
            503: components["responses"]["StreamUnavailable"];
        };
    };
    streamRunEvents: {
        parameters: {
            query?: {
                /** @description A ticket from POST .../ws-ticket (the browser auth path). */
                ticket?: string;
                /** @description Resume cursor — the highest event seq the client has seen. */
                last_seq?: number;
            };
            header?: never;
            path: {
                /** @description The run's UUID. */
                run_id: components["parameters"]["RunID"];
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /**
             * @description The connection is upgraded to a WebSocket. Frames follow the
             *     snapshot → backfill → live-tail protocol described above. The
             *     content schema below documents the frame shapes (a text frame is one
             *     of these); it is not a conventional HTTP body.
             */
            101: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["WSSnapshotFrame"] | components["schemas"]["WSEventFrame"] | components["schemas"]["WSCaughtUpFrame"] | components["schemas"]["WSErrorFrame"];
                };
            };
            400: components["responses"]["BadRequest"];
            401: components["responses"]["Unauthorized"];
            403: components["responses"]["Forbidden"];
            404: components["responses"]["NotFound"];
            429: components["responses"]["RateLimited"];
            500: components["responses"]["Internal"];
            503: components["responses"]["StreamUnavailable"];
        };
    };
    mintFirehoseWSTicket: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description A signed ticket for the firehose WebSocket. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["WSTicketResponse"];
                };
            };
            400: components["responses"]["BadRequest"];
            401: components["responses"]["Unauthorized"];
            403: components["responses"]["Forbidden"];
            429: components["responses"]["RateLimited"];
            500: components["responses"]["Internal"];
            503: components["responses"]["StreamUnavailable"];
        };
    };
    streamEvents: {
        parameters: {
            query?: {
                /** @description A ticket from POST /v1/events/ws-ticket (the browser auth path). */
                ticket?: string;
            };
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /**
             * @description The connection is upgraded to a WebSocket. Frames follow the
             *     subscribe → backfill → live-tail protocol described above. The
             *     content schema below documents the frame shapes (a text frame is one
             *     of these); it is not a conventional HTTP body.
             */
            101: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["WSEventFrame"] | components["schemas"]["WSSubscribedFrame"] | components["schemas"]["WSUnsubscribedFrame"] | components["schemas"]["WSFirehoseCaughtUpFrame"] | components["schemas"]["WSErrorFrame"];
                };
            };
            400: components["responses"]["BadRequest"];
            401: components["responses"]["Unauthorized"];
            403: components["responses"]["Forbidden"];
            429: components["responses"]["RateLimited"];
            500: components["responses"]["Internal"];
            503: components["responses"]["StreamUnavailable"];
        };
    };
    listDefinitions: {
        parameters: {
            query?: {
                /** @description Page size; an integer in [1, 200]. */
                limit?: components["parameters"]["PageLimit"];
                /** @description Opaque keyset cursor from the previous page's `next_cursor`; pass it back verbatim. A cursor from one list endpoint is not valid on another. Garbage is a `400`. */
                cursor?: components["parameters"]["PageCursor"];
            };
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description One page of definitions. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ListDefinitionsResponse"];
                };
            };
            400: components["responses"]["BadRequest"];
            401: components["responses"]["Unauthorized"];
            403: components["responses"]["Forbidden"];
            429: components["responses"]["RateLimited"];
            500: components["responses"]["Internal"];
        };
    };
    createDefinition: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /** @description The definition document to register. */
        requestBody: {
            content: {
                "application/json": components["schemas"]["CreateDefinitionRequest"];
            };
        };
        responses: {
            /** @description The definition is stored at version 1. */
            201: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["DefinitionResponse"];
                };
            };
            400: components["responses"]["BadRequest"];
            401: components["responses"]["Unauthorized"];
            403: components["responses"]["Forbidden"];
            409: components["responses"]["Conflict"];
            429: components["responses"]["RateLimited"];
            500: components["responses"]["Internal"];
        };
    };
    getDefinition: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** @description The stored definition's UUID. */
                definition_id: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description The stored definition, spec included. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["DefinitionResponse"];
                };
            };
            400: components["responses"]["BadRequest"];
            401: components["responses"]["Unauthorized"];
            403: components["responses"]["Forbidden"];
            404: components["responses"]["NotFound"];
            429: components["responses"]["RateLimited"];
            500: components["responses"]["Internal"];
        };
    };
    listDefinitionVersions: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** @description The definition name (the spec's `name` field). */
                name: components["parameters"]["DefinitionName"];
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description All versions of the name. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["DefinitionVersionsResponse"];
                };
            };
            401: components["responses"]["Unauthorized"];
            403: components["responses"]["Forbidden"];
            404: components["responses"]["NotFound"];
            429: components["responses"]["RateLimited"];
            500: components["responses"]["Internal"];
        };
    };
    createDefinitionVersion: {
        parameters: {
            query?: never;
            header?: {
                /** @description Optimistic-concurrency precondition on a definition-version append (ticket 17.6): the version number the client opened the definition at, as a positive integer. If the name's latest version has advanced past it, the append is refused with `409 version_conflict`. Absent skips the check. */
                "If-Match"?: components["parameters"]["IfMatchVersion"];
            };
            path: {
                /** @description The definition name (the spec's `name` field). */
                name: components["parameters"]["DefinitionName"];
            };
            cookie?: never;
        };
        /** @description The new version's definition document; its `name` must match the path. */
        requestBody: {
            content: {
                "application/json": components["schemas"]["CreateDefinitionRequest"];
            };
        };
        responses: {
            /** @description The new version is stored. */
            201: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["DefinitionResponse"];
                };
            };
            400: components["responses"]["BadRequest"];
            401: components["responses"]["Unauthorized"];
            403: components["responses"]["Forbidden"];
            404: components["responses"]["NotFound"];
            409: components["responses"]["Conflict"];
            429: components["responses"]["RateLimited"];
            500: components["responses"]["Internal"];
        };
    };
    listApprovals: {
        parameters: {
            query?: {
                /** @description Filter to one approval status (default = all). */
                status?: "pending" | "approved" | "rejected" | "expired" | "cancelled";
                /** @description Filter to one run's approvals. */
                run_id?: string;
                /** @description Page size (default 50, max 200). */
                limit?: number;
                /** @description Opaque cursor from a previous page's next_cursor. */
                cursor?: string;
            };
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description One page of approvals, oldest-first. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ApprovalListResponse"];
                };
            };
            400: components["responses"]["BadRequest"];
            401: components["responses"]["Unauthorized"];
            403: components["responses"]["Forbidden"];
            429: components["responses"]["RateLimited"];
            500: components["responses"]["Internal"];
        };
    };
    decideApproval: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** @description The approval's UUID. */
                approvalID: string;
            };
            cookie?: never;
        };
        /** @description The decision, an optional edited payload, and an optional comment. */
        requestBody: {
            content: {
                "application/json": components["schemas"]["DecideApprovalRequest"];
            };
        };
        responses: {
            /** @description The decision was recorded and the step settled. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["DecideApprovalResponse"];
                };
            };
            400: components["responses"]["BadRequest"];
            401: components["responses"]["Unauthorized"];
            403: components["responses"]["Forbidden"];
            404: components["responses"]["NotFound"];
            409: components["responses"]["Conflict"];
            422: components["responses"]["DecisionInvalid"];
            429: components["responses"]["RateLimited"];
            500: components["responses"]["Internal"];
        };
    };
    listPlugins: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description The full catalog — one page, no pagination. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ListPluginsResponse"];
                };
            };
            401: components["responses"]["Unauthorized"];
            403: components["responses"]["Forbidden"];
            429: components["responses"]["RateLimited"];
            500: components["responses"]["Internal"];
        };
    };
    bustCache: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /** @description The namespace selector; omit the body to bust everything. */
        requestBody?: {
            content: {
                /**
                 * @example {
                 *       "plugin_kind": "model_provider",
                 *       "plugin_name": "mock"
                 *     }
                 */
                "application/json": components["schemas"]["CacheBustRequest"];
            };
        };
        responses: {
            /** @description The bust completed; `deleted` is the entry count removed. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["CacheBustResponse"];
                };
            };
            400: components["responses"]["BadRequest"];
            401: components["responses"]["Unauthorized"];
            403: components["responses"]["Forbidden"];
            429: components["responses"]["RateLimited"];
            500: components["responses"]["Internal"];
            503: components["responses"]["CacheUnavailable"];
        };
    };
    cacheStats: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description Per-plugin counters, sorted by kind then name. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["CacheStatsResponse"];
                };
            };
            401: components["responses"]["Unauthorized"];
            403: components["responses"]["Forbidden"];
            429: components["responses"]["RateLimited"];
            500: components["responses"]["Internal"];
            503: components["responses"]["CacheUnavailable"];
        };
    };
    listKeys: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description All keys. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ListKeysResponse"];
                };
            };
            401: components["responses"]["Unauthorized"];
            403: components["responses"]["Forbidden"];
            429: components["responses"]["RateLimited"];
            500: components["responses"]["Internal"];
        };
    };
    createKey: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /** @description The key's label, scope set, and optional TTL. */
        requestBody: {
            content: {
                /**
                 * @example {
                 *       "name": "ci-submitter",
                 *       "scopes": [
                 *         "submit",
                 *         "read"
                 *       ],
                 *       "ttl": "720h"
                 *     }
                 */
                "application/json": components["schemas"]["CreateKeyRequest"];
            };
        };
        responses: {
            /** @description The key, plaintext included — shown exactly once. */
            201: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["CreateKeyResponse"];
                };
            };
            400: components["responses"]["BadRequest"];
            401: components["responses"]["Unauthorized"];
            403: components["responses"]["Forbidden"];
            429: components["responses"]["RateLimited"];
            500: components["responses"]["Internal"];
        };
    };
    revokeKey: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** @description The key's UUID (not its prefix). */
                key_id: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description The key is revoked (or already was). */
            204: {
                headers: {
                    [name: string]: unknown;
                };
                content?: never;
            };
            400: components["responses"]["BadRequest"];
            401: components["responses"]["Unauthorized"];
            403: components["responses"]["Forbidden"];
            404: components["responses"]["NotFound"];
            429: components["responses"]["RateLimited"];
            500: components["responses"]["Internal"];
        };
    };
}
