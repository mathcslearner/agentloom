"use client";

// A prompt / prose editor with upstream-only `${{ … }}` autocomplete (ticket
// 17.4). The suggestion set is exactly the upstream steps' output paths and the
// declared run params (refs.ts, mirroring the backend's ancestry predicate), so
// a wrong-direction reference is impossible to author. A plain textarea + a
// popover list keeps it dependency-free (no CodeMirror).

import { useMemo, useRef, useState } from "react";
import { Textarea } from "@/components/ui/textarea";
import { activeExpression, suggestionsFor, type RefEdge, type RefNode, type Suggestion } from "@/lib/pure/builder/refs";

export interface PromptEditorProps {
  value: string;
  onChange: (value: string) => void;
  onFocus?: () => void;
  onBlur?: () => void;
  placeholder?: string;
  rows?: number;
  nodes: readonly RefNode[];
  edges: readonly RefEdge[];
  doc: Record<string, unknown>;
  currentStepId: string;
}

export function PromptEditor(props: PromptEditorProps) {
  const { value, onChange, nodes, edges, doc, currentStepId } = props;
  const ref = useRef<HTMLTextAreaElement>(null);
  const [caret, setCaret] = useState(0);
  const [open, setOpen] = useState(false);
  const [active, setActive] = useState(0);

  const suggestions = useMemo<Suggestion[]>(() => {
    if (!open) return [];
    const expr = activeExpression(value, caret);
    if (!expr) return [];
    return suggestionsFor({ fragment: expr.fragment, nodes, edges, doc, currentStepId });
  }, [open, value, caret, nodes, edges, doc, currentStepId]);

  function syncCaret() {
    const el = ref.current;
    if (el) setCaret(el.selectionStart ?? 0);
  }

  function accept(s: Suggestion) {
    const expr = activeExpression(value, caret);
    if (!expr) return;
    // Replace the current token (the last whitespace/pipe-delimited run) with
    // the suggestion value, keeping the rest of the fragment and text intact.
    const before = value.slice(0, expr.start);
    const frag = value.slice(expr.start, caret);
    const after = value.slice(caret);
    const tokenStart = frag.search(/[^\s|(]*$/);
    const next = before + frag.slice(0, tokenStart) + s.value + after;
    onChange(next);
    setOpen(false);
  }

  return (
    <div className="relative">
      <Textarea
        ref={ref}
        value={value}
        rows={props.rows ?? 3}
        spellCheck={false}
        placeholder={props.placeholder}
        onFocus={() => {
          setOpen(true);
          props.onFocus?.();
        }}
        onBlur={() => {
          // Delay so a mousedown on a suggestion registers before close.
          setTimeout(() => setOpen(false), 120);
          props.onBlur?.();
        }}
        onKeyUp={syncCaret}
        onClick={syncCaret}
        onKeyDown={(e) => {
          if (!open || suggestions.length === 0) return;
          if (e.key === "ArrowDown") {
            e.preventDefault();
            setActive((a) => (a + 1) % suggestions.length);
          } else if (e.key === "ArrowUp") {
            e.preventDefault();
            setActive((a) => (a - 1 + suggestions.length) % suggestions.length);
          } else if (e.key === "Enter") {
            e.preventDefault();
            accept(suggestions[active] ?? suggestions[0]!);
          } else if (e.key === "Escape") {
            setOpen(false);
          }
        }}
        onChange={(e) => {
          onChange(e.target.value);
          setCaret(e.target.selectionStart ?? 0);
          setOpen(true);
          setActive(0);
        }}
      />
      {open && suggestions.length > 0 ? (
        <ul
          data-testid="autocomplete"
          className="absolute z-10 mt-1 max-h-48 w-full overflow-auto rounded-md border border-border bg-card py-1 text-xs shadow-md"
        >
          {suggestions.map((s, i) => (
            <li key={s.value}>
              <button
                type="button"
                data-suggestion={s.value}
                className={`flex w-full items-center justify-between px-2.5 py-1 text-left hover:bg-muted ${i === active ? "bg-muted" : ""}`}
                onMouseDown={(e) => {
                  e.preventDefault();
                  accept(s);
                }}
              >
                <span className="font-mono">{s.label}</span>
                <span className="ml-2 text-[10px] uppercase tracking-wide text-muted-foreground">{s.kind}</span>
              </button>
            </li>
          ))}
        </ul>
      ) : null}
    </div>
  );
}
