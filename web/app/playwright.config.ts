import { defineConfig, devices } from "@playwright/test";

/**
 * Playwright smoke config (ticket 17.1). Runs the built app via `next start` and
 * drives it in Chromium against the compose backend.
 *
 * The app's server env carries the backend key (AGENTLOOM_API_URL /
 * AGENTLOOM_API_KEY); the browser never sees it — the e2e asserts that no
 * browser request carries an Authorization header. `scripts/web-e2e-smoke.sh`
 * boots compose and exports these before invoking `pnpm e2e`.
 */
const PORT = Number(process.env.PLAYWRIGHT_PORT ?? 3100);
const BASE_URL = `http://127.0.0.1:${PORT}`;

export default defineConfig({
  testDir: "./e2e",
  fullyParallel: false,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 1 : 0,
  workers: 1,
  reporter: process.env.CI ? [["list"], ["html", { open: "never" }]] : "list",
  use: {
    baseURL: BASE_URL,
    trace: "on-first-retry",
  },
  projects: [{ name: "chromium", use: { ...devices["Desktop Chrome"] } }],
  webServer: {
    command: `corepack pnpm exec next start -p ${PORT}`,
    url: BASE_URL,
    reuseExistingServer: !process.env.CI,
    timeout: 120_000,
    env: {
      AGENTLOOM_API_URL: process.env.AGENTLOOM_API_URL ?? "http://127.0.0.1:8080",
      AGENTLOOM_API_KEY: process.env.AGENTLOOM_API_KEY ?? "",
      // Browser-reachable API origin for the dashboard's WebSocket (ticket 18.1).
      // The app dials the API's WS directly (a proxy can't forward an upgrade);
      // the key never rides along — a proxy-minted ws-ticket authenticates it.
      AGENTLOOM_API_PUBLIC_URL:
        process.env.AGENTLOOM_API_PUBLIC_URL ?? process.env.AGENTLOOM_API_URL ?? "http://127.0.0.1:8080",
    },
  },
});
