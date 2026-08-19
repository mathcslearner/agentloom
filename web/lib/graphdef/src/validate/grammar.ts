// Grammar helpers ported verbatim from the Go validator so a client finding and
// a server finding name the same problem: the RFC-6901 JSON-pointer syntax
// check (internal/dag/validation.go checkJSONPointer) and the blackboard key /
// tag grammar (internal/blackboard blackboard.go ValidateKey / NormalizeTags).

import {
  MAX_BLACKBOARD_KEY_LEN,
  MAX_BLACKBOARD_TAGS,
  MAX_BLACKBOARD_TAG_LEN,
} from "./limits.js";

// blackboard.keyRe — dot-free (a dot is the path separator elsewhere).
const KEY_RE = /^[A-Za-z0-9_-]{1,128}$/;
// blackboard.tagRe — lowercase identifiers with a small punctuation set.
const TAG_RE = /^[a-z0-9_.:-]{1,64}$/;
const KEY_RE_TEXT = "^[A-Za-z0-9_-]{1,128}$";
const TAG_RE_TEXT = "^[a-z0-9_.:-]{1,64}$";

/** Validate an RFC-6901 JSON pointer's syntax; returns an error string or null. */
export function checkJSONPointer(s: string): string | null {
  if (s === "") return null;
  if (s[0] !== "/") return "must start with '/'";
  for (let i = 0; i < s.length; i += 1) {
    if (s[i] !== "~") continue;
    if (i + 1 >= s.length || (s[i + 1] !== "0" && s[i + 1] !== "1")) {
      return "'~' must be followed by '0' or '1'";
    }
    i += 1;
  }
  return null;
}

/** Validate a blackboard key (blackboard.ValidateKey); returns an error string or null. */
export function validateBlackboardKey(key: string): string | null {
  if (key === "") return "key is required";
  if (key.length > MAX_BLACKBOARD_KEY_LEN) return `longer than ${MAX_BLACKBOARD_KEY_LEN} characters`;
  if (!KEY_RE.test(key)) {
    return `must match ${KEY_RE_TEXT} (letters, digits, '_' and '-'; no dots)`;
  }
  return null;
}

/** Validate a blackboard tag set (blackboard.NormalizeTags); returns an error string or null. */
export function validateBlackboardTags(tags: readonly string[]): string | null {
  if (tags.length > MAX_BLACKBOARD_TAGS) return `at most ${MAX_BLACKBOARD_TAGS} tags, got ${tags.length}`;
  for (const t of tags) {
    if (t === "") return "empty tag";
    if (t.length > MAX_BLACKBOARD_TAG_LEN) return `longer than ${MAX_BLACKBOARD_TAG_LEN} characters`;
    if (!TAG_RE.test(t)) return `must match ${TAG_RE_TEXT}`;
  }
  return null;
}
