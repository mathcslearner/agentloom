import { cleanup, render } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import { STEP_TYPES } from "@agentloom/graphdef";
import { StepNodeView } from "@/components/builder/StepNodeView";

afterEach(cleanup);

// DOM snapshots are the deterministic visual-regression baseline (ticket 17.3
// DoD-3): rendering the same neutral component for the same step yields
// byte-stable markup across machines (unlike a screenshot, which varies with
// font rendering). The dashboard (M18) reuses this component with a status skin.
describe("StepNodeView snapshots", () => {
  for (const type of STEP_TYPES) {
    it(`renders ${type} deterministically`, () => {
      const { container } = render(<StepNodeView step={{ id: `${type}_1`, type, config: {} } as never} />);
      expect(container.firstChild).toMatchSnapshot();
    });
  }

  it("renders a selected node", () => {
    const { container } = render(<StepNodeView step={{ id: "llm_1", type: "llm", config: { model: "mock/sim-1" } } as never} selected />);
    expect(container.firstChild).toMatchSnapshot();
  });

  it("renders a status skin (M18 reuse)", () => {
    const { container } = render(
      <StepNodeView
        step={{ id: "llm_1", type: "llm", config: {} } as never}
        skin={{ className: "border-blue-500", badge: <span data-testid="chip">running</span> }}
      />,
    );
    expect(container.querySelector('[data-testid="chip"]')).not.toBeNull();
    expect(container.firstChild).toMatchSnapshot();
  });

  it("renders the dashboard summary + footer slots (M18.2)", () => {
    const { container, getByText } = render(
      <StepNodeView
        step={{ id: "w_1", type: "llm", config: { model: "mock/sim-1" } } as never}
        skin={{
          badge: <span>running</span>,
          summary: <span data-testid="run-summary">attempt 2</span>,
          footer: <span data-testid="provenance">⑂ planner</span>,
        }}
      />,
    );
    // The run summary overrides the config summary; the footer renders.
    expect(container.querySelector('[data-testid="run-summary"]')).not.toBeNull();
    expect(container.querySelector('[data-testid="provenance"]')).not.toBeNull();
    // The config summary ("mock/sim-1") is not shown when a skin summary is set.
    expect(() => getByText("mock/sim-1")).toThrow();
  });
});
