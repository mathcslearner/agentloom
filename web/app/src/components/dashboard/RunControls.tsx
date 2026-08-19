"use client";

/**
 * Run lifecycle controls in the run header (ticket 18.6): Park / Unpark /
 * Cancel, scope-gated (hidden without the `submit` scope, disabled while
 * permissions load). Availability derives from run status; the state change is
 * reflected live through the event feed. Cancel — irreversible — confirms first.
 */
import { useState } from "react";
import type { RunView } from "@agentloom/api-client";
import type { RunController } from "@/lib/dashboard/run-controller";
import { useRunControls } from "@/lib/dashboard/useRunControls";
import { usePermissions } from "@/lib/permissions";
import { runControls } from "@/lib/pure/dashboard/run-controls";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { toast } from "@/components/ui/toast";

export function RunControls({ run, controller }: { run: RunView; controller: RunController }) {
  const { pending, error, cancel, park, unpark } = useRunControls(run.id, controller);
  const perms = usePermissions();
  const avail = runControls(run.status);
  const [confirmCancel, setConfirmCancel] = useState(false);

  // Nothing to steer on a terminal / cancelling run.
  if (!avail.cancel && !avail.park && !avail.unpark) return null;
  // The submit scope gates every control; hide the whole group when the key
  // definitely lacks it, disable while loading.
  const gate = perms.controlState("cancel");
  if (gate === "hidden") return null;
  const disabled = gate === "disabled" || pending;

  const run_ = async (label: string, fn: () => Promise<boolean>) => {
    const ok = await fn();
    if (ok) toast({ title: `${label} requested`, variant: "success" });
  };

  return (
    <div className="flex items-center gap-2" data-testid="run-controls">
      {avail.park ? (
        <Button
          size="sm"
          variant="outline"
          disabled={disabled}
          data-testid="run-park"
          onClick={() => void run_("Park", park)}
        >
          Park
        </Button>
      ) : null}
      {avail.unpark ? (
        <Button
          size="sm"
          variant="outline"
          disabled={disabled}
          data-testid="run-unpark"
          onClick={() => void run_("Unpark", unpark)}
        >
          Unpark
        </Button>
      ) : null}
      {avail.cancel ? (
        <Button
          size="sm"
          variant="destructive"
          disabled={disabled}
          data-testid="run-cancel"
          onClick={() => setConfirmCancel(true)}
        >
          Cancel
        </Button>
      ) : null}
      {error ? <span className="text-xs text-red-600 dark:text-red-400">{error}</span> : null}

      <Dialog open={confirmCancel} onOpenChange={setConfirmCancel}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Cancel this run?</DialogTitle>
            <DialogDescription>
              In-flight steps stop and the run converges to cancelled. This cannot be undone.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" size="sm" onClick={() => setConfirmCancel(false)}>
              Keep running
            </Button>
            <Button
              variant="destructive"
              size="sm"
              disabled={pending}
              data-testid="run-cancel-confirm"
              onClick={async () => {
                setConfirmCancel(false);
                await run_("Cancel", cancel);
              }}
            >
              Cancel run
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}
