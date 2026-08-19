import { expect, test, type APIRequestContext, type Page } from "@playwright/test";

/**
 * 18.4 — the live cost meter & budget UX. Driven against the compose backend
 * (offline mock provider + test executors). Covers the DoD:
 *   - a budgeted run climbs the meter, parks at its cap, the user raises the
 *     budget and resumes — all live, no reload (DoD-1);
 *   - a downgrade banner names from/to models and the trigger (DoD-2);
 *   - the meter total matches GET /v1/runs/{id}/cost at completion (DoD-3).
 *
 * The mock prices every model at the `mock:*` catalog rate ($1/$2 per Mtok), so
 * the budget arithmetic is deterministic. Response caching is disabled per step
 * — the global response cache would otherwise serve a repeat run for $0 and the
 * run would never climb or park.
 */
const API_URL = process.env.AGENTLOOM_API_URL ?? "http://127.0.0.1:8080";
const API_KEY = process.env.AGENTLOOM_API_KEY ?? "";

/** A ~1.2 KB prompt so each mock llm step books real, non-trivial spend. */
function bigPrompt(tag: string): string {
  return `${tag} ${"lorem ipsum dolor sit amet ".repeat(45)}`; // ~1.2 KB
}

async function submit(request: APIRequestContext, definition: unknown): Promise<string> {
  const res = await request.post(`${API_URL}/v1/runs`, {
    headers: { authorization: `Bearer ${API_KEY}`, "content-type": "application/json" },
    data: { definition },
  });
  expect(res.ok(), `submit: ${res.status()} ${await res.text()}`).toBeTruthy();
  return (await res.json()).run_id as string;
}

async function fetchSpentNano(request: APIRequestContext, runId: string): Promise<number> {
  const res = await request.get(`${API_URL}/v1/runs/${runId}/cost`, {
    headers: { authorization: `Bearer ${API_KEY}` },
  });
  expect(res.ok()).toBeTruthy();
  return (await res.json()).summary.spent_nano_usd as number;
}

function meterSpent(page: Page): Promise<number> {
  return page
    .getByTestId("cost-meter")
    .getAttribute("data-spent-nano")
    .then((v) => Number(v ?? "0"));
}

/** A linear chain of mock llm steps under a tight run budget: it climbs the
 * meter for a few steps, then parks at its cap. */
function budgetedChain(name: string) {
  const steps = [];
  const edges = [];
  for (let i = 1; i <= 8; i++) {
    steps.push({
      id: `s${i}`,
      type: "llm",
      config: { model: "mock/sim-1", prompt: bigPrompt(`s${i}`), max_tokens: 128, temperature: 0 },
      cache: { mode: "off" },
    });
    if (i > 1) edges.push({ from: `s${i - 1}`, to: `s${i}` });
  }
  return { schema_version: 1, name, budget_usd: 0.003, on_budget_exceeded: "park", steps, edges };
}

/** A two-step run: a spending step lifts the run past 30% of budget, then a
 * step carrying a soft `model_fallbacks` threshold downgrades. */
function downgradeChain(name: string) {
  return {
    schema_version: 1,
    name,
    budget_usd: 0.002,
    on_budget_exceeded: "park",
    steps: [
      {
        id: "warm",
        type: "llm",
        config: { model: "mock/sim-1", prompt: bigPrompt("warm"), max_tokens: 128, temperature: 0 },
        cache: { mode: "off" },
      },
      {
        id: "payer",
        type: "llm",
        config: {
          model: "mock/sim-1",
          prompt: bigPrompt("payer"),
          max_tokens: 128,
          temperature: 0,
          model_fallbacks: [{ model: "mock/cheap", at_budget_fraction: 0.3 }],
        },
        cache: { mode: "off" },
      },
    ],
    edges: [{ from: "warm", to: "payer" }],
  };
}

test("DoD-1: budgeted run climbs the meter, parks at cap, raise+resume live", async ({ page, request }) => {
  const runId = await submit(request, budgetedChain(`cost-park-${Date.now()}`));
  await page.goto(`/runs/${runId}`);
  await expect(page.getByTestId("run-id")).toContainText(runId);

  // The meter climbs: spend rises above zero before the run parks.
  await expect.poll(() => meterSpent(page), { timeout: 30_000 }).toBeGreaterThan(0);

  // The run parks at its cap: status parked, the meter tier is exceeded, and the
  // parked banner offers a Raise action.
  await expect(page.getByTestId("run-detail")).toHaveAttribute("data-run-status", "parked", { timeout: 30_000 });
  await expect(page.getByTestId("cost-meter")).toHaveAttribute("data-tier", "exceeded");
  await expect(page.getByTestId("banner-parked_for_budget")).toBeVisible();

  const spentAtPark = await meterSpent(page);
  expect(spentAtPark).toBeGreaterThan(0);

  // Raise the budget and resume, all via the UI.
  await page.getByTestId("banner-raise").click();
  await expect(page.getByTestId("raise-budget-dialog")).toBeVisible();
  await page.getByTestId("budget-input").fill("1");
  await expect(page.getByTestId("budget-resume")).toBeChecked();
  await page.getByTestId("budget-confirm").click();

  // Live: the run resumes and completes without a reload.
  await expect(page.getByTestId("run-detail")).toHaveAttribute("data-run-status", "succeeded", { timeout: 30_000 });
  // The meter kept climbing past the park point.
  await expect.poll(() => meterSpent(page), { timeout: 10_000 }).toBeGreaterThan(spentAtPark);

  // DoD-3: the meter total matches GET /v1/runs/{id}/cost at completion.
  const apiSpent = await fetchSpentNano(request, runId);
  expect(await meterSpent(page)).toBe(apiSpent);
});

test("DoD-2: downgrade banner names from/to models and the trigger", async ({ page, request }) => {
  const runId = await submit(request, downgradeChain(`cost-downgrade-${Date.now()}`));
  await page.goto(`/runs/${runId}`);
  await expect(page.getByTestId("run-id")).toContainText(runId);

  const banner = page.getByTestId("banner-downgrade");
  await expect(banner).toBeVisible({ timeout: 30_000 });
  await expect(banner).toHaveAttribute("data-from", "mock/sim-1");
  await expect(banner).toHaveAttribute("data-to", "mock/cheap");
  await expect(banner).toHaveAttribute("data-trigger", "budget_threshold");

  await expect(page.getByTestId("run-detail")).toHaveAttribute("data-run-status", "succeeded", { timeout: 30_000 });
});
