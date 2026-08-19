"use client";

// The schema-driven config form (ticket 17.4). Renders an ordered set of
// FieldPlans (fields.ts) into widgets — no per-plugin hardcoding: every built-in
// executor's config form comes from its JSON Schema plus the small hint layer.
// Widget selection lives in fields.ts; this component only maps a FieldPlan to a
// control and threads the value in and out.

import { Fragment } from "react";
import type { StepType } from "@agentloom/graphdef";
import type { FieldPlan } from "@/lib/pure/builder/fields";
import { modelSuggestions, retrieverNames, toolNames, type PluginCatalog } from "@/lib/pure/builder/plugins";
import type { RefEdge, RefNode } from "@/lib/pure/builder/refs";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select } from "@/components/ui/select";
import { Checkbox } from "@/components/ui/checkbox";
import { Textarea } from "@/components/ui/textarea";
import { JsonEditor } from "./JsonEditor";
import { PromptEditor } from "./PromptEditor";
import { ModelPicker } from "./ModelPicker";
import { Picker } from "./Picker";

export interface AutocompleteContext {
  nodes: readonly RefNode[];
  edges: readonly RefEdge[];
  doc: Record<string, unknown>;
  currentStepId: string;
}

export interface SchemaFormProps {
  stepType: StepType;
  fields: FieldPlan[];
  value: Record<string, unknown>;
  onChange: (value: Record<string, unknown>) => void;
  catalog: PluginCatalog;
  autocomplete: AutocompleteContext;
  /** Begin/end a coalesced edit (an input's focus/blur) for one undo entry. */
  beginEdit?: () => void;
  endEdit?: () => void;
}

// Merge a field value into an object, deleting the key when the value is empty
// (undefined or "") so a config stays minimal and round-trips cleanly.
function setField(obj: Record<string, unknown>, name: string, value: unknown): Record<string, unknown> {
  const next = { ...obj };
  if (value === undefined || value === "") delete next[name];
  else next[name] = value;
  return next;
}

// Names for a picker widget, chosen by the field's hint.
function pickerOptions(field: FieldPlan, catalog: PluginCatalog, doc: Record<string, unknown>): string[] {
  switch (field.hint) {
    case "tool":
      return toolNames(catalog);
    case "retriever":
      return retrieverNames(catalog);
    case "agent":
      return keysOf(doc["agents"]);
    case "template":
      return keysOf(doc["templates"]);
    default:
      return [];
  }
}

function keysOf(v: unknown): string[] {
  return typeof v === "object" && v !== null && !Array.isArray(v) ? Object.keys(v as Record<string, unknown>).sort() : [];
}

export function SchemaForm(props: SchemaFormProps) {
  const { fields, value, onChange } = props;
  const set = (name: string, v: unknown) => onChange(setField(value, name, v));

  return (
    <div className="flex flex-col gap-3">
      {fields
        .filter((f) => f.hint !== "hidden")
        .map((f) => (
          <FieldRow key={f.name} field={f} props={props} set={set} />
        ))}
    </div>
  );
}

function FieldRow({ field, props, set }: { field: FieldPlan; props: SchemaFormProps; set: (name: string, v: unknown) => void }) {
  const { value, catalog, autocomplete, beginEdit, endEdit } = props;
  const raw = value[field.name];
  const asString = typeof raw === "string" ? raw : "";
  const control = renderControl();

  return (
    <label className="flex flex-col gap-1" data-testid={`field-${field.name}`}>
      <span className="flex items-center gap-1">
        <Label>{field.label}</Label>
        {field.required ? <span className="text-destructive">*</span> : null}
        <span className="ml-auto text-[10px] font-mono text-muted-foreground">{field.name}</span>
      </span>
      {control}
    </label>
  );

  function renderControl() {
    switch (field.widget) {
      case "model":
        return (
          <ModelPicker
            value={asString}
            suggestions={modelSuggestions(catalog)}
            onFocus={beginEdit}
            onBlur={endEdit}
            onChange={(v) => set(field.name, v)}
          />
        );
      case "picker":
        return (
          <Picker
            value={asString}
            options={pickerOptions(field, catalog, autocomplete.doc)}
            warnUnknown={field.hint === "tool" || field.hint === "retriever" || field.hint === "agent" || field.hint === "template"}
            onFocus={beginEdit}
            onBlur={endEdit}
            onChange={(v) => set(field.name, v)}
          />
        );
      case "prompt":
        return (
          <PromptEditor
            value={asString}
            nodes={autocomplete.nodes}
            edges={autocomplete.edges}
            doc={autocomplete.doc}
            currentStepId={autocomplete.currentStepId}
            onFocus={beginEdit}
            onBlur={endEdit}
            onChange={(v) => set(field.name, v)}
          />
        );
      case "textarea":
        return (
          <Textarea
            value={asString}
            onFocus={beginEdit}
            onBlur={endEdit}
            onChange={(e) => set(field.name, e.target.value)}
          />
        );
      case "enum":
        return (
          <Select value={asString} onFocus={beginEdit} onBlur={endEdit} onChange={(e) => set(field.name, e.target.value)}>
            <option value="">— unset —</option>
            {(field.enumValues ?? []).map((v) => (
              <option key={v} value={v}>
                {v}
              </option>
            ))}
          </Select>
        );
      case "boolean":
        return (
          <span>
            <Checkbox
              checked={raw === true}
              onChange={(e) => {
                beginEdit?.();
                set(field.name, e.target.checked);
                endEdit?.();
              }}
            />
          </span>
        );
      case "integer":
      case "number":
        return (
          <Input
            type="number"
            value={typeof raw === "number" ? String(raw) : ""}
            step={field.widget === "integer" ? 1 : "any"}
            onFocus={beginEdit}
            onBlur={endEdit}
            onChange={(e) => {
              const t = e.target.value;
              set(field.name, t === "" ? undefined : Number(t));
            }}
          />
        );
      case "string-list":
        return (
          <Textarea
            value={Array.isArray(raw) ? (raw as unknown[]).map(String).join("\n") : ""}
            placeholder="one per line"
            onFocus={beginEdit}
            onBlur={endEdit}
            onChange={(e) => {
              const lines = e.target.value.split("\n").map((s) => s.trim()).filter((s) => s !== "");
              set(field.name, lines.length > 0 ? lines : undefined);
            }}
          />
        );
      case "object":
        return (
          <div className="rounded border border-border p-2">
            <SchemaForm
              {...props}
              fields={field.fields ?? []}
              value={typeof raw === "object" && raw !== null && !Array.isArray(raw) ? (raw as Record<string, unknown>) : {}}
              onChange={(v) => set(field.name, Object.keys(v).length > 0 ? v : undefined)}
            />
          </div>
        );
      case "object-list":
        return <ObjectList field={field} props={props} value={Array.isArray(raw) ? (raw as Record<string, unknown>[]) : []} onChange={(v) => set(field.name, v.length > 0 ? v : undefined)} />;
      case "duration":
        return (
          <Input
            value={asString}
            placeholder="e.g. 30s, 1h30m"
            onFocus={beginEdit}
            onBlur={endEdit}
            onChange={(e) => set(field.name, e.target.value)}
          />
        );
      case "text":
        return (
          <Input value={asString} onFocus={beginEdit} onBlur={endEdit} onChange={(e) => set(field.name, e.target.value)} />
        );
      case "json":
      default:
        return <JsonEditor value={raw} onFocus={beginEdit} onBlur={endEdit} onChange={(v) => set(field.name, v)} />;
    }
  }
}

function ObjectList({
  field,
  props,
  value,
  onChange,
}: {
  field: FieldPlan;
  props: SchemaFormProps;
  value: Record<string, unknown>[];
  onChange: (v: Record<string, unknown>[]) => void;
}) {
  return (
    <div className="flex flex-col gap-2">
      {value.map((item, i) => (
        <div key={i} className="rounded border border-border p-2">
          <div className="mb-1 flex items-center justify-between">
            <span className="text-[10px] uppercase tracking-wide text-muted-foreground">{field.name}[{i}]</span>
            <button
              type="button"
              className="text-[11px] text-destructive hover:underline"
              onClick={() => onChange(value.filter((_, j) => j !== i))}
            >
              remove
            </button>
          </div>
          <SchemaForm
            {...props}
            fields={field.fields ?? []}
            value={item}
            onChange={(v) => onChange(value.map((x, j) => (j === i ? v : x)))}
          />
        </div>
      ))}
      <button
        type="button"
        className="self-start rounded border border-border px-2 py-1 text-[11px] hover:bg-muted"
        onClick={() => onChange([...value, {}])}
      >
        + add {field.label.toLowerCase()}
      </button>
      <Fragment />
    </div>
  );
}
