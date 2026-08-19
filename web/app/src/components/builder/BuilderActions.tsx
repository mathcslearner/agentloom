"use client";

// The builder toolbar's action cluster (ticket 17.6): Import, Export (canonical
// JSON download), Save (create / version), and Submit (params modal). Plus the
// document name + version + a dirty indicator, and the live error/warning count
// that gates Save/Submit. Owns the dialog open-state and the export download.

import { useState } from "react";
import { canonicalize } from "@agentloom/graphdef";
import { Button } from "@/components/ui/button";
import { useBuilderStore, selectIsDirty } from "@/lib/builder/store";
import { useProblemsCtx } from "@/lib/builder/problems-context";
import { downloadText, definitionFilename } from "@/lib/builder/download";
import { toast } from "@/components/ui/toast";
import { ImportDialog } from "./dialogs/ImportDialog";
import { SaveDialog } from "./dialogs/SaveDialog";
import { SubmitDialog } from "./dialogs/SubmitDialog";
import { ConfirmDialog } from "./dialogs/ConfirmDialog";

export function BuilderActions() {
  const source = useBuilderStore((s) => s.source);
  const doc = useBuilderStore((s) => s.doc);
  const isDirty = useBuilderStore(selectIsDirty);
  const toDefinitionValue = useBuilderStore((s) => s.toDefinitionValue);
  const { problems, focusIssue } = useProblemsCtx();

  const errorCount = problems.errors.length;
  const warningCount = problems.warnings.length;
  const name = typeof doc["name"] === "string" ? (doc["name"] as string) : "untitled";

  const [importOpen, setImportOpen] = useState(false);
  const [saveOpen, setSaveOpen] = useState(false);
  const [submitOpen, setSubmitOpen] = useState(false);
  const [confirmImport, setConfirmImport] = useState(false);

  function doExport() {
    const text = canonicalize(toDefinitionValue());
    downloadText(definitionFilename(name), text);
    toast({ title: "Exported canonical JSON", description: definitionFilename(name), variant: "success" });
  }

  function openImport() {
    if (isDirty) setConfirmImport(true);
    else setImportOpen(true);
  }

  return (
    <div className="flex items-center gap-3">
      <span className="text-xs" data-testid="doc-name">
        <span className="font-medium">{name}</span>
        {source ? <span className="text-muted-foreground"> v{source.version}</span> : <span className="text-muted-foreground"> (unsaved)</span>}
        {isDirty ? (
          <span className="ml-1 text-amber-600 dark:text-amber-500" data-testid="dirty-indicator" title="Unsaved changes">
            ●
          </span>
        ) : null}
      </span>

      {errorCount > 0 || warningCount > 0 ? (
        <button
          type="button"
          data-testid="problem-count"
          onClick={() => {
            const first = problems.errors[0] ?? problems.warnings[0];
            if (first) focusIssue(first);
          }}
          className="text-xs font-medium"
          title="Jump to the first problem"
        >
          {errorCount > 0 ? (
            <span className="text-destructive">
              {errorCount} {errorCount === 1 ? "error" : "errors"}
            </span>
          ) : null}
          {errorCount > 0 && warningCount > 0 ? <span className="text-muted-foreground"> · </span> : null}
          {warningCount > 0 ? (
            <span className="text-amber-600 dark:text-amber-500">
              {warningCount} {warningCount === 1 ? "warning" : "warnings"}
            </span>
          ) : null}
        </button>
      ) : (
        <span className="text-xs font-medium text-emerald-600 dark:text-emerald-500" data-testid="problem-count-ok">
          No problems
        </span>
      )}

      <div className="flex items-center gap-1.5">
        <Button variant="outline" size="sm" onClick={openImport} data-testid="import-open">
          Import
        </Button>
        <Button variant="outline" size="sm" onClick={doExport} data-testid="export">
          Export
        </Button>
        <Button
          variant="outline"
          size="sm"
          onClick={() => setSaveOpen(true)}
          disabled={errorCount > 0}
          data-testid="save-open"
          title={errorCount > 0 ? `${errorCount} ${errorCount === 1 ? "error" : "errors"} block save` : "Save definition"}
        >
          Save
        </Button>
        <Button
          size="sm"
          onClick={() => setSubmitOpen(true)}
          disabled={errorCount > 0}
          data-testid="submit-run"
          title={errorCount > 0 ? `${errorCount} ${errorCount === 1 ? "error" : "errors"} block submit` : "Submit a run"}
        >
          Submit
        </Button>
      </div>

      <ImportDialog open={importOpen} onOpenChange={setImportOpen} />
      <SaveDialog open={saveOpen} onOpenChange={setSaveOpen} />
      <SubmitDialog open={submitOpen} onOpenChange={setSubmitOpen} />
      <ConfirmDialog
        open={confirmImport}
        title="Discard unsaved changes?"
        description="Importing replaces the current canvas. Your unsaved changes will be lost."
        confirmLabel="Discard & import"
        confirmVariant="default"
        onConfirm={() => {
          setConfirmImport(false);
          setImportOpen(true);
        }}
        onCancel={() => setConfirmImport(false)}
      />
    </div>
  );
}
