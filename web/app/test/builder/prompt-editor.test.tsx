// Prompt editor autocomplete (ticket 17.4, DoD-3): the popover offers exactly
// the upstream steps' refs — a wrong-direction reference is not offered.
import { useState } from "react";
import { fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { PromptEditor } from "@/components/builder/config/PromptEditor";
import type { RefEdge, RefNode } from "@/lib/pure/builder/refs";

// a → b, a → c, b → d ; d is the editing step.
const nodes: RefNode[] = [
  { id: "a", type: "llm" },
  { id: "b", type: "retrieve" },
  { id: "c", type: "llm" },
  { id: "d", type: "llm" },
];
const edges: RefEdge[] = [
  { from: "a", to: "b", loop: false },
  { from: "a", to: "c", loop: false },
  { from: "b", to: "d", loop: false },
];

function renderEditor(initial = "") {
  const onChange = vi.fn();
  function Harness() {
    const [value, setValue] = useState(initial);
    return (
      <PromptEditor
        value={value}
        onChange={(v) => {
          onChange(v);
          setValue(v);
        }}
        nodes={nodes}
        edges={edges}
        doc={{ params: { topic: { type: "string" } } }}
        currentStepId="d"
      />
    );
  }
  const util = render(<Harness />);
  return { util, onChange };
}

describe("PromptEditor autocomplete", () => {
  it("offers exactly the upstream steps after `${{ steps.`", () => {
    renderEditor();
    const box = screen.getByRole("textbox");
    fireEvent.focus(box);
    fireEvent.change(box, { target: { value: "${{ steps." } });

    const list = screen.getByTestId("autocomplete");
    const labels = Array.from(list.querySelectorAll("[data-suggestion]")).map((el) => el.getAttribute("data-suggestion"));
    // d's upstream is {a, b}; c is a sibling and must not appear.
    expect(labels).toEqual(["steps.a.output", "steps.b.output"]);
  });

  it("offers a step's output paths and inserts the ref on click", () => {
    const { onChange } = renderEditor();
    const box = screen.getByRole("textbox");
    fireEvent.focus(box);
    fireEvent.change(box, { target: { value: "${{ steps.b.output." } });

    const list = screen.getByTestId("autocomplete");
    const first = list.querySelector('[data-suggestion="steps.b.output.results"]');
    expect(first).not.toBeNull();
    fireEvent.mouseDown(first!);
    expect(onChange).toHaveBeenLastCalledWith("${{ steps.b.output.results");
  });

  it("offers declared run params", () => {
    renderEditor();
    const box = screen.getByRole("textbox");
    fireEvent.focus(box);
    fireEvent.change(box, { target: { value: "${{ run.params." } });
    const list = screen.getByTestId("autocomplete");
    const labels = Array.from(list.querySelectorAll("[data-suggestion]")).map((el) => el.getAttribute("data-suggestion"));
    expect(labels).toEqual(["run.params.topic"]);
  });
});
