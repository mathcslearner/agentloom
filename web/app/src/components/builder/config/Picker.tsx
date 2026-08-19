"use client";

// A combobox for a string field whose values come from the plugin catalog
// (ticket 17.4): the `tool`, `retriever`, `agent`, and map `body` fields. A
// native input + datalist so free text is always allowed (the catalog may be
// unavailable, or the value may name something not yet declared) while the
// known names are one keystroke away. An unknown value is flagged as a warning.

import { useId } from "react";
import { Input } from "@/components/ui/input";

export interface PickerProps {
  value: string;
  onChange: (value: string) => void;
  onFocus?: () => void;
  onBlur?: () => void;
  options: readonly string[];
  placeholder?: string;
  /** When true, a value not in `options` shows a "not in catalog" warning. */
  warnUnknown?: boolean;
}

export function Picker({ value, onChange, onFocus, onBlur, options, placeholder, warnUnknown }: PickerProps) {
  const listId = useId();
  const unknown = warnUnknown && value !== "" && !options.includes(value);
  return (
    <div>
      <Input
        value={value}
        list={listId}
        placeholder={placeholder}
        onFocus={onFocus}
        onBlur={onBlur}
        onChange={(e) => onChange(e.target.value)}
      />
      <datalist id={listId}>
        {options.map((o) => (
          <option key={o} value={o} />
        ))}
      </datalist>
      {unknown ? <p className="mt-1 text-[11px] text-amber-600">not in the catalog — will only fail at run time</p> : null}
    </div>
  );
}
