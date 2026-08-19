import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { toFlow } from "@agentloom/graphdef";
import { CORE_TYPES, stepMeta } from "@/lib/pure/builder/catalog";
import { emptyDefinition, useBuilderStore } from "@/lib/builder/store";
import { Palette } from "@/components/builder/Palette";

afterEach(cleanup);
beforeEach(() => useBuilderStore.getState().load(toFlow(emptyDefinition())));

describe("Palette", () => {
  it("lists every core step type with an accessible add label", () => {
    render(<Palette />);
    for (const t of CORE_TYPES) {
      expect(screen.getByLabelText(`Add ${stepMeta(t).label} step`)).toBeInTheDocument();
    }
  });

  it("clicking an item adds a step to the store", () => {
    render(<Palette />);
    fireEvent.click(screen.getByLabelText("Add LLM step"));
    expect(useBuilderStore.getState().nodes.map((n) => n.type)).toEqual(["llm"]);
  });

  it("test steps are hidden until the group is expanded", () => {
    render(<Palette />);
    expect(screen.queryByLabelText("Add Echo step")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: /test steps/i }));
    expect(screen.getByLabelText("Add Echo step")).toBeInTheDocument();
  });
});
