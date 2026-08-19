/**
 * Inspector test fixtures: the committed Go goldens (internal/api/testdata),
 * the exact wire shapes the backend produces (ticket 18.3). Reading them here
 * ties the frontend inspector tests to the real API contract — a drift in the
 * projection regenerates the golden and surfaces in these tests.
 */
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import type { RunResponse, RunCostResponse, StepLogsResponse } from "@agentloom/api-client";

// vitest runs with cwd = web/app; the Go goldens live at the repo root under
// internal/api/testdata (../../ up from web/app).
const testdata = (name: string): string =>
  resolve(process.cwd(), "../../internal/api/testdata", name);

function load<T>(name: string): T {
  return JSON.parse(readFileSync(testdata(name), "utf8")) as T;
}

export const runDetailFixture = load<RunResponse>("run_detail_fixture.json");
export const runCostFixture = load<RunCostResponse>("run_cost_fixture.json");
export const stepLogsFixture = load<StepLogsResponse>("step_logs_fixture.json");

/** Look up a fixture step by id (fails loudly if the fixture changed shape). */
export function fixtureStep(id: string) {
  const s = runDetailFixture.steps?.find((x) => x.id === id);
  if (!s) throw new Error(`fixture step not found: ${id}`);
  return s;
}

// The approval-inbox golden (ticket 18.5): the exact GET /v1/approvals wire
// shape, covering pending/approved/rejected/expired/park-expired rows.
export const approvalListFixture = load<import("@agentloom/api-client").ApprovalListResponse>(
  "approval_list_fixture.json",
);

// The ops-view goldens (ticket 18.6): the exact GET /v1/dead-letters and
// GET /v1/system/stats wire shapes.
export const deadLetterListFixture = load<import("@agentloom/api-client").DeadLetterListResponse>(
  "dead_letter_list_fixture.json",
);
export const systemStatsFixture = load<import("@agentloom/api-client").SystemStatsResponse>(
  "system_stats_fixture.json",
);
