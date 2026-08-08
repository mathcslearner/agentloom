package store

import "testing"

// Failpoint stages, re-exported for the instantiation tests.
const (
	StageAfterRunInsert = stageAfterRunInsert
	StageAfterSteps     = stageAfterSteps
	StageAfterEdges     = stageAfterEdges
	StageAfterEvents    = stageAfterEvents
)

// SetInstantiateFailpoint installs fn as the instantiation failpoint for
// the duration of tb. The hook is a package global, so tests that arm it
// must not run in parallel with any other test calling CreateRun.
func SetInstantiateFailpoint(tb testing.TB, fn func(stage string) error) {
	tb.Helper()
	instantiateFailpoint = fn
	tb.Cleanup(func() { instantiateFailpoint = nil })
}
