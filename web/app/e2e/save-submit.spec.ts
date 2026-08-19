import { expect, test } from "@playwright/test";

/**
 * 17.6 save & submit e2e (compose backend, mock provider). Drives the full loop
 * through the UI: import a definition → edit a prompt → save (v1) → edit → save
 * (v2) → a concurrent out-of-band append → save again hits the version-conflict
 * guard → save anyway → submit with the params modal → a run id is returned.
 * DoD-1 (import → edit → save new version → submit → run id) and DoD-3
 * (version-conflict / stale-save handling), wired end to end.
 */

const API_URL = process.env.AGENTLOOM_API_URL ?? "http://127.0.0.1:8080";
const API_KEY = process.env.AGENTLOOM_API_KEY ?? "";

const NAME = `web_save_${Date.now()}`;
const DEF = {
  schema_version: 1,
  name: NAME,
  params: { topic: { type: "string", required: true } },
  steps: [
    {
      id: "draft",
      type: "llm",
      config: { model: "mock/sim-1", prompt: "Summarize ${{ run.params.topic }}", max_tokens: 64 },
    },
  ],
  edges: [],
};

test("import → edit → save v1/v2 → conflict guard → save anyway → submit", async ({ page, request }) => {
  test.skip(API_KEY === "", "AGENTLOOM_API_KEY must be set for the save/submit e2e");

  await page.goto("/builder");
  await expect(page.getByTestId("builder-canvas")).toBeVisible();

  // Import the definition.
  await page.getByTestId("import-open").click();
  await page.getByTestId("import-textarea").fill(JSON.stringify(DEF));
  await page.getByTestId("import-confirm").click();
  await expect(page.getByTestId("problem-count-ok")).toBeVisible();

  // Edit the prompt (dirties the canvas).
  await page.locator(".react-flow__node").first().click();
  const prompt = page.getByTestId("field-prompt").getByRole("textbox");
  await prompt.click();
  await prompt.press("End");
  await prompt.pressSequentially(" (v1)");
  await expect(page.getByTestId("dirty-indicator")).toBeVisible();

  // Save → creates v1.
  await page.getByTestId("save-open").click();
  await page.getByTestId("save-confirm").click();
  await expect(page.getByTestId("doc-name")).toContainText("v1");
  await expect(page.getByTestId("dirty-indicator")).toHaveCount(0);

  // Edit again → Save → appends v2 (If-Match: 1).
  await prompt.click();
  await prompt.press("End");
  await prompt.pressSequentially(" (v2)");
  await page.getByTestId("save-open").click();
  await page.getByTestId("save-confirm").click();
  await expect(page.getByTestId("doc-name")).toContainText("v2");

  // A concurrent editor appends a version out-of-band → server latest is now v3.
  const appended = await request.post(`${API_URL}/v1/definitions/${NAME}/versions`, {
    headers: { authorization: `Bearer ${API_KEY}`, "content-type": "application/json" },
    data: { definition: DEF },
  });
  expect(appended.ok(), `out-of-band append: ${appended.status()}`).toBeTruthy();

  // Edit + Save → If-Match: 2 no longer matches → the version-conflict guard.
  await prompt.click();
  await prompt.press("End");
  await prompt.pressSequentially(" (stale)");
  await page.getByTestId("save-open").click();
  await page.getByTestId("save-confirm").click();
  await expect(page.getByTestId("save-conflict")).toBeVisible();

  // Save anyway → appends the next version (v4) without the precondition.
  await page.getByTestId("save-force").click();
  await expect(page.getByTestId("doc-name")).toContainText("v4");
  await expect(page.getByTestId("dirty-indicator")).toHaveCount(0);

  // Submit → the params modal requires `topic`; submitting returns a run id.
  await page.getByTestId("submit-run").click();
  await expect(page.getByTestId("submit-dialog")).toBeVisible();
  await page.getByTestId("param-topic").fill("distributed systems");
  await page.getByTestId("submit-confirm").click();
  await expect(page.getByTestId("submit-run-id")).toBeVisible();
  await expect(page.getByTestId("submit-run-id")).toHaveText(
    /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/,
  );
});

test("open in builder from the definitions list loads the stored definition", async ({ page, request }) => {
  test.skip(API_KEY === "", "AGENTLOOM_API_KEY must be set for the open-in-builder e2e");

  const openName = `web_open_${Date.now()}`;
  const created = await request.post(`${API_URL}/v1/definitions`, {
    headers: { authorization: `Bearer ${API_KEY}`, "content-type": "application/json" },
    data: { definition: { ...DEF, name: openName } },
  });
  expect(created.ok(), `create: ${created.status()}`).toBeTruthy();

  await page.goto("/definitions");
  const row = page.locator("tbody tr", { hasText: openName });
  await row.getByTestId("open-in-builder").click();

  await expect(page).toHaveURL(/\/builder\?definition=/);
  await expect(page.getByTestId("doc-name")).toContainText(openName);
  await expect(page.getByTestId("doc-name")).toContainText("v1");
  await expect(page.locator(".react-flow__node").first()).toBeVisible();
});
