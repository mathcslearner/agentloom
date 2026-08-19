import { describe, expect, it, vi } from "vitest";
import { render, screen, fireEvent } from "@testing-library/react";
import { BudgetBanners } from "@/components/dashboard/BudgetBanners";
import { makeEnv, makeRun } from "./helpers";

describe("BudgetBanners (render)", () => {
  it("renders a downgrade banner with from/to/trigger data attrs", () => {
    render(
      <BudgetBanners
        events={[
          makeEnv("model_downgraded", 1, {
            step_id: "draft",
            attempt: 1,
            from_model: "mock/sim-1",
            to_model: "mock/cheap",
            trigger: "budget_threshold",
            threshold_fraction: 0.5,
          }),
        ]}
        run={makeRun()}
        onRaise={vi.fn()}
      />,
    );
    const b = screen.getByTestId("banner-downgrade");
    expect(b.getAttribute("data-from")).toBe("mock/sim-1");
    expect(b.getAttribute("data-to")).toBe("mock/cheap");
    expect(b.getAttribute("data-trigger")).toBe("budget_threshold");
  });

  it("the parked-for-budget banner carries a Raise action and is dismissible", () => {
    const onRaise = vi.fn();
    render(
      <BudgetBanners
        events={[]}
        run={makeRun({
          status: "parked",
          park_reason: "budget_exceeded",
          cost: { ...makeRun().cost, spent_nano_usd: 50, budget_nano_usd: 100 },
        })}
        onRaise={onRaise}
      />,
    );
    fireEvent.click(screen.getByTestId("banner-raise"));
    expect(onRaise).toHaveBeenCalled();
  });

  it("renders nothing when there are no budget events and the run is not parked for budget", () => {
    const { container } = render(<BudgetBanners events={[]} run={makeRun()} onRaise={vi.fn()} />);
    expect(container.firstChild).toBeNull();
  });
});
