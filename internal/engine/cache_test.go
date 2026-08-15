package engine

import (
	"encoding/json"
	"testing"

	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/exec"
)

// TestDecodeCachePolicy: empty → nil; a materialized policy round-trips.
func TestDecodeCachePolicy(t *testing.T) {
	t.Parallel()
	if p, err := decodeCachePolicy(nil); err != nil || p != nil {
		t.Fatalf("decodeCachePolicy(nil) = %+v, %v; want nil, nil", p, err)
	}
	raw := json.RawMessage(`{"mode":"read_write","ttl":"1h","scope":"run"}`)
	p, err := decodeCachePolicy(raw)
	if err != nil {
		t.Fatalf("decodeCachePolicy: %v", err)
	}
	if p == nil || p.Mode != dag.CacheReadWrite || p.TTL != "1h" || p.Scope != dag.CacheRun {
		t.Errorf("decoded policy = %+v", p)
	}
	if _, err := decodeCachePolicy(json.RawMessage(`{`)); err == nil {
		t.Error("decodeCachePolicy on malformed JSON = nil error, want an error")
	}
}

// TestDecodeCacheEntry: a stored entry decodes to its output and usage.
func TestDecodeCacheEntry(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"output":{"text":"hi"},"usage":{"input_tokens":3,"output_tokens":5}}`)
	out, err := decodeCacheEntry(raw)
	if err != nil {
		t.Fatalf("decodeCacheEntry: %v", err)
	}
	if string(out.Data) != `{"text":"hi"}` {
		t.Errorf("output = %s", out.Data)
	}
	if out.Usage == nil || out.Usage.InputTokens != 3 || out.Usage.OutputTokens != 5 {
		t.Errorf("usage = %+v", out.Usage)
	}
	if _, err := decodeCacheEntry([]byte(`nonsense`)); err == nil {
		t.Error("decodeCacheEntry on garbage = nil error, want an error")
	}
}

// TestMarkCacheHit: the served attempt's usage carries the snapshot counts
// with cache_hit set; a hit with no snapshot still records cache_hit.
func TestMarkCacheHit(t *testing.T) {
	t.Parallel()
	got := markCacheHit(&exec.Usage{InputTokens: 7, OutputTokens: 11})
	if got.InputTokens != 7 || got.OutputTokens != 11 || !got.CacheHit {
		t.Errorf("markCacheHit(usage) = %+v, want the counts with CacheHit true", got)
	}
	none := markCacheHit(nil)
	if none.InputTokens != 0 || none.OutputTokens != 0 || !none.CacheHit {
		t.Errorf("markCacheHit(nil) = %+v, want zero tokens with CacheHit true", none)
	}
	// The marker serializes into the attempt usage JSON.
	b, _ := json.Marshal(got)
	if !json.Valid(b) || !containsCacheHit(b) {
		t.Errorf("marshaled usage = %s, want a cache_hit field", b)
	}
}

func containsCacheHit(b []byte) bool {
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return false
	}
	v, ok := m["cache_hit"].(bool)
	return ok && v
}
