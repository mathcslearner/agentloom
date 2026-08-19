import { expect, test, type APIRequestContext } from "@playwright/test";

/**
 * 18.1 DoD-1: a submitted run appears in the list and updates its status live
 * (over the firehose WebSocket) with no page refresh.
 */
const API_URL = process.env.AGENTLOOM_API_URL ?? "http://127.0.0.1:8080";
const API_KEY = process.env.AGENTLOOM_API_KEY ?? "";

function sleepChain(name: string) {
  return {
    schema_version: 1,
    name,
    steps: [
      { id: "s1", type: "sleep", config: { duration: "1s" } },
      { id: "s2", type: "sleep", config: { duration: "1s" } },
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

test("a submitted run appears and updates in the list without refresh", async ({ page, request }) => {
  expect(API_KEY, "AGENTLOOM_API_KEY must be set for the e2e").not.toBe("");

  await page.goto("/runs");
  await expect(page.getByRole("heading", { name: "Runs" })).toBeVisible();
  // The firehose connects.
  await expect(page.getByTestId("connection-pill")).toBeVisible();

  // Submit AFTER the page loaded: the run must be discovered live (run_created).
  const runId = await submit(request, `dash_live_${Date.now()}`);

  const row = page.locator(`[data-testid="run-row"][data-run-id="${runId}"]`);
  await expect(row).toBeVisible({ timeout: 15_000 });
  // It starts running…
  await expect(row).toHaveAttribute("data-status", "running");
  // …and flips to succeeded live, with no reload.
  await expect(row).toHaveAttribute("data-status", "succeeded", { timeout: 20_000 });
});
