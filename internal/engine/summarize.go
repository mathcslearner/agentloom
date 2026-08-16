package engine

// Summarizer wiring for the compaction stage (ticket 12.5, ADR-014). The
// summarize compaction strategy calls a cheap model to summarize an evicted
// context span; internal/contextmgr owns the strategy and the deterministic
// prompt, while this file owns the two infra concerns the leaf can't: the
// ADR-011 response cache around the (temperature-0, deterministic) summarizer
// call — so a repeated compaction of the same span is a cache hit, not a second
// billed call — and the wiring seam (WithSummarizer). The overhead ledgering and
// the audit events live in context.go, next to the rest of the assembly stage.

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/mathcslearner/agentloom/internal/cache"
	"github.com/mathcslearner/agentloom/internal/contextmgr"
	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/obs/log"
	"github.com/mathcslearner/agentloom/internal/plugin"
)

// summarizerCacheLabel is the bounded plugin label the cache metrics use for
// summarizer calls (ADR-008: metric labels stay finite).
const summarizerCacheLabel = "summarizer"

// summarizerExecutor and summarizerPlugin are the fixed identities the
// summarizer cache key is built over (ADR-011). The summarizer is not a
// per-model provider plugin from the step's config — it is a compaction
// facility with its own behavioral version — so it namespaces its cache under a
// dedicated ref. A prompt/framing change bumps SummarizerVersion and
// invalidates every cached summary.
var (
	summarizerExecutor = cache.PluginRef{Kind: plugin.KindExecutor, Name: "context_summarizer", Version: contextmgr.SummarizerVersion}
	summarizerPlugin   = cache.PluginRef{Kind: plugin.KindModelProvider, Name: "context_summarizer", Version: contextmgr.SummarizerVersion}
)

// WithSummarizer sets the summarizer the compaction stage's summarize strategy
// calls (ticket 12.5) — cmd/worker wires contextmgr.NewLLMSummarizer over the
// model registry here. Nil (the default) leaves a summarize strategy to fall
// back to the next deterministic strategy in the pipeline. When a response
// cache is also wired (WithResponseCache), the engine wraps this summarizer in
// a caching decorator so a repeated compaction of the same span is a cache hit.
func WithSummarizer(s contextmgr.Summarizer) Option {
	return func(e *Engine) { e.summarizer = s }
}

// stepSummarizer returns the summarizer the compaction stage should use: the
// wired summarizer wrapped in the response-cache decorator when a cache is
// configured, or the bare summarizer (or nil) otherwise.
func (e *Engine) stepSummarizer() contextmgr.Summarizer {
	if e.summarizer == nil {
		return nil
	}
	if e.cache == nil {
		return e.summarizer
	}
	return &cachedSummarizer{inner: e.summarizer, cache: e.cache, ttl: e.cacheDefaultTTL, metrics: e.metrics}
}

// cachedSummarizer decorates a Summarizer with the ADR-011 response cache. The
// summarizer request is deterministic (a fixed prompt at temperature 0), so it
// is a valid cache key; a hit returns the stored summary marked CacheHit (the
// engine then ledgers a $0 overhead row with the counterfactual saved figure,
// exactly like a cached provider call). Every cache error is fail-safe: a
// missing/corrupt entry or a store error just calls the inner summarizer.
type cachedSummarizer struct {
	inner   contextmgr.Summarizer
	cache   ResponseCache
	ttl     time.Duration
	metrics Metrics
}

// cachedSummary is the stored value: the summary text, the pricing resource,
// and the provider usage (the counterfactual a hit meters as saved overhead).
type cachedSummary struct {
	Text     string                   `json:"text"`
	Resource string                   `json:"resource"`
	Usage    *contextmgr.SummaryUsage `json:"usage,omitempty"`
}

func (s *cachedSummarizer) Summarize(ctx context.Context, req contextmgr.SummarizeRequest) (contextmgr.SummaryResult, error) {
	logger := log.From(ctx)
	key, kerr := s.summaryKey(req)
	if kerr != nil {
		// An unbuildable key means "do not cache this summary" (ADR-011) — call
		// the inner summarizer uncached.
		logger.WarnContext(ctx, "summarizer cache key unbuildable; summarizing uncached", slog.Any("error", kerr))
		return s.inner.Summarize(ctx, req)
	}
	if raw, hit, gerr := s.cache.Get(ctx, summarizerPlugin, key); gerr != nil {
		s.metrics.CacheFailOpen()
		logger.WarnContext(ctx, "summarizer cache read failed; summarizing (fail-open)", slog.Any("error", gerr))
	} else if hit {
		var cs cachedSummary
		if derr := json.Unmarshal(raw, &cs); derr == nil && cs.Text != "" {
			s.metrics.CacheHit(summarizerCacheLabel)
			return contextmgr.SummaryResult{Text: cs.Text, Resource: cs.Resource, Usage: cs.Usage, CacheHit: true}, nil
		}
		s.metrics.CacheFailOpen()
		logger.WarnContext(ctx, "corrupt summarizer cache entry; summarizing (fail-open)")
	} else {
		s.metrics.CacheMiss(summarizerCacheLabel)
	}

	res, err := s.inner.Summarize(ctx, req)
	if err != nil {
		return res, err
	}
	val, merr := json.Marshal(cachedSummary{Text: res.Text, Resource: res.Resource, Usage: res.Usage})
	if merr == nil {
		if serr := s.cache.Set(ctx, summarizerPlugin, key, val, s.ttl); serr == nil {
			s.metrics.CacheStore(summarizerCacheLabel)
		} else {
			s.metrics.CacheFailOpen()
			logger.WarnContext(ctx, "summarizer cache write failed (fail-open)", slog.Any("error", serr))
		}
	}
	return res, nil
}

// summaryKey builds the ADR-011 cache key for a summarizer request over the
// deterministic summarizer ChatRequest (the same request contextmgr sends).
func (s *cachedSummarizer) summaryKey(req contextmgr.SummarizeRequest) (string, error) {
	chatReq := contextmgr.SummaryChatRequest(req.Model, req.SpanText, req.MaxTokens)
	msgs, err := json.Marshal(chatReq.Messages)
	if err != nil {
		return "", err
	}
	return cache.Key(cache.KeyInput{
		SchemaVersion: dag.CurrentSchemaVersion,
		Executor:      summarizerExecutor,
		Plugin:        summarizerPlugin,
		Scope:         dag.CacheGlobal,
		Request: cache.LLMRequest{
			Model:       chatReq.Model,
			System:      chatReq.System,
			Temperature: chatReq.Temperature,
			MaxTokens:   chatReq.MaxTokens,
			Messages:    msgs,
		},
	})
}
