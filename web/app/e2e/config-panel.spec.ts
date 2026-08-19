import { expect, test, type Page } from "@playwright/test";

/**
 * 17.4 config-panel e2e. The panel renders forms from the plugin schemas (or the
 * offline fallback when the catalog is unavailable), marks invalid nodes, and
 * offers upstream-only `${{ }}` autocomplete. These tests drive the canvas only —
 * the config validation runs client-side against the published schema, so no
 * backend is required (the fallback schemas validate executors offline).
 */

async function addLLM(page: Page) {
  await page.getByLabel("Add LLM step").click();
}

test("a fresh llm node is marked invalid until required config is filled", async ({ page }) => {
  await page.goto("/builder");
  await expect(page.getByTestId("builder-canvas")).toBeVisible();

  await addLLM(page); // llm_1, empty config → model + prompt/messages required
  const node = page.locator('.react-flow__node [data-testid="step-node"]').first();
  await expect(node).toHaveAttribute("data-invalid", "true");
  await expect(page.getByTestId("problem-count")).toContainText("error");

  // Select the node → the config panel opens with the required markers.
  await node.click();
  await expect(page.getByTestId("inspector-step-id")).toHaveText("llm_1");
  await expect(page.getByTestId("step-problem-count")).toBeVisible();

  // Fill the model and a prompt.
  await page.getByTestId("field-model").getByPlaceholder("provider/model").fill("mock/sim-1");
  await page.getByTestId("field-prompt").getByRole("textbox").fill("Say hi");

  // The node clears its invalid mark.
  await expect(node).toHaveAttribute("data-invalid", "false");
  await expect(page.getByTestId("problem-count")).toHaveCount(0);
  // The serialized definition carries the edited config.
  await expect(page.getByTestId("definition-preview")).toContainText('"model": "mock/sim-1"');
});

test("the prompt editor surfaces the `${{ }}` autocomplete", async ({ page }) => {
  // The upstream-only precision (ancestors over normal edges, self/loop
  // excluded) is proven exhaustively in the vitest suite over a branching
  // graph; here we assert the editor wires the autocomplete end-to-end.
  await page.goto("/builder");
  await addLLM(page); // llm_1
  await page.locator('[data-testid="step-node"]').first().click();

  const prompt = page.getByTestId("field-prompt").getByRole("textbox");
  await prompt.click();
  await prompt.fill("${{ ");

  const list = page.getByTestId("autocomplete");
  await expect(list).toBeVisible();
  const roots = list.locator("[data-suggestion]");
  await expect(roots).toHaveCount(2); // steps. and run.params.
  await expect(list).toContainText("steps");
  await expect(list).toContainText("run.params");
});
