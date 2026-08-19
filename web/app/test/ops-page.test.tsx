import { cleanup, render, screen, waitFor, fireEvent } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { Permissions } from "@/lib/pure/dashboard/permissions";
import { controlStateFor, can as canFor } from "@/lib/pure/dashboard/permissions";
import { deadLetterListFixture, systemStatsFixture } from "./dashboard/inspector-fixtures";

const listDeadLetters = vi.fn();
const fetchSystemStats = vi.fn();
const requeueStep = vi.fn();
vi.mock("@/lib/dashboard/streams", () => ({
  listDeadLetters: (...a: unknown[]) => listDeadLetters(...a),
  fetchSystemStats: (...a: unknown[]) => fetchSystemStats(...a),
  requeueStep: (...a: unknown[]) => requeueStep(...a),
}));
vi.mock("next/navigation", () => ({
  useRouter: () => ({ replace: vi.fn(), push: vi.fn() }),
  useSearchParams: () => new URLSearchParams(),
}));
vi.mock("@/lib/dashboard/useDeadLetterList", () => ({ useDeadLetterList: () => "live" }));
vi.mock("@/lib/runtime-config", () => ({ useRuntimeConfig: () => ({ apiPublicUrl: "http://x" }) }));

let perms: Permissions = { status: "ready", scopes: ["submit", "read"] };
vi.mock("@/lib/permissions", () => ({
  usePermissions: () => ({
    ...perms,
    can: (w: Parameters<typeof canFor>[1]) => canFor(perms, w),
    controlState: (c: Parameters<typeof controlStateFor>[1]) => controlStateFor(perms, c),
  }),
}));

import OpsPage from "@/app/(site)/ops/page";

beforeEach(() => {
  perms = { status: "ready", scopes: ["submit", "read"] };
  listDeadLetters.mockResolvedValue(deadLetterListFixture);
  fetchSystemStats.mockResolvedValue(systemStatsFixture);
  requeueStep.mockResolvedValue({ kind: "ok" });
});
afterEach(() => {
  cleanup();
  listDeadLetters.mockReset();
  fetchSystemStats.mockReset();
  requeueStep.mockReset();
});

describe("OpsPage", () => {
  it("renders the queue-health tiles and DLQ rows", async () => {
    render(<OpsPage />);
    await waitFor(() => expect(screen.getAllByTestId("dlq-row").length).toBe(3));
    // Queue-health tiles from the stats fixture.
    await waitFor(() => expect(screen.getByTestId("stat-ready")).toHaveTextContent("42"));
    expect(screen.getByTestId("stat-dead_letters")).toHaveTextContent("2");
  });

  it("shows Requeue for an open row with submit and drives requeueStep", async () => {
    render(<OpsPage />);
    await waitFor(() => expect(screen.getAllByTestId("dlq-row").length).toBe(3));
    const buttons = screen.getAllByTestId("dlq-requeue");
    // Two open rows in the fixture (flaky, publish).
    expect(buttons.length).toBe(2);
    fireEvent.click(buttons[0]!);
    await waitFor(() => expect(requeueStep).toHaveBeenCalledTimes(1));
  });

  it("hides Requeue without the submit scope", async () => {
    perms = { status: "ready", scopes: ["read"] };
    render(<OpsPage />);
    await waitFor(() => expect(screen.getAllByTestId("dlq-row").length).toBe(3));
    expect(screen.queryByTestId("dlq-requeue")).toBeNull();
  });
});
