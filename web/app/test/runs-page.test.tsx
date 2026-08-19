import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

// Mock the browser client so the page renders against canned data (no network).
const get = vi.fn();
vi.mock("@/lib/api/browser", () => ({
  PROXY_BASE: "/api/agentloom",
  browserApi: () => ({ GET: get }),
}));

import RunsPage from "@/app/runs/page";

afterEach(() => {
  cleanup();
  get.mockReset();
});

describe("RunsPage", () => {
  it("renders runs returned through the proxy client", async () => {
    get.mockResolvedValue({
      data: {
        runs: [
          {
            id: "run-abc",
            status: "succeeded",
            on_failure: "fail_fast",
            steps_total: 3,
            steps_succeeded: 3,
            steps_failed: 0,
            steps_skipped: 0,
            steps_cancelled: 0,
            steps_collected: 0,
            created_at: "2026-08-18T00:00:00Z",
            cost: {},
          },
        ],
      },
    });

    render(<RunsPage />);
    await waitFor(() => expect(screen.getByText("run-abc")).toBeInTheDocument());
    expect(screen.getByText("succeeded", { selector: "span" })).toBeInTheDocument();
    expect(get).toHaveBeenCalledWith("/v1/runs", { params: { query: { limit: 25 } } });
  });

  it("shows an empty-state message when no runs match", async () => {
    get.mockResolvedValue({ data: { runs: [] } });
    render(<RunsPage />);
    await waitFor(() => expect(screen.getByText(/No runs match this filter/)).toBeInTheDocument());
  });

  it("surfaces the error envelope message on failure", async () => {
    get.mockResolvedValue({ error: { error: { code: "rate_limited", message: "slow down" } } });
    render(<RunsPage />);
    await waitFor(() => expect(screen.getByText("slow down")).toBeInTheDocument());
  });
});
