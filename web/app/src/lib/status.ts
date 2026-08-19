import type { RunStatus } from "@agentloom/api-client";
import type { BadgeProps } from "@/components/ui/badge";
import type { DisplayStepStatus } from "@/lib/pure/dashboard/run-state";

type BadgeVariant = NonNullable<BadgeProps["variant"]>;

/** Map a run status onto a Badge variant (status-neutral palette reused by M18). */
export function runStatusVariant(status: RunStatus): BadgeVariant {
  switch (status) {
    case "running":
      return "running";
    case "succeeded":
      return "succeeded";
    case "failed":
      return "failed";
    case "parked":
      return "parked";
    case "cancelling":
    case "cancelled":
      return "cancelled";
    default:
      return "muted";
  }
}

/** Map a (display) step status onto a Badge variant (ticket 18.1). */
export function stepStatusVariant(status: DisplayStepStatus): BadgeVariant {
  switch (status) {
    case "running":
      return "running";
    case "succeeded":
      return "succeeded";
    case "failed":
    case "dead_lettered":
      return "failed";
    case "retrying":
    case "throttled":
    case "awaiting_human":
      return "parked";
    case "cancelled":
    case "skipped":
    case "collected":
      return "cancelled";
    default:
      return "muted";
  }
}
