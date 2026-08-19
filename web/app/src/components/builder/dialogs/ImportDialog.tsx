"use client";

// Import a workflow definition JSON into the builder (ticket 17.6). Accepts a
// file or pasted text, parses + maps it to a Flow (surfacing parse and shape
// errors), and shows a validation summary before loading — an invalid-but-
// well-shaped definition may still be imported (ADR-019: a Flow can hold an
// invalid graph; the Problems panel then reports it), a non-object / duplicate-id
// document cannot. Loading replaces the canvas and clears the source (imports
// are unsaved until Save).

import { useRef, useState } from "react";
import { GraphdefError, toFlow, validateDefinition, type Flow } from "@agentloom/graphdef";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Button } from "@/components/ui/button";
import { Textarea } from "@/components/ui/textarea";
import { useBuilderStore } from "@/lib/builder/store";
import { toast } from "@/components/ui/toast";

interface Parsed {
  flow: Flow;
  errors: number;
  warnings: number;
}

export function ImportDialog({ open, onOpenChange }: { open: boolean; onOpenChange: (o: boolean) => void }) {
  const load = useBuilderStore((s) => s.load);
  const [text, setText] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [parsed, setParsed] = useState<Parsed | null>(null);
  const fileRef = useRef<HTMLInputElement>(null);

  function reset() {
    setText("");
    setError(null);
    setParsed(null);
  }

  function evaluate(raw: string) {
    setText(raw);
    setError(null);
    setParsed(null);
    if (!raw.trim()) return;
    let value: unknown;
    try {
      value = JSON.parse(raw);
    } catch (e) {
      setError(`Not valid JSON: ${(e as Error).message}`);
      return;
    }
    let flow: Flow;
    try {
      flow = toFlow(value);
    } catch (e) {
      if (e instanceof GraphdefError) {
        setError(`Cannot import${e.path ? ` at ${e.path}` : ""}: ${e.message}`);
      } else {
        setError((e as Error).message);
      }
      return;
    }
    const issues = validateDefinition(value);
    setParsed({
      flow,
      errors: issues.filter((i) => i.severity === "error").length,
      warnings: issues.filter((i) => i.severity === "warning").length,
    });
  }

  async function onFile(file: File) {
    evaluate(await file.text());
  }

  function doImport() {
    if (!parsed) return;
    load(parsed.flow, null); // imports are unsaved
    toast({ title: "Imported", description: `${parsed.errors} errors, ${parsed.warnings} warnings`, variant: parsed.errors ? "error" : "success" });
    onOpenChange(false);
    reset();
  }

  return (
    <Dialog
      open={open}
      onOpenChange={(o) => {
        onOpenChange(o);
        if (!o) reset();
      }}
    >
      <DialogContent data-testid="import-dialog">
        <DialogHeader>
          <DialogTitle>Import definition</DialogTitle>
          <DialogDescription>Load a workflow definition JSON. This replaces the current canvas.</DialogDescription>
        </DialogHeader>

        <div className="flex flex-col gap-3">
          <div>
            <input
              ref={fileRef}
              type="file"
              accept="application/json,.json"
              data-testid="import-file"
              className="hidden"
              onChange={(e) => {
                const f = e.target.files?.[0];
                if (f) void onFile(f);
              }}
            />
            <Button variant="outline" size="sm" onClick={() => fileRef.current?.click()} data-testid="import-choose-file">
              Choose file…
            </Button>
          </div>
          <Textarea
            value={text}
            onChange={(e) => evaluate(e.target.value)}
            placeholder="…or paste definition JSON here"
            rows={10}
            data-testid="import-textarea"
            className="font-mono text-[11px]"
          />
          {error ? (
            <p className="text-xs text-destructive" data-testid="import-error">
              {error}
            </p>
          ) : parsed ? (
            <p className="text-xs text-muted-foreground" data-testid="import-summary">
              Well-formed. Validation: {parsed.errors} {parsed.errors === 1 ? "error" : "errors"}, {parsed.warnings}{" "}
              {parsed.warnings === 1 ? "warning" : "warnings"}
              {parsed.errors > 0 ? " — you can still import and fix them on the canvas." : ""}
            </p>
          ) : null}
        </div>

        <DialogFooter>
          <Button variant="outline" size="sm" onClick={() => onOpenChange(false)}>
            Cancel
          </Button>
          <Button size="sm" onClick={doImport} disabled={!parsed} data-testid="import-confirm">
            Import
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
