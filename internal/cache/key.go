package cache

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/mathcslearner/agentloom/internal/dag"
)

// KeyInput is the complete set of components a cache key is built from
// (ADR-011). The 9.5 middleware fills it from a resolved request: the
// definition format version, the two plugin identities that bear on the
// output, the cache scope, and the per-step-type request body.
type KeyInput struct {
	// SchemaVersion is the workflow definition format version (dag.CurrentSchemaVersion
	// today): a format change that could reinterpret a stored config
	// invalidates every prior key.
	SchemaVersion int

	// Executor is the cacheable executor plugin — llm/tool/retrieve at its
	// version — the code that turns config into a request and a response
	// into the stored output. A change to that transformation bumps its
	// version and invalidates.
	Executor PluginRef

	// Plugin is the concrete external plugin the step actually used: the
	// model provider for an llm step, the tool for a tool step, the
	// retriever for a retrieve step. It is both hashed into the key and used
	// as the Redis key namespace (RedisKey), so an operator can bust "every
	// entry for provider X / tool Y / retriever Z" by prefix (9.6).
	Plugin PluginRef

	// Scope bounds sharing (ADR-011). CacheGlobal (the zero value is treated
	// as global) shares across all runs; CacheRun mixes RunID into the key.
	Scope dag.CacheScope

	// RunID is required when Scope is CacheRun and ignored otherwise.
	RunID string

	// Request is the per-step-type request body. Exactly one of the
	// concrete request types (LLMRequest, ToolRequest, RetrieveRequest).
	Request Request
}

// Request is the per-step-type body of a cache key: the rendered,
// resolved request whose bytes determine the output. The interface is
// closed — only this package's request types implement it — so the key
// format is a fixed, reviewable set.
type Request interface {
	// components returns the ordered, canonicalized byte components that
	// follow the common header in the key. A canonicalization failure
	// (malformed request JSON) is returned, never hashed over.
	components() ([][]byte, error)
}

// LLMRequest is the cache-key body of an llm step: the resolved model, the
// sampling params (temperature distinguished nil-from-zero — nil is the
// provider default, a different, non-deterministic request than an explicit
// 0), and the rendered conversation and tool schemas as JSON, canonicalized
// so key order and number spelling never move the key. The provider identity
// itself rides on KeyInput.Plugin, not here.
type LLMRequest struct {
	Model       string
	System      string
	Temperature *float64
	MaxTokens   int
	// Messages is the rendered conversation, marshaled to JSON by the
	// caller; canonicalized before hashing.
	Messages json.RawMessage
	// Tools is the tool definitions offered to the model, marshaled to
	// JSON; nil/empty when the step offers none.
	Tools json.RawMessage
}

// noTemperature is the key component standing in for an absent temperature.
// It cannot collide with any strconv.FormatFloat output, so nil and an
// explicit numeric value never share a key.
const noTemperature = "none"

func (r LLMRequest) components() ([][]byte, error) {
	temp := noTemperature
	if r.Temperature != nil {
		temp = strconv.FormatFloat(*r.Temperature, 'g', -1, 64)
	}
	msgs, err := canonicalJSON(r.Messages)
	if err != nil {
		return nil, fmt.Errorf("canonicalizing messages: %w", err)
	}
	tools, err := canonicalJSON(r.Tools)
	if err != nil {
		return nil, fmt.Errorf("canonicalizing tools: %w", err)
	}
	return [][]byte{
		[]byte(r.Model),
		[]byte(r.System),
		[]byte(temp),
		[]byte(strconv.Itoa(r.MaxTokens)),
		msgs,
		tools,
	}, nil
}

// ToolRequest is the cache-key body of a tool step: the rendered tool
// arguments as JSON, canonicalized. The tool identity rides on
// KeyInput.Plugin.
type ToolRequest struct {
	// Input is the rendered tool arguments; canonicalized before hashing.
	Input json.RawMessage
}

func (r ToolRequest) components() ([][]byte, error) {
	input, err := canonicalJSON(r.Input)
	if err != nil {
		return nil, fmt.Errorf("canonicalizing tool input: %w", err)
	}
	return [][]byte{input}, nil
}

// RetrieveRequest is the cache-key body of a retrieve step: the rendered
// query and the result count. The retriever identity rides on
// KeyInput.Plugin.
type RetrieveRequest struct {
	Query string
	TopK  int
}

// components has no failure mode of its own (query and top_k need no
// canonicalization), but keeps the Request signature the JSON-bearing
// bodies need.
//
//nolint:unparam // interface signature shared with LLMRequest/ToolRequest
func (r RetrieveRequest) components() ([][]byte, error) {
	return [][]byte{
		[]byte(r.Query),
		[]byte(strconv.Itoa(r.TopK)),
	}, nil
}

// Key computes the content cache key: the hex SHA-256 over the header
// components (this builder's version, the definition schema version, the
// scope and run id, the executor and concrete plugin identities) followed by
// the request body's canonical components. Components are length-prefixed
// before hashing, so no component's bytes can be mistaken for a boundary —
// strictly stronger than a delimiter, and what makes the separator-ambiguity
// property hold. An unusable input (missing plugin identity, run scope with
// no run id, malformed request JSON) is an error, never a silent weak key;
// the caller treats it as "do not cache this step".
func Key(in KeyInput) (string, error) {
	if in.Executor.empty() {
		return "", errors.New("cache: key input has no executor plugin identity")
	}
	if in.Plugin.empty() {
		return "", errors.New("cache: key input has no plugin identity")
	}
	if in.Request == nil {
		return "", errors.New("cache: key input has no request body")
	}
	scope := in.Scope
	if scope == "" {
		scope = dag.CacheGlobal
	}
	runID := ""
	switch scope {
	case dag.CacheGlobal:
		// runID stays empty — a global entry is shared across runs.
	case dag.CacheRun:
		if in.RunID == "" {
			return "", errors.New("cache: run-scoped key requires a run id")
		}
		runID = in.RunID
	default:
		return "", fmt.Errorf("cache: unknown cache scope %q", scope)
	}

	body, err := in.Request.components()
	if err != nil {
		return "", err
	}

	comps := [][]byte{
		[]byte(strconv.Itoa(KeySchemaVersion)),
		[]byte(strconv.Itoa(in.SchemaVersion)),
		[]byte(scope),
		[]byte(runID),
		[]byte(in.Executor.Kind),
		[]byte(in.Executor.Name),
		[]byte(in.Executor.Version),
		[]byte(in.Plugin.Kind),
		[]byte(in.Plugin.Name),
		[]byte(in.Plugin.Version),
	}
	comps = append(comps, body...)
	return hashComponents(comps), nil
}

// RedisKey builds the storage key an entry lives under: the configured
// prefix, this builder's version, and the concrete plugin's kind and name,
// then the content key. The version and kind/name segments give 9.6 its
// bust-by-prefix granularity — one provider/tool/retriever, or everything
// under the prefix.
func RedisKey(prefix string, p PluginRef, key string) string {
	return prefix + ":v" + strconv.Itoa(KeySchemaVersion) + ":" + string(p.Kind) + ":" + p.Name + ":" + key
}

// hashComponents hashes a list of byte components with unambiguous
// length-prefix framing: an 8-byte big-endian length precedes each
// component, so two different component boundaries can never hash equal.
func hashComponents(comps [][]byte) string {
	h := sha256.New()
	var lenBuf [8]byte
	for _, c := range comps {
		binary.BigEndian.PutUint64(lenBuf[:], uint64(len(c)))
		h.Write(lenBuf[:])
		h.Write(c)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// canonicalJSON round-trips raw JSON through Go's decoder so semantically
// equal documents serialize identically — object keys sorted, number
// spelling normalized (1, 1.0, 1e0 all become 1). nil/empty becomes the
// stable sentinel "null", distinct from any object or array. Integers above
// 2^53 lose precision and duplicate object keys collapse (the round-trip's
// documented limits, ADR-011), acceptable for a cache key over rendered
// requests.
func canonicalJSON(raw json.RawMessage) ([]byte, error) {
	if len(raw) == 0 {
		return []byte("null"), nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return json.Marshal(v)
}
