package engine

import (
	"context"
	"testing"

	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/gen"
)

// NotifyApproval exposes the notification path for the 15.5 integration test:
// re-invoking it for an already-delivered approval must short-circuit on the
// side-effect journal (no second POST).
func (e *Engine) NotifyApproval(ctx context.Context, step gen.RunStep, approval store.ApprovalRow, defName string) {
	e.notifyApproval(ctx, step, approval, defName)
}

// Failpoint stages, re-exported for the single-transaction completion
// tests.
const (
	StageAfterStepTransition = stageAfterStepTransition
	StageAfterExpand         = stageAfterExpand
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
