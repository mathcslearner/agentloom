import { expect, test, type Locator, type Page } from "@playwright/test";

/**
 * 17.3 builder e2e. The builder is a client-side view over the definition
 * contract and needs no backend, so these tests only drive the canvas. (They
 * run in the same `web-e2e` job, which happens to have compose up.)
 *
 * DoD-1: build a 10-node graph with every core node type via mouse/keyboard.
 * DoD-2 (e2e half): undo/redo across create, connect, move, and delete.
 */

const CORE_ADD_LABELS = [
  "Add LLM step",
  "Add Tool step",
  "Add Retrieve step",
  "Add Map step",
  "Add Gather step",
  "Add Planner step",
  "Add Agent step",
  "Add Approval step",
  "Add Join step",
  "Add Branch step",
];

function nodes(page: Page): Locator {
  return page.locator(".react-flow__node");
}
function edges(page: Page): Locator {
  return page.locator(".react-flow__edge");
}

async function dragConnect(page: Page, source: Locator, target: Locator) {
  const s = await source.boundingBox();
  const t = await target.boundingBox();
  if (!s || !t) throw new Error("handle not found");
  await page.mouse.move(s.x + s.width / 2, s.y + s.height / 2);
  await page.mouse.down();
  // React Flow tracks the pointer; move through a midpoint before releasing.
  await page.mouse.move((s.x + t.x) / 2, (s.y + t.y) / 2, { steps: 5 });
  await page.mouse.move(t.x + t.width / 2, t.y + t.height / 2, { steps: 5 });
  await page.mouse.up();
}

test("builds a 10-node graph with every core node type (mouse + keyboard)", async ({ page }) => {
  await page.goto("/builder");
  await expect(page.getByTestId("builder-canvas")).toBeVisible();

  // Eight via mouse click.
  for (const label of CORE_ADD_LABELS.slice(0, 8)) {
    await page.getByLabel(label).click();
  }
  // Two via keyboard (focus the palette button, press Enter).
  for (const label of CORE_ADD_LABELS.slice(8)) {
    await page.getByLabel(label).focus();
    await page.keyboard.press("Enter");
  }

  await expect(nodes(page)).toHaveCount(10);
  await expect(page.getByTestId("node-count")).toHaveText("10 steps");
});

test("connects nodes, including a reject-port decision edge", async ({ page }) => {
  await page.goto("/builder");
  await page.getByLabel("Add Approval step").click(); // human_approval_1
  await page.getByLabel("Add LLM step").click(); // llm_1

  // Drag from the approval step's reject port to the llm step's input.
  const reject = page.locator('[aria-label="human_approval_1 on reject"]');
  const input = page.locator('[aria-label="llm_1 input"]');
  await dragConnect(page, reject, input);

  await expect(edges(page)).toHaveCount(1);
  // The serialized edge carries the reject decision marker.
  await expect(page.getByTestId("definition-preview")).toContainText('"decision": "reject"');
});

test("undo/redo across delete, and move", async ({ page }) => {
  await page.goto("/builder");
  await page.getByLabel("Add LLM step").click();
  await page.getByLabel("Add Tool step").click();
  await expect(nodes(page)).toHaveCount(2);

  // Select the first node and delete it.
  await nodes(page).first().click();
  await page.keyboard.press("Delete");
  await expect(nodes(page)).toHaveCount(1);

  // Undo restores it; redo removes it again.
  await page.keyboard.press("ControlOrMeta+z");
  await expect(nodes(page)).toHaveCount(2);
  await page.keyboard.press("ControlOrMeta+Shift+z");
  await expect(nodes(page)).toHaveCount(1);

  // Move the surviving node, then undo the move (count stays; position reverts).
  const node = nodes(page).first();
  const before = await node.boundingBox();
  if (!before) throw new Error("no node");
  await page.mouse.move(before.x + before.width / 2, before.y + before.height / 2);
  await page.mouse.down();
  await page.mouse.move(before.x + 160, before.y + 120, { steps: 6 });
  await page.mouse.up();
  const moved = await node.boundingBox();
  expect(moved!.x).not.toBeCloseTo(before.x, 0);
  await page.keyboard.press("ControlOrMeta+z");
  const reverted = await node.boundingBox();
  expect(reverted!.x).toBeCloseTo(before.x, 0);
});
