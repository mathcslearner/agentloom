import { expect, test, type Locator, type Page } from "@playwright/test";

/**
 * 17.5 client-side validation e2e. The validator runs entirely client-side over
 * the definition contract, so these tests drive the canvas only (no backend).
 *
 * DoD-2: the loop-edge authoring flow validates (a marked loop is fine; an
 * unmarked cycle is rejected with a path highlight).
 * DoD-3: the Problems panel focuses the offending node/edge on click.
 */

function nodeBodies(page: Page): Locator {
  return page.locator('.react-flow__node [data-testid="step-node"]');
}

async function dragConnect(page: Page, source: Locator, target: Locator) {
  const s = await source.boundingBox();
  const t = await target.boundingBox();
  if (!s || !t) throw new Error("handle not found");
  await page.mouse.move(s.x + s.width / 2, s.y + s.height / 2);
  await page.mouse.down();
  await page.mouse.move((s.x + t.x) / 2, (s.y + t.y) / 2, { steps: 5 });
  await page.mouse.move(t.x + t.width / 2, t.y + t.height / 2, { steps: 5 });
  await page.mouse.up();
}

async function moveNodeBy(page: Page, node: Locator, dx: number, dy: number) {
  const b = await node.boundingBox();
  if (!b) throw new Error("node not found");
  const x = b.x + b.width / 2;
  const y = b.y + 10;
  await page.mouse.move(x, y);
  await page.mouse.down();
  await page.mouse.move(x + dx, y + dy, { steps: 8 });
  await page.mouse.up();
}

test("Problems panel focuses the offending node on click (DoD-3)", async ({ page }) => {
  await page.goto("/builder");
  await expect(page.getByTestId("builder-canvas")).toBeVisible();

  // Two llm nodes with empty config → both invalid (model + prompt required).
  await page.getByLabel("Add LLM step").click();
  await page.getByLabel("Add LLM step").click();

  const panel = page.getByTestId("problems-panel");
  await expect(panel).toBeVisible();
  const rows = page.getByTestId("problem-row");
  await expect(rows.first()).toBeVisible();
  // The toolbar reports the blocking error count and disables submit.
  await expect(page.getByTestId("problem-count")).toContainText("error");
  await expect(page.getByTestId("submit-run")).toBeDisabled();

  // Click the first problem → its node is selected (the inspector follows).
  const firstRowLabel = (await rows.first().getByTestId("problem-location").textContent())!.trim();
  await rows.first().click();
  await expect(page.getByTestId("inspector-step-id")).toHaveText(firstRowLabel);
  // The focused node is highlighted.
  await expect(page.locator('[data-testid="step-node"][data-highlight="true"]')).toHaveCount(1);
});

test("loop authoring: unmarked cycle rejected, marked loop validates (DoD-2)", async ({ page }) => {
  await page.goto("/builder");
  await page.getByLabel("Add LLM step").click(); // llm_1
  await page.getByLabel("Add LLM step").click(); // llm_2
  const bodies = nodeBodies(page);
  await moveNodeBy(page, bodies.nth(0), -160, -40);
  await moveNodeBy(page, bodies.nth(1), 160, 40);

  // llm_1 → llm_2 (a normal forward edge).
  await dragConnect(page, page.locator('[aria-label="llm_1 next"]'), page.locator('[aria-label="llm_2 input"]'));
  // llm_2 → llm_1 from the OUT port: closes an unmarked cycle → cycle_detected.
  await dragConnect(page, page.locator('[aria-label="llm_2 next"]'), page.locator('[aria-label="llm_1 input"]'));

  const cycleRow = page.getByTestId("problem-row").filter({ hasText: "cycle_detected" });
  await expect(cycleRow).toBeVisible();
  // Clicking the cycle problem selects the closing edge (its own path) and
  // highlights the whole cycle path — both nodes and the edge.
  await cycleRow.click();
  await expect(page.locator('[data-testid="step-node"][data-highlight="true"]')).toHaveCount(2);
  await expect(page.locator('[data-testid="step-edge"][data-highlight="true"]')).toHaveCount(1);

  // The edge inspector opened on the closing edge; mark it a loop with a
  // condition → the cycle error clears (only loop edges may form cycles).
  await expect(page.getByTestId("inspector-edge-id")).toContainText("llm_2 → llm_1");
  await page.getByTestId("edge-loop-toggle").check();
  await page.getByTestId("edge-condition").fill("output.again == true");
  await expect(page.getByTestId("problem-row").filter({ hasText: "cycle_detected" })).toHaveCount(0);
});
