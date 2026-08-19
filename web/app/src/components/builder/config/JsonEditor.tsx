"use client";

// A JSON value editor (ticket 17.4): the fallback for config fields whose schema
// is the any-schema (`true`, i.e. a json.RawMessage — payload, input, value,
// edit_schema, arbitrary JSON). Edits are held as text so an in-progress,
// not-yet-valid value never clobbers the model; a parse error is shown inline
// and the value is committed only when it parses.

import { useEffect, useRef, useState } from "react";
import { Textarea } from "@/components/ui/textarea";

export interface JsonEditorProps {
  value: unknown;
  onChange: (value: unknown) => void;
  onFocus?: () => void;
  onBlur?: () => void;
  placeholder?: string;
  rows?: number;
}

function toText(value: unknown): string {
  if (value === undefined) return "";
  try {
    return JSON.stringify(value, null, 2);
  } catch {
    return "";
  }
}

export function JsonEditor({ value, onChange, onFocus, onBlur, placeholder, rows = 4 }: JsonEditorProps) {
  const [text, setText] = useState(() => toText(value));
  const [error, setError] = useState<string | null>(null);
  const editing = useRef(false);

  // Re-sync from the model when the value changes externally (not mid-edit).
  useEffect(() => {
    if (!editing.current) setText(toText(value));
  }, [value]);

  return (
    <div>
      <Textarea
        value={text}
        rows={rows}
        spellCheck={false}
        placeholder={placeholder ?? "JSON value"}
        className="font-mono text-[11px]"
        onFocus={() => {
          editing.current = true;
          onFocus?.();
        }}
        onBlur={() => {
          editing.current = false;
          onBlur?.();
        }}
        onChange={(e) => {
          const next = e.target.value;
          setText(next);
          if (next.trim() === "") {
            setError(null);
            onChange(undefined);
            return;
          }
          try {
            onChange(JSON.parse(next));
            setError(null);
          } catch (err) {
            setError(err instanceof Error ? err.message : "invalid JSON");
          }
        }}
      />
      {error ? <p className="mt-1 text-[11px] text-destructive">{error}</p> : null}
    </div>
  );
}
