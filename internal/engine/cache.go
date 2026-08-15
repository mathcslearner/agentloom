package engine

// This file is the ticket 9.5 response-cache middleware (ADR-011): the
// read-through/write-through layer that sits AHEAD of the ADR-010 rate
// limiter in the executor chain. On a cache hit the step completes from the
// stored result without acquiring a rate-limit token or calling the provider
// — the headline win. On a miss the step executes normally and, on success,
// its result is written through for the next identical request.
//
// The cache is disposable derived data (ADR-011): every error path here is
// fail-safe. A missing binder, a policy that declines, an unbuildable key, a
// corrupt stored value, or a Redis error all resolve to "execute the step
// uncached" — never a run failure. The dag/cache leaf package owns the key
// and the policy; this file owns the storage seam, the hit accounting, and
// the failure routing.

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/mathcslearner/agentloom/internal/cache"
	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/exec"
	"github.com/mathcslearner/agentloom/internal/obs/log"
	"github.com/mathcslearner/agentloom/internal/store/gen"
)

// ResponseCache is the engine's seam onto the Redis response-cache store
// (ticket 9.5, ADR-011) — satisfied by *cache/redisstore.Store. The
// middleware reads a hit ahead of the limiter and writes a miss through
// after a successful execution. Nil disables caching entirely (the default
// for every test layer that doesn't opt in): the middleware is then a no-op
// and every step executes uncached.
//
// Get returns (value, true, nil) on a hit, (nil, false, nil) on a miss, and
// (nil, false, err) on a store error — the middleware treats a miss and an
// error identically (execute), but the error is counted as a fail-open. Set
// writes with the TTL; an oversized value is skipped (redisstore.ErrValueTooLarge),
// which the middleware records as a bypass.
type ResponseCache interface {
	Get(ctx context.Context, plugin cache.PluginRef, key string) ([]byte, bool, error)
	Set(ctx context.Context, plugin cache.PluginRef, key string, val []byte, ttl time.Duration) error
}

// cacheEntry is the stored value (ADR-011): the step's output payload, the
// usage snapshot the original miss recorded, and the cost resource that miss
// billed to. On a hit the usage becomes the attempt's counterfactual
// "would-have-cost" accounting (M10), marked cache_hit; the resource lets the
// hit price that counterfactual into a "saved" figure (ticket 10.2); the
// output is served verbatim. Resource is omitempty so a pre-10.2 entry
// (written before the field existed) decodes cleanly to "" — the hit still
// serves, it just ledgers no saved figure.
type cacheEntry struct {
	Output   json.RawMessage `json:"output"`
	Usage    *exec.Usage     `json:"usage,omitempty"`
	Resource string          `json:"cost_resource,omitempty"`
}

// cacheWriteBinding carries what a miss needs to write its result through
// after a successful execution: the concrete plugin (Redis namespace), the
// content key, the TTL, and the bounded plugin label for the store metric.
type cacheWriteBinding struct {
	plugin      cache.PluginRef
	key         string
	ttl         time.Duration
	pluginLabel string
}

// cacheRead is the read-through half, called in execute() before the rate
// limiter. It returns handled=true only on a hit — the step was completed
// from cache, and the returned error is the completion's Handle/ACK result;
// no limiter acquire or executor invocation happens. On any other outcome
// (no cache wired, no binder, a declined policy, an unbuildable key, a store
// error, a miss) it returns handled=false with a *cacheWriteBinding when the
// policy writes (nil otherwise), which execute() threads to the write-through
// after a successful execution.
func (e *Engine) cacheRead(ctx context.Context, step gen.RunStep, executor exec.Executor, sc exec.StepContext) (bool, *cacheWriteBinding, error) {
	if e.cache == nil {
		return false, nil, nil
	}
	binder, ok := executor.(exec.CacheBinder)
	if !ok {
		return false, nil, nil // this step type is never cacheable
	}
	logger := log.From(ctx)
	binding, err := binder.CacheBinding(sc)
	if err != nil {
		// The binding could not be computed (unroutable model, corrupt
		// config). Don't cache and don't classify — let the executor run and
		// land its own failure, the same convention rateLimit uses.
		logger.WarnContext(ctx, "cache binding failed; executing uncached",
			slog.Any("error", err))
		e.metrics.CacheBypass(cacheUnknownPlugin)
		return false, nil, nil
	}
	if binding == nil {
		return false, nil, nil // executor declared this step non-cacheable
	}
	plabel := string(binding.Plugin.Kind) + ":" + binding.Plugin.Name

	policy, perr := decodeCachePolicy(step.CachePolicy)
	if perr != nil {
		// Corrupt materialized policy. Unlike a corrupt timeout (which
		// perm-fails), the cache is advisory — bypass and execute.
		logger.WarnContext(ctx, "corrupt materialized cache policy; executing uncached",
			slog.Any("error", perr))
		e.metrics.CacheBypass(plabel)
		return false, nil, nil
	}
	dec := cache.Decide(binding.Caps, binding.Deterministic, policy, e.cacheDefaultTTL)
	if !dec.Read && !dec.Write {
		e.metrics.CacheBypass(plabel)
		return false, nil, nil
	}

	key, kerr := cache.Key(cache.KeyInput{
		SchemaVersion: dag.CurrentSchemaVersion,
		Executor:      binding.Executor,
		Plugin:        binding.Plugin,
		Scope:         dec.Scope,
		RunID:         step.RunID.String(),
		Request:       binding.Request,
	})
	if kerr != nil {
		// An unusable key means "do not cache this step" (ADR-011).
		logger.WarnContext(ctx, "cache key unbuildable; executing uncached",
			slog.Any("error", kerr))
		e.metrics.CacheBypass(plabel)
		return false, nil, nil
	}

	var wb *cacheWriteBinding
	if dec.Write {
		wb = &cacheWriteBinding{plugin: binding.Plugin, key: key, ttl: dec.TTL, pluginLabel: plabel}
	}

	if dec.Read {
		raw, hit, gerr := e.cache.Get(ctx, binding.Plugin, key)
		switch {
		case gerr != nil:
			// A store error is fail-open: proceed as a miss, still allowing a
			// later write-through to repopulate.
			e.metrics.CacheFailOpen()
			logger.WarnContext(ctx, "cache read failed; executing (fail-open)",
				slog.Any("error", gerr))
			return false, wb, nil
		case hit:
			out, derr := decodeCacheEntry(raw)
			if derr != nil {
				// A corrupt entry is a miss, not a failure — execute and let
				// the write-through overwrite it.
				e.metrics.CacheFailOpen()
				logger.WarnContext(ctx, "corrupt cache entry; executing (fail-open)",
					slog.Any("error", derr))
				return false, wb, nil
			}
			e.metrics.CacheHit(plabel)
			logger.InfoContext(ctx, "response cache hit; skipping rate limiter and provider",
				slog.String("plugin", plabel))
			// The served attempt records counterfactual usage marked cache_hit
			// (M10), never a real debit; a hit never touches the rate limiter.
			out.Usage = markCacheHit(out.Usage)
			return true, nil, e.completeSuccess(ctx, step, out)
		}
	}
	e.metrics.CacheMiss(plabel)
	return false, wb, nil
}

// cacheWrite is the write-through half, called in execute() after a
// successful execution on a miss whose policy writes. It stores the result
// under the miss's key with the policy TTL. Every failure is swallowed: an
// oversized value is a skip (bypass metric, ADR-011's size-cap fallback), a
// store error is a fail-open. Writing before the completion transaction is
// safe by construction — the entry is a pure function of the resolved
// request, so even a later-fenced completion wrote a correct entry.
func (e *Engine) cacheWrite(ctx context.Context, wb *cacheWriteBinding, out exec.Output) {
	logger := log.From(ctx)
	val, err := json.Marshal(cacheEntry{Output: out.Data, Usage: out.Usage, Resource: out.Resource})
	if err != nil {
		// A marshal failure on the fixed entry shape is not worth failing a
		// succeeded step over — skip the write.
		logger.WarnContext(ctx, "marshaling cache entry failed; result not cached",
			slog.Any("error", err))
		return
	}
	switch err := e.cache.Set(ctx, wb.plugin, wb.key, val, wb.ttl); {
	case err == nil:
		e.metrics.CacheStore(wb.pluginLabel)
		logger.DebugContext(ctx, "response cache write-through",
			slog.String("plugin", wb.pluginLabel), slog.Duration("ttl", wb.ttl))
	case errors.Is(err, cache.ErrValueTooLarge):
		// ADR-011: an oversized value is left to re-compute, recorded as a
		// bypass (a store-skip), never chunked or spilled.
		e.metrics.CacheBypass(wb.pluginLabel)
		logger.InfoContext(ctx, "cache value over size cap; skipped (will re-compute)",
			slog.String("plugin", wb.pluginLabel), slog.Int("bytes", len(val)))
	default:
		e.metrics.CacheFailOpen()
		logger.WarnContext(ctx, "cache write failed; result not cached (fail-open)",
			slog.Any("error", err))
	}
}

// cacheUnknownPlugin is the bounded plugin label for a bypass recorded
// before the concrete plugin identity is known (a binder error). It keeps
// the bypass counter's label set finite even when the binding failed.
const cacheUnknownPlugin = "unknown"

// decodeCachePolicy decodes the materialized run_steps.cache_policy JSONB
// into the authored policy. Empty means the step authored no `cache` block
// (nil policy → engine default). It was validated and canonically encoded at
// submit, so a plain decode suffices.
func decodeCachePolicy(raw json.RawMessage) (*dag.CachePolicy, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var p dag.CachePolicy
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// decodeCacheEntry decodes a stored value into the entry.
func decodeCacheEntry(raw []byte) (exec.Output, error) {
	var ent cacheEntry
	if err := json.Unmarshal(raw, &ent); err != nil {
		return exec.Output{}, err
	}
	return exec.Output{Data: ent.Output, Usage: ent.Usage, Resource: ent.Resource}, nil
}

// markCacheHit returns the usage a cache-served attempt records: the stored
// snapshot's token counts (counterfactual — not spent) with CacheHit set, so
// M10 can meter the savings. A hit with no stored usage (a non-llm step)
// still records cache_hit=true with zero tokens, the durable signal that the
// attempt was served from cache.
func markCacheHit(u *exec.Usage) *exec.Usage {
	if u == nil {
		return &exec.Usage{CacheHit: true}
	}
	return &exec.Usage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, CacheHit: true}
}
