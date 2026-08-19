import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { expect, test } from "@playwright/test";

/**
 * 17.6 import/export & unsaved-guard e2e (client-only — no backend). Imports a
 * corpus fixture through the builder UI, exports the canonical JSON, and asserts
 * it equals the Go-produced canonical golden byte-for-byte (DoD-2 wired through
 * the DOM). Also proves the unsaved-changes guard prompts on navigation.
 */

const REPO_ROOT = fileURLToPath(new URL("../../../", import.meta.url));
const FIXTURE_KEY = "examples/definitions/mock_pipeline.json";
const fixtureText = readFileSync(`${REPO_ROOT}${FIXTURE_KEY}`, "utf8");
const goldenText = readFileSync(`${REPO_ROOT}internal/dag/testdata/canonical.golden.json`, "utf8");
const golden = (JSON.parse(goldenText) as Record<string, string>)[FIXTURE_KEY]!;

test("import a fixture, export canonical JSON byte-for-byte", async ({ page }) => {
  await page.goto("/builder");
  await expect(page.getByTestId("builder-canvas")).toBeVisible();

  // Import via paste.
  await page.getByTestId("import-open").click();
  await page.getByTestId("import-textarea").fill(fixtureText);
  await expect(page.getByTestId("import-summary")).toBeVisible();
  await page.getByTestId("import-confirm").click();

  // The canvas now holds the fixture's steps.
  await expect(page.locator(".react-flow__node").first()).toBeVisible();
  await expect(page.getByTestId("doc-name")).toContainText("(unsaved)");

  // Export → capture the download → compare bytes to the Go canonical golden.
  const [download] = await Promise.all([
    page.waitForEvent("download"),
    page.getByTestId("export").click(),
  ]);
  const path = await download.path();
  const exported = readFileSync(path, "utf8");
  expect(exported).toBe(golden);
});

test("the unsaved-changes guard prompts before navigating away", async ({ page }) => {
  await page.goto("/builder");
  await expect(page.getByTestId("builder-canvas")).toBeVisible();

  // A fresh canvas is clean → no guard.
  await expect(page.getByTestId("dirty-indicator")).toHaveCount(0);

  // Add a step → dirty.
  await page.getByLabel("Add LLM step").click();
  await expect(page.getByTestId("dirty-indicator")).toBeVisible();

  // Clicking a nav link opens the confirm dialog instead of navigating.
  await page.getByRole("link", { name: "Definitions" }).click();
  await expect(page.getByTestId("confirm-dialog")).toBeVisible();
  await expect(page).toHaveURL(/\/builder/);

  // Cancel keeps us on the builder.
  await page.getByTestId("confirm-cancel").click();
  await expect(page.getByTestId("confirm-dialog")).toHaveCount(0);
  await expect(page).toHaveURL(/\/builder/);
});
