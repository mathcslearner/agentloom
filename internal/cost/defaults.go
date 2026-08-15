package cost

import (
	_ "embed"
	"fmt"
	"sync"
)

// defaultsJSON is the embedded default pricing catalog compiled into the
// binary (ADR-012's "embedded defaults + file/env override"). Its per-model
// rates are illustrative starting points, effective-dated so historical runs
// keep their price when a rate is later corrected; a production deploy
// overrides them from real billing data via AGENTLOOM_PRICING[_FILE]. The
// "mock:*" wildcard prices the deterministic mock provider at synthetic rates
// so the M10 exit-criterion workflow shows accurate, reproducible cost fully
// offline (no keys, no network) on compose and in CI.
//
//go:embed defaults.json
var defaultsJSON []byte

var (
	defaultOnce sync.Once
	defaultCat  *Catalog
	defaultErr  error
)

// Default returns the embedded default catalog, parsed once and cached. It
// errors only if the embedded document is itself invalid — a build-time
// mistake the defaults test guards against — so in practice it always
// succeeds. The returned catalog is shared and treated as immutable; Merge
// copies before overlaying, so an override never mutates it.
func Default() (*Catalog, error) {
	defaultOnce.Do(func() {
		defaultCat, defaultErr = Parse(defaultsJSON)
		if defaultErr != nil {
			defaultErr = fmt.Errorf("cost: embedded default catalog is invalid: %w", defaultErr)
		}
	})
	return defaultCat, defaultErr
}
