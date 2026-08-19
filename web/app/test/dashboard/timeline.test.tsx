import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import { Timeline } from "@/components/dashboard/Timeline";
import { makeEnv } from "./helpers";

afterEach(cleanup);

describe("Timeline", () => {
  const events = [
    makeEnv("run_created", 1, { name: "wf", steps_total: 2 }),
    makeEnv("step_succeeded", 2, { attempt: 1 }, "a"),
    makeEnv("cost_updated", 3, { run_spent_nano_usd: 1000, cost_nano_usd: 1000 }, "a"),
  ];

  it("renders every event row with its seq", () => {
    render(<Timeline events={events} />);
    const rows = screen.getAllByTestId("timeline-row");
    expect(rows.map((r) => r.getAttribute("data-seq"))).toEqual(["1", "2", "3"]);
  });

  it("filters by category", () => {
    render(<Timeline events={events} />);
    fireEvent.click(screen.getByTestId("timeline-filter-cost"));
    const rows = screen.getAllByTestId("timeline-row");
    expect(rows).toHaveLength(1);
    expect(rows[0]!.getAttribute("data-type")).toBe("cost_updated");
  });

  it("collapses and expands", () => {
    render(<Timeline events={events} />);
    expect(screen.getByTestId("timeline-list")).toBeInTheDocument();
    fireEvent.click(screen.getByTestId("timeline-toggle"));
    expect(screen.queryByTestId("timeline-list")).not.toBeInTheDocument();
  });
});
