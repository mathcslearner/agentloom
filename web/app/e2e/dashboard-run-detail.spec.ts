import { expect, test, type APIRequestContext } from "@playwright/test";

/**
 * 18.1 DoD-2 (reconnect mid-run resumes with no visual gaps) and DoD-3 (the
 * timeline renders the normalized feed with type filtering), driven through the
 * run-detail page against compose.
 */
const API_URL = process.env.AGENTLOOM_API_URL ?? "http://127.0.0.1:8080";
const API_KEY = process.env.AGENTLOOM_API_KEY ?? "";

function sleepChain(name: string) {
  return {
    schema_version: 1,
    name,
    steps: [
      { id: "s1", type: "sleep", config: { duration: "1500ms" } },
      { id: "s2", type: "sleep", config: { duration: "1500ms" } },
    ],
    edges: [{ from: "s1", to: "s2" }],
  };
}

async function submit(request: APIRequestContext, name: string): Promise<string> {
  const res = await request.post(`${API_URL}/v1/runs`, {
    headers: { authorization: `Bearer ${API_KEY}`, "content-type": "application/json" },
    data: { definition: sleepChain(name) },
  });
  expect(res.ok(), `submit ${name}: ${res.status()}`).toBeTruthy();
  return (await res.json()).run_id as string;
}

test("reconnect mid-run resumes with no visual gaps", async ({ page, request }) => {
  expect(API_KEY).not.toBe("");

  // Route the run WebSocket so we can drop it exactly once, mid-run.
  let dropped = false;
  await page.routeWebSocket(/\/v1\/runs\/.*\/ws/, (ws) => {
    ws.connectToServer(); // forward both directions
    if (!dropped) {
      dropped = true;
      // Close the client side after a beat so the client reconnects mid-run.
      setTimeout(() => ws.close(), 1200);
    }
  });

  const runId = await submit(request, `dash_detail_${Date.now()}`);
  await page.goto(`/runs/${runId}`);

  // The connection goes live and the run is visible.
  await expect(page.getByTestId("run-id")).toContainText(runId);

  // A reconnect is surfaced.
  await expect(page.getByTestId("reconnect-count")).toBeVisible({ timeout: 20_000 });

  // The run reaches a terminal status live (no refresh).
  await expect(page.getByTestId("run-detail")).toHaveAttribute("data-run-status", "succeeded", {
    timeout: 25_000,
  });

  // The assembled timeline is gap-free and dup-free (seq-resume through the UI).
  const seqs = await page.locator('[data-testid="timeline-row"]').evaluateAll((rows) =>
    rows.map((r) => Number(r.getAttribute("data-seq"))),
  );
  expect(seqs.length).toBeGreaterThan(0);
  const sorted = [...seqs].sort((a, b) => a - b);
  expect(new Set(seqs).size).toBe(seqs.length); // no dupes
  for (let i = 0; i < sorted.length; i++) {
    expect(sorted[i]).toBe(i + 1); // contiguous 1..N
  }
});

test("the timeline filters by event category", async ({ page, request }) => {
  expect(API_KEY).not.toBe("");
  const runId = await submit(request, `dash_timeline_${Date.now()}`);
  await page.goto(`/runs/${runId}`);

  // Wait for the run to finish so the full feed is present.
  await expect(page.getByTestId("run-detail")).toHaveAttribute("data-run-status", "succeeded", {
    timeout: 25_000,
  });

  const allRows = await page.locator('[data-testid="timeline-row"]').count();
  expect(allRows).toBeGreaterThan(0);

  // Filter to the "run" category — fewer rows, all of that category.
  await page.getByTestId("timeline-filter-run").click();
  const runRows = page.locator('[data-testid="timeline-row"]');
  await expect(runRows.first()).toBeVisible();
  const cats = await runRows.evaluateAll((rows) => rows.map((r) => r.getAttribute("data-category")));
  expect(cats.every((c) => c === "run")).toBe(true);
  expect(cats.length).toBeLessThan(allRows);
});
