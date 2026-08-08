package store

import "testing"

// Failpoint stages, re-exported for the instantiation tests.
const (
	StageAfterRunInsert = stageAfterRunInsert
	StageAfterSteps     = stageAfterSteps
	StageAfterEdges     = stageAfterEdges
	StageAfterEvents    = stageAfterEvents
	StageAfterOutbox    = stageAfterOutbox
)

// SetInstantiateFailpoint installs fn as the instantiation failpoint for
// the duration of tb. The hook is a package global (atomically swapped, so
// never a data race) — but a test that arms it still must not run in
// parallel with other tests calling CreateRun, or it will abort their
// transactions too.
func SetInstantiateFailpoint(tb testing.TB, fn func(stage string) error) {
	tb.Helper()
	instantiateFailpoint.Store(&fn)
	tb.Cleanup(func() { instantiateFailpoint.Store(nil) })
}

// ToMigrateDSN re-exports toMigrateDSN for its unit tests.
var ToMigrateDSN = toMigrateDSN
