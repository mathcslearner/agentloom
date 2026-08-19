import { expect, test, type APIRequestContext, type Page } from "@playwright/test";

/**
 * 18.5 — the HITL approval inbox & decision UI, driven against the compose
 * backend (offline mock provider + test executors). Covers the DoD:
 *   - DoD-1: a run pauses at a gate → approve WITH an edit via the UI → the run
 *     continues and the edited payload is visible downstream;
 *   - DoD-2: a concurrent decision from another "session" (a raw REST call) →
 *     the open dialog surfaces the conflict gracefully;
 *   - DoD-3: reject routes per config (on_reject: route → the reject branch
 *     runs, the approve branch is skipped) and the outcome renders.
 * Plus the inbox lists the pending gate live.
 */
const API_URL = process.env.AGENTLOOM_API_URL ?? "http://127.0.0.1:8080";
const API_KEY = process.env.AGENTLOOM_API_KEY ?? "";

// A gate with an editable payload and on_reject: fail (approval_gate.json shape,
// minus the topic param so submit is self-contained). The echo mock makes
// `draft` deterministic; publish reads the approved (possibly edited) payload.
function gateDef(name: string) {
  return {
    schema_version: 1,
    name,
    steps: [
      { id: "draft", type: "llm", config: { model: "mock/sim-1", prompt: "Draft an article.", max_tokens: 64 } },
      {
        id: "approve_publish",
        type: "human_approval",
        config: {
          title: "Publish this article?",
          description: "Review before it goes live.",
          payload: "${{ steps.draft.output }}",
          allowed_decisions: ["approve", "reject"],
          allow_edit: true,
          edit_schema: { type: "object", properties: { text: { type: "string" } } },
          timeout: "48h",
          on_timeout: "reject",
          on_reject: "fail",
        },
      },
      { id: "publish", type: "echo", config: { input: { published: "${{ steps.approve_publish.output.payload }}" } } },
    ],
    edges: [
      { from: "draft", to: "approve_publish" },
      { from: "approve_publish", to: "publish" },
    ],
  };
}

// on_reject: route — a rejected decision fires only the reject-marked edge.
function rejectRouteDef(name: string) {
  return {
    schema_version: 1,
    name,
    steps: [
      { id: "draft", type: "llm", config: { model: "mock/sim-1", prompt: "Draft an article.", max_tokens: 64 } },
      {
        id: "review",
        type: "human_approval",
        config: {
          title: "Publish or send back?",
          payload: "${{ steps.draft.output }}",
          allowed_decisions: ["approve", "reject"],
          on_reject: "route",
        },
      },
      { id: "publish", type: "echo", config: { input: { published: "${{ steps.review.output.payload }}" } } },
      { id: "notify_rejected", type: "echo", config: { input: { comment: "${{ steps.review.output.comment }}" } } },
    ],
    edges: [
      { from: "draft", to: "review" },
      { from: "review", to: "publish" },
      { from: "review", to: "notify_rejected", decision: "reject" },
    ],
  };
}

async function submit(request: APIRequestContext, definition: unknown): Promise<string> {
  const res = await request.post(`${API_URL}/v1/runs`, {
    headers: { authorization: `Bearer ${API_KEY}`, "content-type": "application/json" },
    data: { definition },
  });
  expect(res.ok(), `submit: ${res.status()} ${await res.text()}`).toBeTruthy();
  return (await res.json()).run_id as string;
}

async function fetchRun(request: APIRequestContext, runId: string): Promise<{ run: { status: string }; steps: { id: string; status: string; output?: unknown }[] }> {
  const res = await request.get(`${API_URL}/v1/runs/${runId}`, { headers: { authorization: `Bearer ${API_KEY}` } });
  expect(res.ok()).toBeTruthy();
  return res.json();
}

async function pendingApprovalId(request: APIRequestContext, runId: string): Promise<string> {
  const res = await request.get(`${API_URL}/v1/approvals?run_id=${runId}&status=pending`, {
    headers: { authorization: `Bearer ${API_KEY}` },
  });
  expect(res.ok()).toBeTruthy();
  const body = await res.json();
  return body.approvals[0].id as string;
}

async function waitAwaitingHuman(page: Page, stepId: string): Promise<void> {
  const node = page.locator(`[data-testid="run-node"][data-step-id="${stepId}"]`);
  await expect(node).toHaveAttribute("data-step-status", "awaiting_human", { timeout: 30_000 });
}

test.beforeEach(() => {
  expect(API_KEY, "AGENTLOOM_API_KEY must be set for the e2e").not.toBe("");
});

test("DoD-1: pause → approve with edit via the UI → run continues, edited payload downstream", async ({ page, request }) => {
  const runId = await submit(request, gateDef(`approve_edit_${Date.now()}`));
  await page.goto(`/runs/${runId}`);
  await expect(page.getByTestId("run-id")).toContainText(runId);
  await waitAwaitingHuman(page, "approve_publish");

  // The "waiting on you" affordance is on the node and the header banner.
  await expect(page.getByTestId("approval-banner")).toBeVisible();
  await page.locator('[data-testid="run-node"][data-step-id="approve_publish"] [data-testid="node-decide"]').click();

  const dialog = page.getByTestId("decision-dialog");
  await expect(dialog).toBeVisible();
  // Edit the payload: set a recognizable article text.
  await page.getByTestId("decision-edit-toggle").click();
  await page.getByTestId("decision-editor").fill('{"text":"EDITED-ARTICLE-18-5"}');
  await page.getByTestId("decision-approve").click();

  // The run reaches succeeded and the edited text flows into publish.
  await expect(page.getByTestId("run-status")).toHaveText("succeeded", { timeout: 30_000 });
  await expect.poll(async () => {
    const body = await fetchRun(request, runId);
    const publish = body.steps.find((s) => s.id === "publish");
    return JSON.stringify(publish?.output ?? {});
  }, { timeout: 30_000 }).toContain("EDITED-ARTICLE-18-5");
});

test("DoD-2: a concurrent decision from another session surfaces the 409 gracefully", async ({ page, request }) => {
  const runId = await submit(request, gateDef(`conflict_${Date.now()}`));
  await page.goto(`/runs/${runId}`);
  await waitAwaitingHuman(page, "approve_publish");

  // Open the decision dialog.
  await page.locator('[data-testid="run-node"][data-step-id="approve_publish"] [data-testid="node-decide"]').click();
  await expect(page.getByTestId("decision-dialog")).toBeVisible();

  // Another session decides it first (raw REST approve).
  const approvalId = await pendingApprovalId(request, runId);
  const decideRes = await request.post(`${API_URL}/v1/approvals/${approvalId}:decide`, {
    headers: { authorization: `Bearer ${API_KEY}`, "content-type": "application/json" },
    data: { decision: "approve" },
  });
  expect(decideRes.ok(), `decide: ${decideRes.status()} ${await decideRes.text()}`).toBeTruthy();

  // This session tries to approve — the dialog reveals the decided-elsewhere
  // state either from the live approval_decided event (which, on a fast stack,
  // arrives first and swaps the dialog to the conflict view, removing the
  // approve button) or from the 409 on submit. The click is best-effort with a
  // short timeout so a vanished button (the live-event-won race) does not hang
  // the whole test before the conflict assertion runs.
  await page.getByTestId("decision-approve").click({ timeout: 3000 }).catch(() => {});
  await expect(page.getByTestId("decision-conflict")).toBeVisible({ timeout: 30_000 });
});

test("DoD-3: reject routes per config and the outcome renders", async ({ page, request }) => {
  const runId = await submit(request, rejectRouteDef(`reject_${Date.now()}`));
  await page.goto(`/runs/${runId}`);
  await waitAwaitingHuman(page, "review");

  await page.locator('[data-testid="run-node"][data-step-id="review"] [data-testid="node-decide"]').click();
  const dialog = page.getByTestId("decision-dialog");
  await expect(dialog).toBeVisible();
  // The reject-plan hint names the routed target.
  await expect(page.getByTestId("decision-reject-plan")).toContainText("notify_rejected");
  await page.getByTestId("decision-comment").fill("needs another pass");
  await page.getByTestId("decision-reject").click();

  // The run succeeds; the reject branch ran, the approve branch was skipped.
  await expect(page.getByTestId("run-status")).toHaveText("succeeded", { timeout: 30_000 });
  const body = await fetchRun(request, runId);
  expect(body.steps.find((s) => s.id === "notify_rejected")?.status).toBe("succeeded");
  expect(body.steps.find((s) => s.id === "publish")?.status).toBe("skipped");

  // The Approval tab renders the routed outcome.
  await page.locator('[data-testid="run-node"][data-step-id="review"]').click();
  await page.getByTestId("inspector-tab-approval").click();
  await expect(page.getByTestId("approval-outcome")).toContainText("rejected");
});

test("the inbox lists a pending gate live and links into the run", async ({ page, request }) => {
  const runId = await submit(request, gateDef(`inbox_${Date.now()}`));
  // Wait for the gate to park (poll the API so the run is definitely awaiting).
  await expect.poll(async () => {
    const res = await request.get(`${API_URL}/v1/approvals?run_id=${runId}&status=pending`, {
      headers: { authorization: `Bearer ${API_KEY}` },
    });
    return (await res.json()).approvals.length;
  }, { timeout: 30_000 }).toBeGreaterThan(0);

  await page.goto("/approvals");
  const row = page.locator(`[data-testid="approval-row"]`).filter({ has: page.locator(`a[href="/runs/${runId}"]`) });
  await expect(row).toBeVisible({ timeout: 30_000 });
  await expect(row.getByTestId("approval-age")).toBeVisible();
  await expect(row.getByTestId("row-decide")).toBeVisible();
});
