# ADR-007: Authentication, authorization & API rate limiting

- **Status:** Accepted
- **Date:** 2026-08-12
- **Ticket:** ROADMAP.md ticket 6.1

## Context

The 4.6 ingest API is deliberately dev-mode: every route is anonymous,
and compose publishes it on `0.0.0.0`. M6 turns it into the real product
surface, which forces three coupled decisions now:

- **Authentication.** Who may call `/v1` at all, and how a credential is
  issued, stored, presented, revoked, and expired. The store must survive
  a database leak without leaking the credentials themselves, and the
  hot path (every request) must stay a single indexed read plus a cheap
  compare.
- **Authorization.** The API's operations have very different blast
  radii — reading a run's status tree, submitting a run that spends real
  LLM dollars, approving a parked human-in-the-loop step (M15), and
  minting new credentials are not one privilege. The scope model must be
  decided before 6.2 hard-codes enforcement and before 6.5 multiplies
  the route count.
- **Rate limiting.** Per-client API limits are an explicit M6 requirement
  (6.3/6.4), and M9 needs fleet-wide limits for LLM providers. Building
  two limiters would be a bug factory; the shared design must be fixed
  before 6.3 writes the library.

Constraints from the existing system: the API deployable talks only to
Postgres (ADR-002 — no Redis client until 6.4's limiter forces one);
`ctl` is a pure HTTP client by the 4.6 design decision, so key
management must be API routes, not a CLI that reaches into the store;
timestamps are app-written from the injected clock (project invariant);
and "no secrets in code, fixtures, logs, traces, or state" is
non-negotiable — the plaintext credential may exist only in the one HTTP
response that mints it.

There is also a bootstrap circularity to break: if key management
requires an admin credential and credentials come from key management,
a fresh deployment can never mint its first key.

## Decision

### Credential: bearer API keys, not JWT

We will authenticate clients with **opaque bearer API keys**. v1's
clients are services and operators (`ctl`, CI, agent backends), not
humans in browsers: keys give service-to-service simplicity (no IdP, no
issuer/audience/refresh machinery, no clock-skew handling) and **instant
revocation** — a key is a database row, so revoking it is one UPDATE
visible on the very next request, where revoking a JWT before expiry
requires the denylist infrastructure that erases JWT's statelessness
advantage. JWT/OIDC (human SSO for the M17+ dashboard) is backlog, not
v1.

### Key format

```
sk_<43 chars of base64url>        e.g. sk_Jmn0…  (46 chars total)
```

- 32 bytes from `crypto/rand`, encoded base64url without padding
  (43 chars), prefixed `sk_`. Generation happens server-side in the
  create endpoint; the plaintext is returned once in that response and
  never again.
- The **lookup prefix** is the first 11 characters (`sk_` + 8 random
  chars, ~2⁴⁸ space). It is stored in clear, UNIQUE-indexed, and is the
  only key material that may appear in logs and listings — enough for an
  operator to correlate a leaked key with a row, useless to an attacker.
- A prefix collision at create time (unique violation, ~impossible at
  realistic key counts) is handled by regenerating, bounded retries.

### Storage: SHA-256 hash + prefix

`api_keys` stores the **hex SHA-256 of the full plaintext** (UNIQUE) and
the lookup prefix — never the plaintext. Verification is: parse bearer →
fetch the row by prefix (one indexed read) → constant-time compare of
hashes → revocation/expiry checks.

A deliberate divergence from password storage: **a fast hash, not
bcrypt/argon2/scrypt.** Those exist to slow brute force on low-entropy
human passwords; this secret is 256 random bits, so inverting SHA-256 of
it is infeasible, and a KDF would add tens of milliseconds of CPU to
*every authenticated request*. HMAC with a server pepper was likewise
rejected: it protects only against an attacker who has the table but not
the server config, at the cost of making the pepper an
unrotatable-in-practice second secret.

### Scopes

Four scopes, stored as a set (`TEXT[]`) on the key:

| Scope | Grants |
|---|---|
| `read` | Read-only inspection: runs, definitions, attempts, events. |
| `submit` | Creating and steering work: submit runs, cancel, park/unpark, requeue dead-lettered steps, create definitions (6.5). |
| `approve` | **Reserved for M15**: resolving human-approval steps. Assignable now so approval bots can be provisioned ahead of enforcement. |
| `admin` | Everything, including key management. `admin` **implies all other scopes** — route checks test "has scope X or admin", so least-privilege keys simply never request admin. |

The route→scope table (6.2 enforces it; 6.5/M15 rows are assigned now so
those tickets implement against a decided model):

| Route | Scope | Since |
|---|---|---|
| `GET /healthz` (later `/readyz`, `/metrics`) | exempt | 4.6 |
| `POST /v1/runs` | `submit` | 4.6 |
| `GET /v1/runs`, `GET /v1/runs/{id}` | `read` | 4.6/6.5 |
| `POST /v1/keys`, `GET /v1/keys`, `DELETE /v1/keys/{id}` | `admin` | 6.1 |
| `POST /v1/runs/{id}/cancel`, `…/park`, `…/unpark`, `…/steps/{sid}/requeue` | `submit` | 6.5 |
| `POST /v1/definitions`, new-version | `submit` | 6.5 |
| `GET /v1/definitions*` | `read` | 6.5 |
| M15 approval resolution | `approve` | M15 |

### Lifecycle

- **Create** (`POST /v1/keys`, admin): name, scope set, optional TTL.
  Expiry is requested as a duration and resolved to an absolute
  `expires_at` against the **server's injected clock** — clients don't
  supply timestamps, so a client with a skewed clock can't mint a
  key that outlives its intent.
- **Revoke** (`DELETE /v1/keys/{id}`, admin): soft — stamps
  `revoked_at`, keeps the row for audit. Idempotent: revoking a revoked
  key is a no-op success. There is no hard delete and no un-revoke.
- **Expire**: `expires_at` is checked at verification time; an expired
  key is indistinguishable from a revoked one to the caller. No
  background sweeper — expired rows are inert and visible in listings.
- **No rotation primitive in v1**: rotation is create-new + revoke-old.

### Admin bootstrap: env-provided root key

The API reads an optional **root key** from `AGENTLOOM_API_ROOT_KEY`.
It must have the standard `sk_` shape, is hashed at boot (only the hash
is retained in memory), never touches the database, and authenticates as
an implicit `admin` credential logged as `key_id="root"`. This breaks
the bootstrap circularity: set the root key, `ctl keys create` a real
admin key, then unset the root key and restart. When the variable is
unset, the root path simply doesn't exist. The root key is
process-config, deliberately not a row: it cannot be revoked by the
thing it bootstraps.

### 401 vs 403 (enforced fully in 6.2)

- **401 `unauthorized`**: no `Authorization` header, malformed bearer,
  unknown prefix, hash mismatch, revoked, expired. All credential
  failures collapse to one indistinguishable answer — the response never
  reveals whether a prefix exists or a key "almost worked".
- **403 `forbidden`**: the key is valid but lacks the route's scope. The
  machine-readable body names the missing scope (the caller already
  proved key possession, so this leaks nothing).
- Auth outcomes are logged with `key_id` and prefix, never the
  credential or its hash. `/healthz` (and future `/readyz`, `/metrics`)
  stay exempt so probes and scrapers need no secret.

In 6.1 only the `/v1/keys` subtree is gated (the bootstrap makes no
sense otherwise); 6.2 generalizes the same verifier to every `/v1`
route per the table above, and also owns the two parked post-M4-audit
items (compose publishes the dev API on `0.0.0.0`; the `counter` test
executor writes to arbitrary submitted paths).

### As built (ticket 6.2)

Enforcement landed exactly per the table: `requireScope` is mounted
per-route (`submit` on `POST /v1/runs`, `read` on `GET /v1/runs/{id}`,
`admin` on the `/v1/keys` subtree), and a walk-based route-coverage test
fails any future `/v1` route that is not in the route→scope table — a
new endpoint cannot ship anonymous by omission. Deliberate edge: 404/405
responses for unmatched paths answer anonymously (chi's fallback
handlers run outside the `/v1` middleware tree); route existence is
public knowledge — the spec — so this leaks nothing.

`requireScope` now also stamps the authenticated identity (key id +
scopes) into the request context — the hook 6.4's per-key rate limiting
reads — and reports `key_id` back up to the per-request log line, so
every authenticated request is attributable without ever logging key
material. A non-`Bearer` authorization scheme is one more uniform 401.

The two parked post-M4-audit items were resolved here:

- **Compose bind**: the api port mapping defaults to `127.0.0.1`
  (`AGENTLOOM_API_BIND` overrides). Auth is now the real gate, but a dev
  stack carrying a bootstrap credential still shouldn't listen on all
  interfaces by default — defense in depth, one line.
- **Test executors**: the two with filesystem side effects (`counter`,
  `effectful_echo` — both append to a submitter-chosen path) moved out
  of the production default registry. `exec.CoreBuiltins()` is what
  `cmd/worker` registers unless `AGENTLOOM_WORKER_TEST_EXECUTORS=true`
  opts the full `exec.Builtins()` set in (binary default **false**;
  docker-compose.yml sets it true — the compose stack is the dev/demo
  environment and the crash demo's fixtures need `counter`). A submitted
  step of an unregistered type still passes validation (the dag catalog
  is definition shape, not fleet capability) and then dead-letters
  permanent at claim time via the registry miss — the established 5.4
  path, visible in the DLQ rather than silently dropped.

### Rate limiting (design here, built in 6.3/6.4, reused in M9)

One generic **token-bucket library** (`internal/ratelimit`): an atomic
Redis Lua script per acquire — capacity, refill rate, variable cost —
returning `allowed`, `remaining`, `retry_after`. Atomicity in Lua is the
point: check-then-decrement as two round trips over-grants under
concurrency.

Two tenants, same library:

- **6.4, per-client API limits**: buckets keyed `ratelimit:api:<key_id>:
  <route_class>`, with route classes (submits stricter than reads,
  admin/key-management strictest) configured via env/config, plus one
  global safety bucket protecting the API even when every individual key
  is under its limit. Over-limit → **429** with `Retry-After` and
  `X-RateLimit-Limit` / `-Remaining` / `-Reset` headers.
- **M9, fleet-wide LLM limits**: buckets keyed by provider resource
  (e.g. tokens/minute per model), acquired by workers before provider
  calls; a throttled acquire becomes a delayed requeue, not a failure.

The two differ only in key naming and cost semantics — which is why the
library takes both as parameters and knows nothing about HTTP or LLMs.

### As built (ticket 6.3: the library)

`internal/ratelimit`: `New(redis.Cmdable)` wraps an existing client
(mirroring `queue.New`); `Acquire(ctx, Bucket{Key, Capacity,
RefillPerSec}, cost)` runs one atomic Lua script and returns `Result{
Allowed, Remaining, RetryAfter}`. Decisions made here:

- **Clock: Redis `TIME` inside the script**, not a caller-injected now —
  a deliberate divergence from the queue library's convention. Acquirers
  are many API replicas and workers with independently skewed clocks,
  and a bucket is *shared* state: a skewed caller passing its own now
  could mint or destroy tokens for everyone. One Redis = one clock (safe
  under Redis 7's effect replication). The injectable-time invariant is
  honored through a test-only seam: the script takes an optional ARGV
  time override, reachable only via an unexported method exposed to
  tests through `export_test.go` — production code cannot pass a time.
  Negative elapsed time (a clock step backwards across failover) is
  clamped to zero rather than minting tokens.
- **State: one hash per bucket key (`tokens`, `ts` in epoch µs), absent
  key = full bucket.** Every acquire re-arms the key's TTL to
  time-to-full plus a safety margin (a late expiry only fires once the
  bucket would have refilled anyway, so expiry can never over-grant);
  idle buckets self-clean, which is what keeps 6.4's per-key cardinality
  bounded. Rate-zero buckets (fixed quotas) `PERSIST` instead — expiring
  their state would silently re-arm the quota. The balance is serialized
  with `%.17g` so the float64 round-trips exactly (`tostring`'s `%.14g`
  would corrupt it); the refill-math property test compares the script
  against a pure-Go model for *exact* equality, on the strength of that
  round-trip.
- **Config lives with the caller, not in Redis**: capacity and rate ride
  along on every acquire, so limit changes take effect on the next
  request; a capacity shrink clamps stored balances down.
- **`cost > capacity` is a typed error (`ErrCostExceedsCapacity`), not a
  denial** — it can never succeed, and M9 must distinguish "wait and
  requeue" from "perm-fail". A denial on a never-refilling bucket
  reports the `RetryAfterNever` sentinel for the same reason.
- **The library does not log**: acquire is a per-request hot-path
  primitive (~141µs sequential / ~44µs at GOMAXPROCS parallelism against
  a local dockerized Redis, ≈7× under the 1ms target); deny/429 logging
  belongs to the callers who know the tenant semantics (6.4/M9).

### As built (ticket 6.4: the API middleware)

Enforcement landed as a `rateLimit(class)` middleware mounted after
`requireScope` on every `/v1` route — the bucket key is the
authenticated `key_id` (the root credential rides under `"root"`), so
401/403 requests consume no tokens. Route→class mirrors the scope
table (`POST /v1/runs` → submit, `GET /v1/runs/{id}` → read,
`/v1/keys/*` → admin) with the same walk-based coverage test: a new
`/v1` route cannot ship unclassified. `/healthz` and the 404/405
fallbacks stay exempt, exactly as they are for auth. Class limits and
the global bucket are env-configured
(`AGENTLOOM_API_RATELIMIT_<CLASS>_CAPACITY`/`_REFILL_PER_SEC`, plus
`_ENABLED` and the test-isolation `_KEY_PREFIX`); config validation
requires a strictly positive refill — a rate-zero API bucket would
permanently brick a key, which is never a sane API limit (fixed quotas
remain an M9 shape). Decisions made here:

- **Fail-open on Redis errors.** An `Acquire` failure logs at Error and
  lets the request through. Rate limits are protective, not
  correctness; Postgres stays the API's only hard dependency, `cmd/api`
  opens its Redis client without a boot-time dependency (the boot ping
  is advisory), and `/healthz` never touches Redis. The Redis client in
  the API serves rate-limit buckets *only* — dispatch remains the
  worker fleet's (ADR-002 unchanged).
- **Per-key before global, sequentially.** A caller that has exhausted
  its own bucket is rejected *without* touching the global bucket, so
  one abusive client's 429 storm cannot drain the fleet's shared
  budget. The accepted cost, deliberately not compensated: when the
  global bucket denies, the per-key token already spent is not
  refunded (a refund would be a second write racing other acquirers).
  6.3's deferred two-key atomic script stays deferred until the two
  round trips measurably matter.
- **Headers describe the caller's own class bucket, always.**
  `X-RateLimit-Limit`/`-Remaining` go on every limited response,
  allowed or denied — including global denials, where the per-key
  numbers are still the caller's real quota state. `Retry-After`
  (whole seconds, rounded up so an honoring client never retries
  early) comes from whichever bucket denied. `X-RateLimit-Reset` is
  *derived* client-side — ceil((capacity − remaining)/refill) seconds
  until full — taking the 6.3 deferred decision's cheap branch: ≤ 1
  token of imprecision is immaterial for a pacing hint and keeps the
  library reply untouched.
- **429 body** is the standard envelope with new code `rate_limited`
  (contract, like every other code); denials log with `key_id`, class,
  and the global/per-key distinction — the caller-side deny logging
  6.3 deferred here.
- **Metrics are a stubbed seam** (`RateLimitMetrics`: per-bucket
  decisions + fail-open events, no-op until M7 wires the 429 counters
  the roadmap names).

### As built (ticket 6.5: lifecycle endpoints & idempotency)

6.5 filled in the pre-assigned rows: every lifecycle route
(`cancel`/`park`/`unpark`/`steps/{sid}/requeue`) and definition-create
route mounts `requireScope(submit) + rateLimit(submit)`, the listings
(`GET /v1/runs`, `GET /v1/definitions*`) mount read/read — route→class
keeps mirroring the scope table, enforced by the same walk-based
coverage tests. The API's lifecycle handlers call `engine.Control`
(Cancel/Park/Unpark/Requeue extracted from Engine in 6.5), built by
`api.New` over the same store and clock with **no dispatcher nudge**:
the ops' outbox rows are drained on the worker fleet's dispatch cadence,
keeping ADR-002's "the API never dispatches" intact. Wrong-state
refusals surface as 409 with new envelope code `conflict`.

Submission idempotency was hardened here (the post-M4 audit items):

- **The token rides the `Idempotency-Key` header**, not the body — the
  4.6 `idempotency_token` body field is gone (pre-1.0 break; ctl moved
  with it, 6.6 pins the contract).
- **Length is bounded** at `store.MaxIdempotencyTokenLength` (200
  bytes) with a 400 — an unbounded token used to surface as a 500 on
  the btree index row limit.
- **The token is fingerprinted to its payload**: CreateRun stores the
  hex SHA-256 over the canonical definition snapshot, the canonicalized
  params, and the definition ref (`runs.idempotency_fingerprint`,
  migration 0009). Replaying a token with the same payload returns the
  original run (200, `reused`); a different payload is a 409 with new
  code `idempotency_key_conflict` instead of silently returning the
  original. Formatting-only differences (params key order) are not a
  mismatch; pre-0009 rows (NULL fingerprint) are grandfathered as
  unchecked reuse.
- **Tokens are global, not per-key** — the unique index is on the bare
  column, so two clients sharing a token collide. Recorded as accepted:
  scoping later is an additive column keyed on `key_id`, not a contract
  break, and v1's operator count makes collisions a non-issue.

Easier:

- 6.2 is pure wiring: the verifier, scope model, and error semantics are
  decided; the middleware generalizes an existing gate.
- A leaked database dump contains no usable credentials — hashes of
  256-bit secrets and 11-char prefixes only.
- Revocation and expiry are ordinary row predicates; no token
  infrastructure, no clock-skew bugs.
- The prefix gives operators a safe correlation handle in logs and
  listings, so "never log the key" costs no debuggability.
- One limiter library serves M6 and M9; the concurrency-correctness test
  burden is paid once.

Harder / accepted costs:

- Every authenticated request costs one Postgres read. Fine at v1
  traffic; a read-through cache is the obvious later optimization and
  deliberately out of scope (it would reintroduce revocation lag, the
  thing keys were chosen to avoid).
- Plaintext-shown-once means a lost key is unrecoverable by design;
  operators re-mint.
- `admin` implying all scopes is coarse: there is no "can manage keys
  but not submit runs". Acceptable for v1's operator count.
- The root key is a standing secret in process env wherever it is set;
  mitigated by the documented set → mint → unset flow.
- Scope names are now contract: renaming one is a breaking change, same
  as error codes.

## Alternatives considered

- **JWT (HS256/RS256) for v1.** Rejected: no revocation without a
  denylist (which is just this table with extra steps), key rotation and
  clock-skew handling for zero v1 benefit — there is no IdP, no browser
  session, and no multi-service token audience yet. Backlog for human
  SSO.
- **bcrypt/argon2 for key hashing.** Rejected above: KDFs defend
  low-entropy secrets; these are 256-bit random. Per-request CPU cost
  with no threat-model gain.
- **HMAC with a server-side pepper.** Rejected: adds an unrotatable
  second secret to defend only the stolen-table-without-stolen-config
  case.
- **Storing plaintext keys.** Rejected outright: violates the no-secrets
  invariant; a DB leak becomes a full credential leak.
- **Random-suffix lookup by full-hash index only (no prefix column).**
  Workable (hash is UNIQUE), but the prefix buys an operator-safe
  display/correlation handle and makes the "what may appear in logs"
  rule mechanical. Kept.
- **Bootstrap by seeding an admin key row in a migration.** Rejected: a
  migration cannot emit a secret without persisting or printing it —
  exactly what the model forbids. Env root key keeps the secret out of
  both code and state.
- **`ctl keys` talking directly to Postgres.** Rejected: 4.6 decided ctl
  is a pure HTTP client; a second write path into the store would bypass
  the API's validation and logging and complicate deployment (ctl would
  need a DSN).
- **Per-route API keys / capability URLs.** Over-engineered for four
  scopes and v1's client count.
