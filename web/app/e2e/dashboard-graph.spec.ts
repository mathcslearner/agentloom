import { expect, test, type APIRequestContext } from "@playwright/test";

/**
 * 18.2 — the live DAG view. Driven against the compose backend (offline mock +
 * test executors). Covers the DoD:
 *   - status skins driven by live events (a retry → running → succeeded chain
 *     and a skipped branch, visible on the canvas without a refresh);
 *   - planner expansion animating in with a provenance badge while the authored
 *     nodes' layout stays put (no reshuffle).
 * Loop-iteration grouping (collapse/expand) is exercised too.
 */
const API_URL = process.env.AGENTLOOM_API_URL ?? "http://127.0.0.1:8080";
const API_KEY = process.env.AGENTLOOM_API_KEY ?? "";

async function submit(request: APIRequestContext, definition: unknown): Promise<string> {
  const res = await request.post(`${API_URL}/v1/runs`, {
    headers: { authorization: `Bearer ${API_KEY}`, "content-type": "application/json" },
    data: { definition },
  });
  expect(res.ok(), `submit: ${res.status()} ${await res.text()}`).toBeTruthy();
  return (await res.json()).run_id as string;
}

function retryFixture(name: string) {
  return {
    schema_version: 1,
    name,
    steps: [
      { id: "start", type: "echo", config: { input: { go: true } } },
      { id: "flaky", type: "fail_n_times", config: { n: 1 }, retry: { max_attempts: 3, backoff: { initial: "1200ms", cap: "3s" } } },
      { id: "maybe", type: "echo", config: { input: { x: 1 } } },
    ],
    edges: [
      { from: "start", to: "flaky" },
      { from: "start", to: "maybe", when: "false" },
    ],
  };
}

function plannerFixture(name: string) {
  const plan = JSON.stringify({
    schema_version: 1,
    steps: [
      { id: "work_a", type: "llm", config: { model: "mock/sim-1", prompt: "A", max_tokens: 32, temperature: 0 } },
      { id: "work_b", type: "llm", config: { model: "mock/sim-1", prompt: "B", max_tokens: 32, temperature: 0 } },
    ],
    edges: [
      { from: "plan", to: "work_a" },
      { from: "plan", to: "work_b" },
      { from: "work_a", to: "gather" },
      { from: "work_b", to: "gather" },
    ],
  });
  return {
    schema_version: 1,
    name,
    steps: [
      { id: "warmup", type: "sleep", config: { duration: "2800ms" } },
      { id: "plan", type: "planner", config: { model: "mock/sim-1", prompt: plan, max_tokens: 512, temperature: 0, max_added_steps: 8 }, validation: { max_attempts: 3 } },
      { id: "gather", type: "join", config: { mode: "all" } },
      { id: "report", type: "echo", config: { input: { ok: true } } },
    ],
    edges: [
      { from: "warmup", to: "plan" },
      { from: "plan", to: "gather" },
      { from: "gather", to: "report" },
    ],
    expansion: { max_added_steps: 16, max_expansions: 10, max_depth: 2 },
  };
}

function loopFixture(name: string) {
  return {
    schema_version: 1,
    name,
    steps: [
      { id: "entry", type: "echo", config: { input: { i: 1 } } },
      { id: "sink", type: "echo", config: { input: { done: true } } },
      { id: "finish", type: "echo", config: { input: { end: true } } },
    ],
    edges: [
      { from: "entry", to: "sink" },
      { from: "sink", to: "entry", type: "loop", condition: "true", max_iterations: 2, on_exhausted: "proceed" },
      { from: "sink", to: "finish" },
    ],
  };
}

test.beforeEach(() => {
  expect(API_KEY, "AGENTLOOM_API_KEY must be set (compose root key)").not.toBe("");
});

test("status skins are driven by live events (retry + skip)", async ({ page, request }) => {
  const runId = await submit(request, retryFixture(`m182_status_${Date.now()}`));
  await page.goto(`/runs/${runId}`);

  const graph = page.getByTestId("run-graph");
  await expect(graph).toBeVisible();

  // The flaky step retries once (attempt 2) then succeeds — the live status
  // skin ends succeeded with the second attempt recorded.
  const flaky = page.locator('[data-testid="run-node"][data-step-id="flaky"]');
  await expect(flaky).toHaveAttribute("data-step-status", "succeeded", { timeout: 20_000 });
  await expect(flaky).toHaveAttribute("data-attempt", "2");

  // The false-`when` branch skips its target, live.
  const maybe = page.locator('[data-testid="run-node"][data-step-id="maybe"]');
  await expect(maybe).toHaveAttribute("data-step-status", "skipped", { timeout: 20_000 });

  // The run reaches terminal.
  await expect(page.getByTestId("run-detail")).toHaveAttribute("data-run-status", "succeeded", { timeout: 20_000 });
});

test("planner expansion animates in with provenance; authored layout stays put", async ({ page, request }) => {
  const runId = await submit(request, plannerFixture(`m182_planner_${Date.now()}`));
  await page.goto(`/runs/${runId}`);

  const graph = page.getByTestId("run-graph");
  await expect(graph).toBeVisible();
  await expect(graph).toHaveAttribute("data-layout-ready", "true", { timeout: 15_000 });

  // Before the planner completes, the injected work nodes are absent.
  const workA = page.locator('[data-testid="run-node"][data-step-id="work_a"]');
  await expect(workA).toHaveCount(0);

  // Capture the authored `gather` node's canvas transform (its layout position).
  const gatherWrap = page.locator('.react-flow__node[data-id="gather"]');
  await expect(gatherWrap).toBeVisible();
  const beforeTransform = await gatherWrap.evaluate((el) => (el as HTMLElement).style.transform);

  // The planner injects work_a / work_b live, badged with planner provenance.
  await expect(workA).toBeVisible({ timeout: 20_000 });
  await expect(workA).toHaveAttribute("data-origin-kind", "planner");
  await expect(workA.locator('[data-testid="node-provenance"]')).toBeVisible();

  // The authored `gather` node did not move (sticky, no reshuffle).
  const afterTransform = await gatherWrap.evaluate((el) => (el as HTMLElement).style.transform);
  expect(afterTransform).toBe(beforeTransform);

  await expect(page.getByTestId("run-detail")).toHaveAttribute("data-run-status", "succeeded", { timeout: 25_000 });
});

test("loop iterations group and collapse", async ({ page, request }) => {
  const runId = await submit(request, loopFixture(`m182_loop_${Date.now()}`));
  await page.goto(`/runs/${runId}`);

  await expect(page.getByTestId("run-detail")).toHaveAttribute("data-run-status", "succeeded", { timeout: 20_000 });

  // Loop instances rendered as grouped containers.
  await expect(page.locator('[data-testid="run-group"]').first()).toBeVisible({ timeout: 20_000 });
  const groups = page.locator('[data-testid="run-group"]');
  expect(await groups.count()).toBeGreaterThanOrEqual(2); // two iterations

  // A loop instance node carries loop provenance.
  await expect(page.locator('[data-testid="run-node"][data-step-id="sink#1"]')).toHaveAttribute("data-origin-kind", "loop");

  // Collapse all groups: the member instance node is hidden.
  await page.getByTestId("collapse-groups").click();
  await expect(page.locator('[data-testid="run-group"][data-collapsed="true"]').first()).toBeVisible();
});
