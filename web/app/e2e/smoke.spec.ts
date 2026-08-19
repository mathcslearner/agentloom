import { expect, test } from "@playwright/test";

/**
 * 17.1 smoke: the app lists definitions and runs from the compose backend
 * through the typed client, and the API key never reaches the browser.
 *
 * Seeding talks to the backend directly (server-side, with the key from env);
 * the page assertions go through the app + its same-origin proxy.
 */
const API_URL = process.env.AGENTLOOM_API_URL ?? "http://127.0.0.1:8080";
const API_KEY = process.env.AGENTLOOM_API_KEY ?? "";

const DEF_NAME = `web_smoke_${Date.now()}`;
const DEFINITION = {
  schema_version: 1,
  name: DEF_NAME,
  steps: [{ id: "a", type: "echo", config: { input: { note: "hello from the web smoke" } } }],
  edges: [],
};

test.beforeAll(async ({ request }) => {
  expect(API_KEY, "AGENTLOOM_API_KEY must be set for the e2e smoke").not.toBe("");

  // Register a definition and submit a run so both list pages have a row.
  const created = await request.post(`${API_URL}/v1/definitions`, {
    headers: { authorization: `Bearer ${API_KEY}`, "content-type": "application/json" },
    data: { definition: DEFINITION },
  });
  expect(created.ok(), `create definition: ${created.status()}`).toBeTruthy();

  const submitted = await request.post(`${API_URL}/v1/runs`, {
    headers: { authorization: `Bearer ${API_KEY}`, "content-type": "application/json" },
    data: { definition: DEFINITION },
  });
  expect(submitted.ok(), `submit run: ${submitted.status()}`).toBeTruthy();
});

test("home shows the backend as reachable", async ({ page }) => {
  await page.goto("/");
  await expect(page.getByText("Backend")).toBeVisible();
  await expect(page.getByText("ok", { exact: true })).toBeVisible();
});

test("definitions page lists the seeded definition", async ({ page }) => {
  await page.goto("/definitions");
  await expect(page.getByRole("heading", { name: "Definitions" })).toBeVisible();
  await expect(page.getByText(DEF_NAME)).toBeVisible();
});

test("runs page lists runs through the proxy, and no browser request carries a key", async ({ page }) => {
  const authHeaders: string[] = [];
  page.on("request", (req) => {
    const auth = req.headers()["authorization"];
    if (auth) authHeaders.push(`${req.url()} ${auth}`);
  });

  await page.goto("/runs");
  await expect(page.getByRole("heading", { name: "Runs" })).toBeVisible();
  // At least one run row (the seeded run) renders through the proxy client.
  await expect(page.locator("tbody tr").first()).toBeVisible();

  // The key must never leave the server: no browser request has an Authorization header.
  expect(authHeaders, `browser requests carried Authorization:\n${authHeaders.join("\n")}`).toEqual([]);
});
