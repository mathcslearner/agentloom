"use client";

/**
 * A dependency-free collapsible JSON tree for the inspector's Output tab
 * (ticket 18.3). Accepts an already-parsed value or a raw JSON string; renders
 * scalars inline and objects/arrays as expandable nodes. Kept small and local
 * rather than pulling a viewer dependency.
 */
import { useState } from "react";
import { cn } from "@/lib/utils";

function isObject(v: unknown): v is Record<string, unknown> {
  return typeof v === "object" && v !== null && !Array.isArray(v);
}

function Scalar({ value }: { value: unknown }) {
  const cls =
    typeof value === "string"
      ? "text-emerald-700 dark:text-emerald-400"
      : typeof value === "number"
        ? "text-blue-700 dark:text-blue-400"
        : typeof value === "boolean"
          ? "text-purple-700 dark:text-purple-400"
          : "text-muted-foreground";
  const text = typeof value === "string" ? JSON.stringify(value) : String(value);
  return <span className={cls}>{text}</span>;
}

function Node({ name, value, depth }: { name?: string; value: unknown; depth: number }) {
  const composite = isObject(value) || Array.isArray(value);
  const [open, setOpen] = useState(depth < 2);
  if (!composite) {
    return (
      <div className="flex gap-1 whitespace-pre-wrap break-all font-mono text-[11px] leading-relaxed">
        {name !== undefined ? <span className="text-muted-foreground">{name}:</span> : null}
        <Scalar value={value} />
      </div>
    );
  }
  const entries = Array.isArray(value)
    ? value.map((v, i) => [String(i), v] as const)
    : Object.entries(value as Record<string, unknown>);
  const brace = Array.isArray(value) ? ["[", "]"] : ["{", "}"];
  return (
    <div className="font-mono text-[11px] leading-relaxed">
      <button
        type="button"
        className="flex items-center gap-1 text-left hover:text-foreground"
        onClick={() => setOpen((o) => !o)}
      >
        <span className="w-3 text-muted-foreground">{open ? "▾" : "▸"}</span>
        {name !== undefined ? <span className="text-muted-foreground">{name}:</span> : null}
        <span className="text-muted-foreground">
          {brace[0]}
          {open ? "" : `…${entries.length}`}
          {open ? "" : brace[1]}
        </span>
      </button>
      {open ? (
        <div className={cn("border-l border-border pl-3", depth === 0 ? "ml-1" : "ml-2")}>
          {entries.map(([k, v]) => (
            <Node key={k} name={k} value={v} depth={depth + 1} />
          ))}
          <div className="text-muted-foreground">{brace[1]}</div>
        </div>
      ) : null}
    </div>
  );
}

export function JsonViewer({ value }: { value: unknown }) {
  let parsed = value;
  if (typeof value === "string") {
    try {
      parsed = JSON.parse(value);
    } catch {
      // Not JSON — show the raw string.
      return <pre className="max-h-96 overflow-auto whitespace-pre-wrap break-all font-mono text-[11px]">{value}</pre>;
    }
  }
  if (parsed === undefined || parsed === null) {
    return <p className="text-xs italic text-muted-foreground">No output.</p>;
  }
  return (
    <div className="max-h-[28rem] overflow-auto" data-testid="json-viewer">
      <Node value={parsed} depth={0} />
    </div>
  );
}
