package engine

import "testing"

// Failpoint stages, re-exported for the single-transaction completion
// tests.
const (
	StageAfterStepTransition = stageAfterStepTransition
	StageAfterFanOut         = stageAfterFanOut
	StageAfterOutbox         = stageAfterOutbox
)

// SetCompleteFailpoint installs fn as the completion failpoint for the
// duration of tb (same pattern as store.SetInstantiateFailpoint). The hook
// is a package global — a test that arms it must not run in parallel with
// other tests driving completion transactions, or it will abort those too.
func SetCompleteFailpoint(tb testing.TB, fn func(stage string) error) {
	tb.Helper()
	completeFailpoint.Store(&fn)
	tb.Cleanup(func() { completeFailpoint.Store(nil) })
}
