import type { RunStatus } from "@agentloom/api-client";
import type { BadgeProps } from "@/components/ui/badge";

/** Map a run status onto a Badge variant (status-neutral palette reused by M18). */
export function runStatusVariant(status: RunStatus): NonNullable<BadgeProps["variant"]> {
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
