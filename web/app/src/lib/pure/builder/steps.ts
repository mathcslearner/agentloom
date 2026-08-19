// Pure helpers for minting new steps on the canvas (ticket 17.3): a fresh,
// unique step id and a bare step object of a given type. Config editing is
// 17.4; a new step carries an empty `config: {}`, which is well-shaped (the
// backend's config fields are all optional) and rides losslessly through
// graphdef, and may be invalid until the config panel fills it in (17.5).

import type { Position, Step, StepType } from "@agentloom/graphdef";

/**
 * A unique step id of the form `<type>_<n>` (matching the backend's step-id
 * grammar), choosing the lowest free ordinal not already taken.
 */
export function allocateStepId(type: StepType, existing: Iterable<string>): string {
  const taken = new Set(existing);
  let n = 1;
  let id = `${type}_${n}`;
  while (taken.has(id)) {
    n += 1;
    id = `${type}_${n}`;
  }
  return id;
}

/** A bare step of the given type with an empty config. */
export function newStepForType(type: StepType, id: string): Step {
  // config values are all optional in the schema, so `{}` is well-shaped for
  // every step type; the discriminated union is satisfied by the `type` key.
  return { id, type, config: {} } as Step;
}

/**
 * A cascade offset from a base point, so repeated adds without a drop point
 * (palette click / keyboard) do not stack exactly on top of one another.
 */
export function cascadePlacement(base: Position, count: number, step = 28, span = 8): Position {
  const k = count % span;
  return { x: base.x + k * step, y: base.y + k * step };
}
