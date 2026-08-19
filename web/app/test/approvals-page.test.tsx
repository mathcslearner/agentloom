import { cleanup, render, screen, waitFor, fireEvent } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

const get = vi.fn();
vi.mock("@/lib/api/browser", () => ({
  PROXY_BASE: "/api/agentloom",
  browserApi: () => ({ GET: get, POST: vi.fn() }),
}));
const replace = vi.fn();
vi.mock("next/navigation", () => ({
  useRouter: () => ({ replace, push: vi.fn() }),
  useSearchParams: () => new URLSearchParams(),
}));
vi.mock("@/lib/dashboard/useApprovalInbox", () => ({
  useApprovalInbox: () => "live",
}));

import ApprovalsPage from "@/app/(site)/approvals/page";

afterEach(() => {
  cleanup();
  get.mockReset();
  replace.mockReset();
});

const pendingRow = {
  id: "ap-1",
  run_id: "run-1",
  step_id: "gate",
  attempt: 1,
  status: "pending",
  title: "Publish the article?",
  allowed_decisions: ["approve", "reject"],
  allow_edit: true,
  created_at: "2026-08-18T09:00:00Z",
};

describe("ApprovalsPage", () => {
  it("lists pending approvals through the proxy and defaults to status=pending", async () => {
    get.mockResolvedValue({ data: { approvals: [pendingRow] } });
    render(<ApprovalsPage />);
    await waitFor(() => expect(screen.getByTestId("approval-row")).toBeInTheDocument());
    expect(screen.getByTestId("approval-row")).toHaveAttribute("data-approval-id", "ap-1");
    // The default query filters to pending, limit 50.
    expect(get).toHaveBeenCalledWith("/v1/approvals", { params: { query: { limit: 50, status: "pending" } } });
    // A pending row offers a Decide action.
    expect(screen.getByTestId("row-decide")).toBeInTheDocument();
  });

  it("renders an empty state when there are none", async () => {
    get.mockResolvedValue({ data: { approvals: [] } });
    render(<ApprovalsPage />);
    await waitFor(() => expect(screen.getByTestId("inbox-empty")).toBeInTheDocument());
  });

  it("switching to 'all' clears the status filter in the URL", async () => {
    get.mockResolvedValue({ data: { approvals: [] } });
    render(<ApprovalsPage />);
    await waitFor(() => expect(screen.getByTestId("filter-all")).toBeInTheDocument());
    fireEvent.click(screen.getByTestId("filter-all"));
    expect(replace).toHaveBeenCalledWith("/approvals?status=all");
  });
});
