"use client";

import type { StepView } from "@agentloom/api-client";
import { JsonViewer } from "./JsonViewer";

export function OutputTab({ step }: { step: StepView }) {
  const hasError = (step.error ?? null) != null;
  return (
    <div className="space-y-4 text-xs" data-testid="inspector-output">
      {hasError ? (
        <section className="space-y-1">
          <h4 className="font-medium text-red-600 dark:text-red-400">Error</h4>
          <JsonViewer value={step.error} />
        </section>
      ) : null}
      <section className="space-y-1">
        <h4 className="font-medium text-foreground">Output</h4>
        <JsonViewer value={step.output} />
      </section>
    </div>
  );
}
