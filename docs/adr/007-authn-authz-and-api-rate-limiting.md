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

## Consequences

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
