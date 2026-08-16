// Package blackboard defines the run-scoped shared-memory store (ticket
// 12.2, ADR-014): a versioned key/value space steps read and write during a
// run, the substrate M14's multi-agent handoffs and M12.3's context
// assembly build on. This package is a leaf — it imports only the standard
// library, so exec/engine and the coming internal/contextmgr can depend on
// it without a cycle (the internal/cache, internal/limits precedent). It
// owns the domain types (Entry, PutArgs, the Board interface) and the
// key/tag/value rules; the Postgres implementation lives in the pgboard
// subpackage (which alone imports internal/store), keeping this a leaf.
//
// The store is append-only per key: a Put never overwrites, it inserts the
// next version, so History reconstructs every revision — the audit ADR-014
// requires for compaction (12.4) and agent threads (14.2). Tags (including
// the reserved "pinned" tag M12.4 never drops) are per-version and
// immutable: re-tagging is a new version, so the history stays honest.
package blackboard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// TagPinned marks an entry that no compaction strategy may drop or truncate
// (ADR-014). 12.2 only reserves the tag and lets authors set it (via the
// `pinned: true` write sugar or an explicit tag); the never-drop guarantee
// is enforced by 12.4's strategy pipeline.
const TagPinned = "pinned"

// Limits on the shape of an entry. Keys are short identifiers (the same
// grammar an author writes in a step's `blackboard` block); values are
// capped so one entry cannot blow past a context budget on its own or wedge
// the row.
const (
	// MaxKeyLen bounds a blackboard key.
	MaxKeyLen = 128
	// MaxTags bounds the tag set on one entry.
	MaxTags = 32
	// MaxTagLen bounds one tag.
	MaxTagLen = 64
	// MaxValueBytes caps a single entry's value (1 MiB, the response-cache
	// value cap's ceiling — a blackboard entry is a memory item, not a blob
	// store).
	MaxValueBytes = 1 << 20
)

// keyRe is the blackboard key grammar. Deliberately dot-free: a dot is the
// path separator in 12.3's `blackboard.<key>.<path>` context selectors and
// template refs, so a dotted key would be ambiguous. Same character class
// as a plugin name, plus a length bound.
var keyRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// tagRe is the tag grammar: lowercase identifiers with a small punctuation
// set (`:` for namespaced tags like "role:writer", `.` for dotted ones).
var tagRe = regexp.MustCompile(`^[a-z0-9_.:-]{1,64}$`)

// Entry is one immutable version of one blackboard key. A Put appends the
// next version; nothing is ever updated in place. TokenCount is the value's
// size under TokenCounter (a tokens.Counter.ID() fingerprint), stored
// together so a consumer knows which counter produced the number and can
// recompute on a fingerprint mismatch (ADR-014).
type Entry struct {
	Key     string          `json:"key"`
	Version int             `json:"version"`
	Value   json.RawMessage `json:"value"`
	// TokenCount is the value's token size under TokenCounter.
	TokenCount int `json:"token_count"`
	// TokenCounter is the tokens.Counter.ID() that produced TokenCount, e.g.
	// "openai/o200k_base@1" or "fallback/chars4@1".
	TokenCounter string `json:"token_counter"`
	// Tags are the entry's labels, deduped and sorted; may include TagPinned.
	Tags []string `json:"tags"`
	// AuthorStepID is the step that wrote this version; "" for a non-step
	// author (reserved for a future system/API writer — 12.2 only writes
	// from steps).
	AuthorStepID string `json:"author_step_id,omitempty"`
	// AuthorAttempt is the 1-based attempt of the authoring step; 0 when
	// there is no step author.
	AuthorAttempt int       `json:"author_attempt,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
}

// Pinned reports whether the entry carries the reserved pinned tag.
func (e Entry) Pinned() bool {
	for _, t := range e.Tags {
		if t == TagPinned {
			return true
		}
	}
	return false
}

// PutArgs is a single blackboard write. Value is stored verbatim (any valid
// JSON — a string, object, array, number). Tags are validated, deduped, and
// sorted before storage. ExpectedVersion is the optional compare-and-swap
// guard: nil writes unconditionally (the next version wins), a non-nil value
// requires the current head to equal it — 0 means "the key must not yet
// exist". A CAS mismatch is a *VersionConflictError, never a lost update.
type PutArgs struct {
	Key             string
	Value           json.RawMessage
	Tags            []string
	ExpectedVersion *int
}

// ListFilter selects heads (the latest version of each key). An empty
// filter returns every key's head. Keys, when non-empty, restricts to those
// keys. Tags, when non-empty, keeps only heads whose tag set contains every
// listed tag (AND semantics).
type ListFilter struct {
	Keys []string
	Tags []string
}

// Board is the read/write surface executors and the API use. The engine
// binds a step-scoped Board onto StepContext (writes are attributed to the
// step); the API reads through a run-scoped Board. Implementations are in
// the pgboard subpackage.
type Board interface {
	// Get returns the head (latest version) of key. The bool is false with a
	// nil error when the key has never been written.
	Get(ctx context.Context, key string) (Entry, bool, error)
	// History returns every version of key in ascending version order; an
	// empty slice when the key does not exist.
	History(ctx context.Context, key string) ([]Entry, error)
	// List returns the head of each key matching the filter, ordered by key.
	List(ctx context.Context, filter ListFilter) ([]Entry, error)
	// Put appends a new version of a key, returning the stored entry. A CAS
	// mismatch is a *VersionConflictError; an invalid key/tag or oversized
	// value is a permanent typed error; a fenced (zombie) write is ErrFenced.
	Put(ctx context.Context, args PutArgs) (Entry, error)
}

// ErrNotFound reports a read for a key that has never been written. Test
// with errors.Is.
var ErrNotFound = errors.New("blackboard: key not found")

// ErrFenced reports a write from a step whose claim no longer matches the
// run_steps row — a zombie whose lease was taken over (ADR-005 fencing).
// Unlike the side-effect journal's first-wins result write, a blackboard
// write is claim-fenced: a taken-over attempt must not mutate shared memory
// the new holder is now responsible for. Test with errors.Is.
var ErrFenced = errors.New("blackboard: write rejected by claim fence")

// VersionConflictError reports a compare-and-swap mismatch: the caller
// expected the head to be Expected but it was Current (0 = the key did not
// exist). The engine surfaces this as a transient step error so the retry
// re-reads the head and can succeed at the next version — the ADR-014
// concurrent-writers contract.
type VersionConflictError struct {
	Key      string
	Expected int
	Current  int
}

func (e *VersionConflictError) Error() string {
	return fmt.Sprintf("blackboard: CAS conflict on key %q: expected version %d, current %d", e.Key, e.Expected, e.Current)
}

// InvalidKeyError reports a key that does not match the blackboard key
// grammar. Permanent by nature — the same key fails identically on retry.
type InvalidKeyError struct {
	Key    string
	Reason string
}

func (e *InvalidKeyError) Error() string {
	return fmt.Sprintf("blackboard: invalid key %q: %s", e.Key, e.Reason)
}

// InvalidTagError reports a tag that does not match the tag grammar or a
// tag-set-size violation. Permanent.
type InvalidTagError struct {
	Tag    string
	Reason string
}

func (e *InvalidTagError) Error() string {
	if e.Tag == "" {
		return fmt.Sprintf("blackboard: invalid tags: %s", e.Reason)
	}
	return fmt.Sprintf("blackboard: invalid tag %q: %s", e.Tag, e.Reason)
}

// ValueTooLargeError reports a value over MaxValueBytes. Permanent.
type ValueTooLargeError struct {
	Key  string
	Size int
	Max  int
}

func (e *ValueTooLargeError) Error() string {
	return fmt.Sprintf("blackboard: value for key %q is %d bytes, over the %d-byte limit", e.Key, e.Size, e.Max)
}

// ValidateKey enforces the key grammar.
func ValidateKey(key string) error {
	if key == "" {
		return &InvalidKeyError{Key: key, Reason: "key is required"}
	}
	if len(key) > MaxKeyLen {
		return &InvalidKeyError{Key: key, Reason: fmt.Sprintf("longer than %d characters", MaxKeyLen)}
	}
	if !keyRe.MatchString(key) {
		return &InvalidKeyError{Key: key, Reason: "must match " + keyRe.String() + " (letters, digits, '_' and '-'; no dots)"}
	}
	return nil
}

// NormalizeTags validates, deduplicates, and sorts a tag set. A nil/empty
// input returns an empty (non-nil) slice so the stored column is never NULL.
// The returned slice is safe to store and query with array-contains.
func NormalizeTags(tags []string) ([]string, error) {
	if len(tags) > MaxTags {
		return nil, &InvalidTagError{Reason: fmt.Sprintf("at most %d tags, got %d", MaxTags, len(tags))}
	}
	seen := make(map[string]struct{}, len(tags))
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		if t == "" {
			return nil, &InvalidTagError{Reason: "empty tag"}
		}
		if len(t) > MaxTagLen {
			return nil, &InvalidTagError{Tag: t, Reason: fmt.Sprintf("longer than %d characters", MaxTagLen)}
		}
		if !tagRe.MatchString(t) {
			return nil, &InvalidTagError{Tag: t, Reason: "must match " + tagRe.String()}
		}
		if _, dup := seen[t]; dup {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	sort.Strings(out)
	return out, nil
}

// ValidateValue enforces the value size cap and that the value is present.
func ValidateValue(key string, value json.RawMessage) error {
	if len(value) == 0 {
		return &InvalidKeyError{Key: key, Reason: "value is required"}
	}
	if len(value) > MaxValueBytes {
		return &ValueTooLargeError{Key: key, Size: len(value), Max: MaxValueBytes}
	}
	if !json.Valid(value) {
		return &InvalidKeyError{Key: key, Reason: "value is not valid JSON"}
	}
	return nil
}

// CanonicalValue compacts a JSON value so identical logical values store
// identical bytes and token-count identically — the ADR-014 determinism
// requirement the stored (token_count, counter_id) rests on. Invalid JSON is
// returned unchanged (callers validate first).
func CanonicalValue(raw json.RawMessage) json.RawMessage {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return raw
	}
	return json.RawMessage(append([]byte(nil), buf.Bytes()...))
}

// ResolvePointer resolves an RFC 6901 JSON pointer into a JSON document,
// returning the addressed sub-value as compact JSON. An empty pointer
// selects the whole document. A pointer that does not resolve (a missing
// object key or an out-of-range/non-array index) is an error — the engine's
// declarative-write path treats it as a permanent data error, since the same
// output resolves the same way on every retry.
func ResolvePointer(doc json.RawMessage, pointer string) (json.RawMessage, error) {
	if len(doc) == 0 {
		// A step with no output: the whole-output value is JSON null; a
		// pointer into it cannot resolve.
		doc = json.RawMessage("null")
	}
	if pointer == "" {
		return CanonicalValue(doc), nil
	}
	tokens, err := parsePointer(pointer)
	if err != nil {
		return nil, err
	}
	var cur any
	if err := json.Unmarshal(doc, &cur); err != nil {
		return nil, fmt.Errorf("value is not valid JSON: %w", err)
	}
	for _, tok := range tokens {
		switch node := cur.(type) {
		case map[string]any:
			v, ok := node[tok]
			if !ok {
				return nil, fmt.Errorf("pointer %q: no key %q", pointer, tok)
			}
			cur = v
		case []any:
			idx, ierr := strconv.Atoi(tok)
			if ierr != nil || idx < 0 || idx >= len(node) {
				return nil, fmt.Errorf("pointer %q: index %q out of range", pointer, tok)
			}
			cur = node[idx]
		default:
			return nil, fmt.Errorf("pointer %q: cannot descend into %q", pointer, tok)
		}
	}
	out, err := json.Marshal(cur)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(out), nil
}

// parsePointer splits an RFC 6901 pointer ("/a/b/0") into its decoded
// reference tokens.
func parsePointer(pointer string) ([]string, error) {
	if pointer[0] != '/' {
		return nil, fmt.Errorf("pointer %q must start with '/'", pointer)
	}
	parts := strings.Split(pointer[1:], "/")
	for i, p := range parts {
		p = strings.ReplaceAll(p, "~1", "/")
		p = strings.ReplaceAll(p, "~0", "~")
		parts[i] = p
	}
	return parts, nil
}
