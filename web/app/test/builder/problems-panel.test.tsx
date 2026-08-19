import { beforeEach, describe, expect, it } from "vitest";
import { render, screen, fireEvent, cleanup } from "@testing-library/react";
import { ReactFlowProvider } from "@xyflow/react";
import { toFlow } from "@agentloom/graphdef";
import { emptyDefinition, useBuilderStore } from "@/lib/builder/store";
import { ProblemsProvider } from "@/lib/builder/problems-context";
import { ProblemsPanel } from "@/components/builder/ProblemsPanel";

const store = useBuilderStore;

function renderPanel() {
  return render(
    <ReactFlowProvider>
      <ProblemsProvider>
        <ProblemsPanel />
      </ProblemsProvider>
    </ReactFlowProvider>,
  );
}

beforeEach(() => {
  cleanup();
  store.getState().load(toFlow(emptyDefinition()));
});

describe("ProblemsPanel", () => {
  it("shows the empty state when the workflow validates", () => {
    store.getState().load(toFlow({ schema_version: 1, name: "ok", steps: [{ id: "a", type: "noop" }], edges: [] } as never));
    renderPanel();
    expect(screen.getByTestId("problems-empty")).toBeInTheDocument();
  });

  it("lists a step's config error and focuses the node on click", () => {
    store.getState().addStep("llm"); // llm_1 → model + prompt/messages required
    const id = store.getState().nodes[0]!.id;
    // Deselect so the click's selection change is observable.
    store.getState().selectOnly("edge", "none");
    renderPanel();

    const rows = screen.getAllByTestId("problem-row");
    expect(rows.length).toBeGreaterThan(0);
    // The panel names the offending step.
    expect(rows[0]!).toHaveTextContent("llm_1");

    fireEvent.click(rows[0]!);
    // Click-to-focus selected the node (DoD-3).
    expect(store.getState().nodes.find((n) => n.id === id)!.selected).toBe(true);
  });
});
