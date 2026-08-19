"use client";

// The model picker (ticket 17.4). No model list exists in the API (providers
// carry only names), so the picker offers known-good demo models grouped by the
// configured providers (plugins.ts) via a datalist, allows free text, and warns
// — never errors — when a bare model matches no vendor prefix, since that is a
// run-time routing failure the backend does not catch at submit.

import { useId } from "react";
import { Input } from "@/components/ui/input";

// Vendor prefixes the backend's llm.Registry routes bare models by
// (internal/llm/registry.go). A `provider/model` form always routes; a bare
// model must match one of these or it is an UnknownModel at run time.
const VENDOR_PREFIXES = ["claude", "gpt-", "chatgpt-", "o1", "o3", "o4"];

export interface ModelPickerProps {
  value: string;
  onChange: (value: string) => void;
  onFocus?: () => void;
  onBlur?: () => void;
  suggestions: readonly string[];
}

function routable(model: string): boolean {
  if (model === "") return true;
  if (model.includes("/")) return true; // explicit provider/model form
  return VENDOR_PREFIXES.some((p) => model.startsWith(p));
}

export function ModelPicker({ value, onChange, onFocus, onBlur, suggestions }: ModelPickerProps) {
  const listId = useId();
  const warn = !routable(value);
  return (
    <div>
      <Input
        value={value}
        list={listId}
        placeholder="provider/model"
        onFocus={onFocus}
        onBlur={onBlur}
        onChange={(e) => onChange(e.target.value)}
      />
      <datalist id={listId}>
        {suggestions.map((m) => (
          <option key={m} value={m} />
        ))}
      </datalist>
      {warn ? (
        <p className="mt-1 text-[11px] text-amber-600">
          no vendor prefix matched — use a `provider/model` form (e.g. `mock/sim-1`) or it fails at run time
        </p>
      ) : null}
    </div>
  );
}
