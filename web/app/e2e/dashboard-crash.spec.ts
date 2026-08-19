import { execSync } from "node:child_process";
import { expect, test, type APIRequestContext } from "@playwright/test";

/**
 * 18.2 DoD-3 — the crash-recovery demo, visible on the live DAG: kill the worker
 * holding a running step's lease and watch the node recover (reclaimed → a fresh
 * attempt runs on another worker) without a page refresh.
 *
 * This test SIGKILLs a compose worker container, so it is gated behind
 * AGENTLOOM_E2E_CRASH=1 — a normal `pnpm e2e` run never touches the fleet. It
 * restores the fleet afterward. Enable it with:
 *   AGENTLOOM_E2E_CRASH=1 pnpm e2e dashboard-crash
 */
const API_URL = process.env.AGENTLOOM_API_URL ?? "http://127.0.0.1:8080";
const API_KEY = process.env.AGENTLOOM_API_KEY ?? "";
const QUEUE_STREAM = process.env.AGENTLOOM_QUEUE_STREAM ?? "steps:ready";
const QUEUE_GROUP = process.env.AGENTLOOM_QUEUE_GROUP ?? "workers";

test.describe(() => {
  test.skip(process.env.AGENTLOOM_E2E_CRASH !== "1", "destructive: set AGENTLOOM_E2E_CRASH=1 to run");
  test.setTimeout(150_000);

  async function submitSleep(request: APIRequestContext, name: string): Promise<string> {
    const res = await request.post(`${API_URL}/v1/runs`, {
      headers: { authorization: `Bearer ${API_KEY}`, "content-type": "application/json" },
      data: {
        definition: {
          schema_version: 1,
          name,
          steps: [{ id: "long_task", type: "sleep", config: { duration: "55s" } }],
          edges: [],
        },
      },
    });
    expect(res.ok(), `submit: ${res.status()}`).toBeTruthy();
    return (await res.json()).run_id as string;
  }

  // The consumer holding the sole pending entry (the demo assumes it is our run).
  function leaseHolder(): string {
    const out = execSync(`docker compose exec -T redis redis-cli --no-raw XPENDING ${QUEUE_STREAM} ${QUEUE_GROUP} - + 10`, {
      encoding: "utf8",
    });
    const m = out.match(/^\s*2\)\s+"(.*)"$/m);
    return m ? m[1]! : "";
  }

  function victimContainer(consumer: string): string {
    const ids = execSync("docker compose ps -q worker", { encoding: "utf8" }).trim().split(/\s+/).filter(Boolean);
    for (const cid of ids) {
      const logs = execSync(`docker logs ${cid} 2>&1 || true`, { encoding: "utf8" });
      if (logs.includes(consumer)) return cid;
    }
    return "";
  }

  test.afterEach(() => {
    // Restore the fleet regardless of outcome.
    execSync("docker compose --profile app up -d --wait", { stdio: "ignore" });
  });

  test("killing the lease holder recovers the node live (reclaim → new attempt)", async ({ page, request }) => {
    expect(API_KEY).not.toBe("");
    const runId = await submitSleep(request, `m182_crash_${Date.now()}`);
    await page.goto(`/runs/${runId}`);

    const node = page.locator('[data-testid="run-node"][data-step-id="long_task"]');
    await expect(node).toHaveAttribute("data-step-status", "running", { timeout: 20_000 });

    // Find and SIGKILL the worker holding the lease.
    let holder = "";
    for (let i = 0; i < 40 && !holder; i++) {
      holder = leaseHolder();
      if (!holder) await page.waitForTimeout(500);
    }
    expect(holder, "a lease holder should appear in the PEL").not.toBe("");
    const victim = victimContainer(holder);
    expect(victim, `a worker container should carry consumer ${holder}`).not.toBe("");
    execSync(`docker kill -s KILL ${victim}`, { stdio: "ignore" });

    // After the lease expires the survivor reclaims: the node shows a reclaim and
    // a fresh attempt, all without a page refresh.
    await expect(node).toHaveAttribute("data-reclaims", "1", { timeout: 90_000 });
    await expect(node).toHaveAttribute("data-attempt", "2");
    await expect(node).toHaveAttribute("data-step-status", "running");

    // …and the run completes on the survivor.
    await expect(page.getByTestId("run-detail")).toHaveAttribute("data-run-status", "succeeded", { timeout: 90_000 });
  });
});
