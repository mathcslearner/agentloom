import { expect, test, type APIRequestContext } from "@playwright/test";

/**
 * 18.6 — the ops views (DLQ list, queue health, run controls), driven against
 * the compose backend (offline mock provider + test executors). Covers the DoD:
 *   - DoD-1: a step exhausts its retry budget into the DLQ → Requeue from the
 *     /ops page → the step re-runs and the run reaches succeeded, and the row
 *     leaves the open list;
 *   - run controls: park → unpark → cancel on a sleep chain, reflected live;
 *   - the queue-health panel renders live figures.
 */
const API_URL = process.env.AGENTLOOM_API_URL ?? "http://127.0.0.1:8080";
const API_KEY = process.env.AGENTLOOM_API_KEY ?? "";

// A run whose first step exhausts a 2-attempt budget into the DLQ; fail_n_times
// keys off the durable attempt number, so a requeue's attempt 3 succeeds.
function dlqDef(name: string) {
  return {
    schema_version: 1,
    name,
    steps: [
      {
        id: "flaky",
        type: "fail_n_times",
        config: { n: 2 },
        retry: {
          max_attempts: 2,
          backoff: { initial: "50ms", cap: "100ms", multiplier: 2 },
          jitter: "full",
          retry_on: ["transient"],
        },
      },
      { id: "done", type: "noop" },
    ],
    edges: [{ from: "flaky", to: "done" }],
  };
}

function sleepChain(name: string) {
  return {
    schema_version: 1,
    name,
    steps: [
      { id: "s1", type: "sleep", config: { duration: "30s" } },
      { id: "s2", type: "noop" },
    ],
    edges: [{ from: "s1", to: "s2" }],
  };
}

async function submit(request: APIRequestContext, def: object): Promise<string> {
  const res = await request.post(`${API_URL}/v1/runs`, {
    headers: { authorization: `Bearer ${API_KEY}`, "content-type": "application/json" },
    data: { definition: def },
  });
  expect(res.ok(), `submit: ${res.status()}`).toBeTruthy();
  return (await res.json()).run_id as string;
}

async function runStatus(request: APIRequestContext, runId: string): Promise<string> {
  const res = await request.get(`${API_URL}/v1/runs/${runId}`, {
    headers: { authorization: `Bearer ${API_KEY}` },
  });
  expect(res.ok()).toBeTruthy();
  return (await res.json()).run.status as string;
}

test("DoD-1: requeue a dead-lettered step from /ops → the run completes", async ({ page, request }) => {
  expect(API_KEY).not.toBe("");
  const name = `ops-dlq-${Date.now()}`;
  const runId = await submit(request, dlqDef(name));

  // Wait for the DLQ row (the run fails fast into the DLQ).
  await expect.poll(() => runStatus(request, runId), { timeout: 20_000 }).toBe("failed");

  await page.goto("/ops");
  const row = page.locator(`[data-testid="dlq-row"][data-run-id="${runId}"]`);
  await expect(row).toBeVisible({ timeout: 15_000 });
  expect(await row.getAttribute("data-open")).toBe("true");

  // Requeue from the UI.
  await row.getByTestId("dlq-requeue").click();

  // The run recovers to succeeded (attempt 3 passes).
  await expect.poll(() => runStatus(request, runId), { timeout: 20_000 }).toBe("succeeded");

  // The row leaves the open list (filter open is the default).
  await page.goto("/ops");
  await expect(
    page.locator(`[data-testid="dlq-row"][data-run-id="${runId}"]`),
  ).toHaveCount(0, { timeout: 15_000 });
  // …but status=all still shows it (now closed).
  await page.goto("/ops?status=all");
  const allRow = page.locator(`[data-testid="dlq-row"][data-run-id="${runId}"]`);
  await expect(allRow).toBeVisible({ timeout: 15_000 });
  expect(await allRow.getAttribute("data-open")).toBe("false");
});

test("run controls: park → unpark → cancel a sleeping run", async ({ page, request }) => {
  expect(API_KEY).not.toBe("");
  const runId = await submit(request, sleepChain(`ops-controls-${Date.now()}`));

  await page.goto(`/runs/${runId}`);
  // Wait for a worker to hold the sleeping step.
  await expect
    .poll(() => runStatus(request, runId), { timeout: 15_000 })
    .toBe("running");

  const controls = page.getByTestId("run-controls");
  await expect(controls).toBeVisible({ timeout: 10_000 });

  // Park.
  await page.getByTestId("run-park").click();
  await expect.poll(() => runStatus(request, runId), { timeout: 15_000 }).toBe("parked");
  await expect(page.getByTestId("run-status")).toHaveText("parked", { timeout: 10_000 });

  // Unpark.
  await page.getByTestId("run-unpark").click();
  await expect.poll(() => runStatus(request, runId), { timeout: 15_000 }).toBe("running");

  // Cancel (confirm).
  await page.getByTestId("run-cancel").click();
  await page.getByTestId("run-cancel-confirm").click();
  await expect
    .poll(() => runStatus(request, runId), { timeout: 20_000 })
    .toMatch(/cancelling|cancelled/);
});

test("queue-health panel renders live figures", async ({ page, request }) => {
  expect(API_KEY).not.toBe("");
  // Submit a couple of runs so the panel has something to show.
  await submit(request, sleepChain(`ops-health-a-${Date.now()}`));

  await page.goto("/ops");
  await expect(page.getByTestId("queue-health")).toBeVisible({ timeout: 10_000 });
  // The queue block should be present (Redis is up in compose), with a workers
  // tile and an active-runs tile.
  await expect(page.getByTestId("stat-runs")).toBeVisible({ timeout: 10_000 });
  await expect(page.getByTestId("stat-dead_letters")).toBeVisible();
  // The "as of" indicator confirms the poll landed.
  await expect(page.getByTestId("stats-asof")).toBeVisible();
});
