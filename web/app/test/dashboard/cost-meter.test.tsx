import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import { CostMeter } from "@/components/dashboard/CostMeter";
import { makeRun } from "./helpers";
import { runDetailFixture } from "./inspector-fixtures";

describe("CostMeter (render)", () => {
  it("renders spend from the run-detail golden without a budget bar", () => {
    render(<CostMeter run={runDetailFixture.run} />);
    const meter = screen.getByTestId("cost-meter");
    expect(meter.getAttribute("data-tier")).toBe("unbudgeted");
    expect(meter.getAttribute("data-spent-nano")).toBe(String(runDetailFixture.run.cost.spent_nano_usd));
    expect(screen.queryByTestId("budget-bar")).toBeNull();
  });

  it("renders a budget bar with a warn tier at 80% and the saved indicator", () => {
    const run = makeRun({
      cost: { ...makeRun().cost, spent_nano_usd: 80, saved_nano_usd: 5, budget_nano_usd: 100 },
    });
    render(<CostMeter run={run} />);
    const meter = screen.getByTestId("cost-meter");
    expect(meter.getAttribute("data-tier")).toBe("warn");
    const bar = screen.getByTestId("budget-bar");
    expect(bar.getAttribute("aria-valuenow")).toBe("80");
    expect(screen.getByTestId("cost-saved")).toBeInTheDocument();
  });

  it("shows 'parked at cap' when parked for budget", () => {
    const run = makeRun({
      status: "parked",
      park_reason: "budget_exceeded",
      cost: { ...makeRun().cost, spent_nano_usd: 100, budget_nano_usd: 100 },
    });
    render(<CostMeter run={run} />);
    expect(screen.getByTestId("cost-meter").getAttribute("data-tier")).toBe("exceeded");
    expect(screen.getByTestId("cost-parked")).toBeInTheDocument();
  });
});
