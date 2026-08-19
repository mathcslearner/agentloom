/**
 * Reconnection backoff — a pure function of the attempt number, so it is
 * exhaustively testable with an injected RNG.
 */

export interface BackoffOptions {
  /** Delay for the first (attempt 0) reconnect, in ms. */
  initialMs: number;
  /** Ceiling on the computed delay, in ms. */
  maxMs: number;
  /** Exponential growth base. */
  factor: number;
  /** `full` = uniform in [0, base) (AWS full jitter); `none` = exactly base. */
  jitter: "full" | "none";
}

export const defaultBackoff: BackoffOptions = {
  initialMs: 250,
  maxMs: 10_000,
  factor: 2,
  jitter: "full",
};

export function resolveBackoff(o?: Partial<BackoffOptions>): BackoffOptions {
  return { ...defaultBackoff, ...o };
}

/**
 * The delay before reconnect attempt `attempt` (0-based). The uncapped base is
 * `initialMs * factor**attempt`, clamped to `maxMs`; full jitter then spreads it
 * uniformly over `[0, base)` to avoid a thundering herd. `rng` returns [0, 1).
 */
export function backoffDelay(
  attempt: number,
  opts: BackoffOptions,
  rng: () => number = Math.random,
): number {
  const base = Math.min(opts.maxMs, opts.initialMs * Math.pow(opts.factor, Math.max(0, attempt)));
  if (opts.jitter === "none") return Math.round(base);
  return Math.floor(rng() * base);
}
