import { describe, expect, it } from "vitest";
import { render, screen, fireEvent, within } from "@testing-library/react";
import { InspectorPane } from "@/components/dashboard/InspectorPane";
import type { RunController } from "@/lib/dashboard/run-controller";
import type { StepState } from "@/lib/pure/dashboard/run-state";
import { fixtureStep, runCostFixture } from "./inspector-fixtures";

// A no-op controller stand-in: the tabs render from props; refreshViews is a
// no-op here (the refresh path is unit-tested on the controller directly).
const controller = { refreshViews: async () => {} } as unknown as RunController;

function stepState(id: string, status = "succeeded"): StepState {
  const view = fixtureStep(id);
  return { id, type: view.type, status: status as StepState["status"], attempt: view.attempt_count, reclaims: 0, view, viewSeq: 20 };
}

describe("InspectorPane tabs (render from the Go golden fixture — DoD-1)", () => {
  it("Overview shows idempotency key, model chain, and attempt/claim history", () => {
    render(<InspectorPane runId="run-1" step={stepState("draft")} events={[]} cost={runCostFixture} controller={controller} />);
    expect(screen.getByTestId("inspector-overview")).toBeInTheDocument();
    expect(screen.getByTestId("idempotency-key").textContent).toMatch(/[0-9a-f-]{36}/);
    // draft is llm ⇒ a model section renders.
    expect(screen.getByTestId("inspector-model")).toBeInTheDocument();
    expect(screen.getByTestId("attempt-timeline")).toBeInTheDocument();
  });

  it("Output tab renders the JSON viewer over the step output", () => {
    render(<InspectorPane runId="run-1" step={stepState("draft")} events={[]} cost={runCostFixture} controller={controller} />);
    fireEvent.click(screen.getByTestId("inspector-tab-output"));
    expect(screen.getByTestId("inspector-output")).toBeInTheDocument();
    expect(screen.getByTestId("json-viewer")).toBeInTheDocument();
  });

  it("Validation tab shows fail→pass verdicts and the prompt diff (DoD-2)", () => {
    render(<InspectorPane runId="run-1" step={stepState("draft")} events={[]} cost={runCostFixture} controller={controller} />);
    fireEvent.click(screen.getByTestId("inspector-tab-validation"));
    const list = screen.getByTestId("verdict-list");
    const statuses = within(list)
      .getAllByRole("listitem")
      .map((li) => li.getAttribute("data-status"))
      .filter((s): s is string => s !== null);
    expect(statuses).toEqual(["fail", "pass"]);
    const diff = screen.getByTestId("prompt-diff");
    // The killer demo: the diff between attempts is pure additions.
    expect(Number(diff.getAttribute("data-added-lines"))).toBeGreaterThan(0);
    expect(diff.getAttribute("data-deleted-lines")).toBe("0");
  });

  it("Cost tab lists the productive + overhead ledger rows", () => {
    render(<InspectorPane runId="run-1" step={stepState("draft")} events={[]} cost={runCostFixture} controller={controller} />);
    fireEvent.click(screen.getByTestId("inspector-tab-cost"));
    const rows = within(screen.getByTestId("cost-rows")).getAllByRole("row");
    // header + 3 entries (attempt 1, attempt 2, judge:0).
    expect(rows.length).toBe(4);
    expect(screen.getByTestId("cost-rows").textContent).toContain("judge:0");
  });

  it("the reclaimed step's claim history names both workers (DoD-3)", () => {
    render(<InspectorPane runId="run-1" step={stepState("crunch")} events={[]} cost={runCostFixture} controller={controller} />);
    const table = screen.getByTestId("claim-history");
    const workers = within(table)
      .getAllByRole("row")
      .map((r) => r.getAttribute("data-worker"))
      .filter(Boolean);
    expect(workers).toContain("worker-alpha");
    expect(workers).toContain("worker-bravo");
  });
});
