/**
 * gen-openapi-types.ts — generate `src/generated/openapi.ts` from the backend's
 * hand-maintained OpenAPI contract (`api/openapi.yaml`, ticket 6.6).
 *
 * The Go handlers implement the contract and `TestOpenAPIRouteCoverage` /
 * `TestOpenAPIOperationContracts` keep the spec and the routes in lockstep; this
 * script emits the TypeScript `paths`/`components`/`operations` types from that
 * same spec. So the typed client cannot drift from the documented API — the
 * committed `openapi.ts` is CI-diffed against a fresh run of this generator (the
 * same two-layer drift discipline as engine-client's events.ts).
 *
 * The spec `$ref`s two generated external schemas
 * (`docs/schema/{workflow-definition,events}.v1.json`); openapi-typescript
 * resolves and inlines them, so a change to the Go dag/event structs flows all
 * the way through to these types and is caught by the drift check.
 *
 * openapi-typescript's output is deterministic given the same input, which is
 * what a CI diff needs.
 *
 * Run: `pnpm generate` (from web/lib/api-client).
 */
import { writeFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import openapiTS, { astToString } from "openapi-typescript";

const here = dirname(fileURLToPath(import.meta.url));
const SPEC_PATH = resolve(here, "../../../../api/openapi.yaml");
const OUT_PATH = resolve(here, "../src/generated/openapi.ts");

const ast = await openapiTS(new URL(`file://${SPEC_PATH}`));
const body = astToString(ast);

const header = `/**
 * GENERATED FILE — do not edit by hand.
 *
 * Emitted from api/openapi.yaml by scripts/gen-openapi-types.ts (\`pnpm generate\`).
 * CI regenerates this and fails on any diff, so it cannot drift from the spec.
 */
`;

writeFileSync(OUT_PATH, header + body);
process.stderr.write(`wrote ${OUT_PATH}\n`);
