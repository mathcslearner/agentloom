import { describe, expect, it, vi, beforeEach } from "vitest";
import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import type { RunController } from "@/lib/dashboard/run-controller";
import { RaiseBudgetDialog } from "@/components/dashboard/RaiseBudgetDialog";
import { makeRun } from "./helpers";

// Mock the network layer; the dialog drives setRunBudget then (optionally) unparkRun.
const setRunBudget = vi.fn();
const unparkRun = vi.fn();
vi.mock("@/lib/dashboard/streams", () => ({
  setRunBudget: (...a: unknown[]) => setRunBudget(...a),
  unparkRun: (...a: unknown[]) => unparkRun(...a),
}));

const refreshViews = vi.fn(async () => {});
const controller = { refreshViews } as unknown as RunController;

function parkedRun() {
  return makeRun({
    status: "parked",
    park_reason: "budget_exceeded",
    cost: { ...makeRun().cost, spent_nano_usd: 30_000_000, budget_nano_usd: 100_000_000 },
  });
}

beforeEach(() => {
  setRunBudget.mockReset();
  unparkRun.mockReset();
  refreshViews.mockReset();
  setRunBudget.mockResolvedValue({ kind: "ok" });
  unparkRun.mockResolvedValue({ kind: "ok" });
});

describe("RaiseBudgetDialog", () => {
  it("raises then unparks when resume is checked (default while parked)", async () => {
    const onOpenChange = vi.fn();
    render(
      <RaiseBudgetDialog runId="run-1" run={parkedRun()} controller={controller} open onOpenChange={onOpenChange} />,
    );
    // Resume is checked by default for a budget-parked run.
    expect((screen.getByTestId("budget-resume") as HTMLInputElement).checked).toBe(true);
    fireEvent.change(screen.getByTestId("budget-input"), { target: { value: "1" } });
    fireEvent.click(screen.getByTestId("budget-confirm"));
    await waitFor(() => expect(setRunBudget).toHaveBeenCalledWith("run-1", 1));
    expect(unparkRun).toHaveBeenCalledWith("run-1");
    await waitFor(() => expect(onOpenChange).toHaveBeenCalledWith(false));
  });

  it("does not unpark when resume is unchecked", async () => {
    render(<RaiseBudgetDialog runId="run-1" run={parkedRun()} controller={controller} open onOpenChange={vi.fn()} />);
    fireEvent.click(screen.getByTestId("budget-resume")); // uncheck
    fireEvent.change(screen.getByTestId("budget-input"), { target: { value: "2" } });
    fireEvent.click(screen.getByTestId("budget-confirm"));
    await waitFor(() => expect(setRunBudget).toHaveBeenCalledWith("run-1", 2));
    expect(unparkRun).not.toHaveBeenCalled();
  });

  it("surfaces a 409 conflict on the raise and stays open", async () => {
    setRunBudget.mockResolvedValue({ kind: "conflict", message: "run is terminal" });
    const onOpenChange = vi.fn();
    render(
      <RaiseBudgetDialog runId="run-1" run={parkedRun()} controller={controller} open onOpenChange={onOpenChange} />,
    );
    fireEvent.change(screen.getByTestId("budget-input"), { target: { value: "1" } });
    fireEvent.click(screen.getByTestId("budget-confirm"));
    await waitFor(() => expect(screen.getByTestId("budget-error").textContent).toContain("terminal"));
    expect(onOpenChange).not.toHaveBeenCalledWith(false);
  });

  it("a 409 on the unpark (already resumed) is tolerated — the raise still succeeded", async () => {
    unparkRun.mockResolvedValue({ kind: "conflict", message: "run not parked" });
    const onOpenChange = vi.fn();
    render(
      <RaiseBudgetDialog runId="run-1" run={parkedRun()} controller={controller} open onOpenChange={onOpenChange} />,
    );
    fireEvent.change(screen.getByTestId("budget-input"), { target: { value: "1" } });
    fireEvent.click(screen.getByTestId("budget-confirm"));
    await waitFor(() => expect(onOpenChange).toHaveBeenCalledWith(false));
    expect(screen.queryByTestId("budget-error")).toBeNull();
  });

  it("warns when the budget is below current spend", () => {
    render(<RaiseBudgetDialog runId="run-1" run={parkedRun()} controller={controller} open onOpenChange={vi.fn()} />);
    fireEvent.change(screen.getByTestId("budget-input"), { target: { value: "0.01" } });
    expect(screen.getByTestId("budget-below-spend")).toBeInTheDocument();
  });
});
