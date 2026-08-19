/**
 * Pure budget-banner derivation for the run header (ticket 18.4).
 *
 * Cost/budget-lifecycle events are projected onto typed banners the header
 * surfaces: a model downgrade (names from/to models + the trigger), a
 * budget-exceeded park or fail (names the limit + projection), a budget raise,
 * and the live "parked at budget cap" affordance (present while the run is
 * parked for budget, gone once it unparks). Banners are keyed by the event seq
 * that produced them, so they dedupe across a reconnect's re-backfill; the
 * caller keeps a local set of dismissed seqs.
 *
 * No React/UI imports — under the app's `pure/` eslint boundary.
 */
import type { EventEnvelope } from "@agentloom/engine-client";
import type { RunView } from "@agentloom/api-client";
import { formatUsd } from "./cost-meter";

export type BannerKind = "downgrade" | "budget_exceeded" | "budget_failed" | "budget_raised" | "parked_for_budget";
export type BannerSeverity = "info" | "warn" | "error" | "success";

export interface BudgetBanner {
  /** The seq of the event that produced it (dedupe + dismiss key). Synthetic
   * banners (the live parked affordance) use a stable non-event key < 0. */
  key: number;
  kind: BannerKind;
  severity: BannerSeverity;
  title: string;
  detail: string;
  /** Set on the parked-for-budget banner: the header renders a Raise action. */
  actionable?: boolean;
  /** Downgrade banner: the from/to models + resolved trigger, for the DoD-2
   * assertion (`data-from`/`data-to`/`data-trigger`). */
  from?: string;
  to?: string;
  trigger?: string;
}

const PARKED_BANNER_KEY = -1;

/** Human label for a downgrade trigger. */
function triggerLabel(trigger: string, limit: string | undefined, fraction: number | undefined): string {
  if (trigger === "budget_threshold") {
    return fraction ? `budget threshold ${Math.round(fraction * 100)}%` : "budget threshold";
  }
  if (trigger === "budget_projection") {
    return limit ? `budget projection (${limit})` : "budget projection";
  }
  return trigger;
}

/** Human label for a budget limit. */
function limitLabel(limit: string): string {
  switch (limit) {
    case "run":
      return "run budget";
    case "step_usd":
      return "step spend cap";
    case "step_tokens":
      return "step token cap";
    default:
      return limit;
  }
}

/**
 * Derive the current banner set from the event feed and the run's live status.
 * Newest-first; the live parked-for-budget affordance (if any) is pinned first.
 */
export function deriveBudgetBanners(events: readonly EventEnvelope[], run: RunView | undefined): BudgetBanner[] {
  const banners: BudgetBanner[] = [];

  for (const env of events) {
    switch (env.type) {
      case "model_downgraded": {
        const p = env.payload;
        banners.push({
          key: env.seq,
          kind: "downgrade",
          severity: "info",
          title: "Model downgraded",
          detail: `${p.step_id} (attempt ${p.attempt}): ${p.from_model} → ${p.to_model} — ${triggerLabel(
            p.trigger,
            p.limit,
            p.threshold_fraction,
          )}`,
          from: p.from_model,
          to: p.to_model,
          trigger: p.trigger,
        });
        break;
      }
      case "budget_exceeded": {
        const p = env.payload;
        const projected = p.projected_nano_usd ?? p.estimate_nano_usd;
        const detailParts = [`${p.step_id}`, limitLabel(p.limit)];
        if (projected !== undefined && p.budget_nano_usd !== undefined) {
          detailParts.push(`projected ${formatUsd(projected)} > cap ${formatUsd(p.budget_nano_usd)}`);
        }
        if (p.action === "fail") {
          banners.push({
            key: env.seq,
            kind: "budget_failed",
            severity: "error",
            title: "Budget exceeded — run failed",
            detail: detailParts.join(" · "),
          });
        } else {
          banners.push({
            key: env.seq,
            kind: "budget_exceeded",
            severity: "warn",
            title: "Budget exceeded — run parked",
            detail: detailParts.join(" · "),
          });
        }
        break;
      }
      case "run_budget_updated": {
        const p = env.payload;
        banners.push({
          key: env.seq,
          kind: "budget_raised",
          severity: "success",
          title: "Budget raised",
          detail: `${formatUsd(p.previous_nano_usd)} → ${formatUsd(p.budget_nano_usd)}`,
        });
        break;
      }
    }
  }

  banners.reverse(); // newest-first

  // The live "parked at cap" affordance — present only while the run is
  // actually parked for budget (cleared on unpark). Pinned to the top; it is
  // the surface for the Raise action.
  if (run && run.status === "parked" && run.park_reason === "budget_exceeded") {
    banners.unshift({
      key: PARKED_BANNER_KEY,
      kind: "parked_for_budget",
      severity: "warn",
      title: "Run parked at budget cap",
      detail:
        run.cost.budget_nano_usd !== undefined
          ? `spent ${formatUsd(run.cost.spent_nano_usd)} of ${formatUsd(run.cost.budget_nano_usd)}`
          : `spent ${formatUsd(run.cost.spent_nano_usd)}`,
      actionable: true,
    });
  }

  return banners;
}

export { PARKED_BANNER_KEY };
