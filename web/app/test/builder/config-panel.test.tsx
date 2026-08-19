// The config panel (ticket 17.4, DoD-1): every built-in plugin's config is
// editable via generated forms, and edits round-trip into the canvas store.
import { fireEvent, render, screen, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { STEP_TYPES, fallbackConfigSchemas, toFlow } from "@agentloom/graphdef";
import { planFields } from "@/lib/pure/builder/fields";
import { SchemaForm } from "@/components/builder/config/SchemaForm";
import { emptyCatalog } from "@/lib/pure/builder/plugins";
import { emptyDefinition, useBuilderStore } from "@/lib/builder/store";

// The catalog store fetches through the proxy on mount; stub the browser client
// so the panel uses the offline fallback schemas (no network in tests).
vi.mock("@/lib/api/browser", () => ({
  browserApi: () => ({ GET: async () => ({ error: { error: { code: "x", message: "x" } } }) }),
  PROXY_BASE: "/api/agentloom",
}));

const store = useBuilderStore;
const schemas = fallbackConfigSchemas();
const noop = () => {};

beforeEach(() => {
  store.getState().load(toFlow(emptyDefinition()));
});

describe("SchemaForm renders a control for every field of every executor", () => {
  for (const type of STEP_TYPES) {
    it(`${type}`, () => {
      const entry = schemas[type]!;
      const fields = planFields(entry.schema, { stepType: type, defs: entry.defs });
      render(
        <SchemaForm
          stepType={type}
          fields={fields}
          value={{}}
          catalog={emptyCatalog()}
          autocomplete={{ nodes: [], edges: [], doc: {}, currentStepId: `${type}_1` }}
          onChange={noop}
        />,
      );
      // Every non-hidden field renders a row (data-testid=field-<name>).
      for (const f of fields.filter((x) => x.hint !== "hidden")) {
        expect(screen.getByTestId(`field-${f.name}`)).toBeInTheDocument();
      }
    });
  }
});

describe("edits round-trip into the store", () => {
  it("model and prompt reach the selected step's config", () => {
    const type = "llm" as const;
    const entry = schemas[type]!;
    const fields = planFields(entry.schema, { stepType: type, defs: entry.defs });

    let value: Record<string, unknown> = {};
    const onChange = (v: Record<string, unknown>) => {
      value = v;
    };
    const { rerender } = render(
      <SchemaForm
        stepType={type}
        fields={fields}
        value={value}
        catalog={emptyCatalog()}
        autocomplete={{ nodes: [], edges: [], doc: {}, currentStepId: "llm_1" }}
        onChange={onChange}
      />,
    );

    const modelRow = screen.getByTestId("field-model");
    fireEvent.change(within(modelRow).getByPlaceholderText("provider/model"), { target: { value: "mock/sim-1" } });
    expect(value.model).toBe("mock/sim-1");

    rerender(
      <SchemaForm
        stepType={type}
        fields={fields}
        value={value}
        catalog={emptyCatalog()}
        autocomplete={{ nodes: [], edges: [], doc: {}, currentStepId: "llm_1" }}
        onChange={onChange}
      />,
    );
    const promptRow = screen.getByTestId("field-prompt");
    fireEvent.change(within(promptRow).getByRole("textbox"), { target: { value: "hello" } });
    expect(value.prompt).toBe("hello");
  });
});
