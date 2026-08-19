import { expect, test, type APIRequestContext } from "@playwright/test";

/**
 * 18.3 — the tabbed step inspector, driven live against compose. A semantic
 * retry runs on the offline echo mock: `draft`'s authored prompt omits the
 * marker (attempt 1 fails the `contains` validator), and the feedback template
 * appends the marker (attempt 2 passes) — so the Validation tab shows a
 * fail→pass verdict pair and, the killer demo (DoD-2), the prompt diff between
 * the two attempts is pure additions (the feedback augmentation).
 */
const API_URL = process.env.AGENTLOOM_API_URL ?? "http://127.0.0.1:8080";
const API_KEY = process.env.AGENTLOOM_API_KEY ?? "";

// The echo mock returns "[mock] <the last user text>". So attempt 1's output
// omits APPROVED (prompt has none), attempt 2's includes it (the feedback
// template appends "end your reply with APPROVED").
function semanticRetry(name: string) {
  return {
    schema_version: 1,
    name,
    steps: [
      {
        id: "draft",
        type: "llm",
        config: { model: "mock/sim-1", prompt: "Write a short launch blurb.", max_tokens: 128, temperature: 0 },
        validation: {
          validators: [{ name: "contains", config: { substring: "APPROVED" }, target: "/text" }],
          max_attempts: 3,
          feedback: {
            template:
              "This is attempt ${{ feedback.attempt }} of ${{ feedback.max_attempts }}. Problems: ${{ feedback.issues }}. Now end your reply with APPROVED.",
            max_output_chars: 2000,
          },
        },
      },
      { id: "publish", type: "echo", config: { input: { blurb: "${{ steps.draft.output.text }}" } } },
    ],
    edges: [{ from: "draft", to: "publish" }],
  };
}

async function submit(request: APIRequestContext, name: string): Promise<string> {
  const res = await request.post(`${API_URL}/v1/runs`, {
    headers: { authorization: `Bearer ${API_KEY}`, "content-type": "application/json" },
    data: { definition: semanticRetry(name) },
  });
  expect(res.ok(), `submit ${name}: ${res.status()}`).toBeTruthy();
  return (await res.json()).run_id as string;
}

test("step inspector renders every tab and the semantic-retry prompt diff", async ({ page, request }) => {
  expect(API_KEY).not.toBe("");
  const runId = await submit(request, `inspector_${Date.now()}`);

  await page.goto(`/runs/${runId}`);
  await expect(page.getByTestId("run-id")).toContainText(runId);

  // The draft node reaches succeeded (after a semantic retry).
  const draft = page.locator('[data-testid="run-node"][data-step-id="draft"]');
  await expect(draft).toHaveAttribute("data-step-status", "succeeded", { timeout: 30_000 });

  // Select it — the inspector opens on the Overview tab.
  await draft.click();
  await expect(page.getByTestId("inspector-pane")).toHaveAttribute("data-step-id", "draft");
  await expect(page.getByTestId("inspector-overview")).toBeVisible();
  // Overview: an idempotency key (UUID) and the model section render.
  await expect(page.getByTestId("idempotency-key")).toContainText(/[0-9a-f-]{36}/);
  await expect(page.getByTestId("inspector-model")).toBeVisible();

  // Output tab: the JSON viewer over the step output.
  await page.getByTestId("inspector-tab-output").click();
  await expect(page.getByTestId("json-viewer")).toBeVisible();

  // Validation tab: fail→pass verdicts and the prompt diff (DoD-2).
  await page.getByTestId("inspector-tab-validation").click();
  const verdicts = page.getByTestId("verdict-list");
  await expect(verdicts).toBeVisible();
  const statuses = await verdicts
    .locator("li[data-status]")
    .evaluateAll((els) => els.map((e) => e.getAttribute("data-status")));
  expect(statuses).toEqual(["fail", "pass"]);

  const diff = page.getByTestId("prompt-diff");
  await expect(diff).toBeVisible();
  expect(Number(await diff.getAttribute("data-added-lines"))).toBeGreaterThan(0);
  expect(await diff.getAttribute("data-deleted-lines")).toBe("0");
  // The added lines carry the feedback text.
  await expect(diff).toContainText("end your reply with APPROVED");

  // Logs tab renders (lines or the empty state — the mock executor may log none).
  await page.getByTestId("inspector-tab-logs").click();
  await expect(page.getByTestId("inspector-logs")).toBeVisible();

  // Cost tab: the two productive ledger rows.
  await page.getByTestId("inspector-tab-cost").click();
  await expect(page.getByTestId("inspector-cost")).toBeVisible();
});
