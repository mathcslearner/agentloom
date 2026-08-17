package engine

import "testing"

// The crash seam's matching case calls os.Exit, so these white-box tests cover
// everything up to the exit: env parsing, and that maybeCrash is a no-op when
// disarmed or when the stage/step does not match. The kill-at-boundary matrix
// (test/crash) exercises the actual process exit against a real cmd/worker.

func TestInstallCrashPointFromEnvParsing(t *testing.T) {
	// crashPoint is a package global; restore it after each case so a stray
	// arming can never leak into another test (which could os.Exit the binary).
	t.Cleanup(func() { crashPoint.Store(nil) })

	cases := []struct {
		name       string
		value      string
		wantArmed  bool
		wantStage  string
		wantStepID string
	}{
		{name: "empty disarms", value: "", wantArmed: false},
		{name: "whitespace disarms", value: "   ", wantArmed: false},
		{name: "no separator disarms", value: "post_commit", wantArmed: false},
		{name: "empty stage disarms", value: ":plan", wantArmed: false},
		{name: "empty step disarms", value: "post_commit:", wantArmed: false},
		{name: "valid arms", value: "post_commit:plan", wantArmed: true, wantStage: CrashStagePostCommit, wantStepID: "plan"},
		{name: "instance id arms", value: "after_expand:process#gather", wantArmed: true, wantStage: CrashStageAfterExpand, wantStepID: "process#gather"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			crashPoint.Store(nil)
			InstallCrashPointFromEnv(func(k string) string {
				if k != EnvWorkerCrashPoint {
					t.Fatalf("read unexpected env %q", k)
				}
				return tc.value
			})
			got := crashPoint.Load()
			if tc.wantArmed {
				if got == nil {
					t.Fatalf("value %q left the seam disarmed, want armed", tc.value)
				}
				if got.stage != tc.wantStage || got.stepID != tc.wantStepID {
					t.Fatalf("value %q armed {%q,%q}, want {%q,%q}", tc.value, got.stage, got.stepID, tc.wantStage, tc.wantStepID)
				}
			} else if got != nil {
				t.Fatalf("value %q armed {%q,%q}, want disarmed", tc.value, got.stage, got.stepID)
			}
		})
	}
}

func TestMaybeCrashNoOpWhenUnarmedOrMismatched(t *testing.T) {
	t.Cleanup(func() { crashPoint.Store(nil) })

	// Disarmed: no-op regardless of stage/step. (If it exited, the test binary
	// would die and the run would fail — reaching the assertions is the proof.)
	crashPoint.Store(nil)
	maybeCrash(CrashStagePostCommit, "plan")
	maybeCrash(CrashStagePreClaim, "plan")

	// Armed but mismatched on stage, then on step: still a no-op.
	crashPoint.Store(&crashDirective{stage: CrashStagePostCommit, stepID: "plan"})
	maybeCrash(CrashStagePreClaim, "plan")    // wrong stage
	maybeCrash(CrashStagePostCommit, "other") // wrong step
}
