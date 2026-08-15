package engine

import (
	"testing"

	"github.com/mathcslearner/agentloom/internal/store"
)

// TestChooseDowngrade covers the pure tier selection over pre-priced
// candidates: soft thresholds, the hard projection trigger, rescue when the
// threshold tier does not fit, and exhaustion.
func TestChooseDowngrade(t *testing.T) {
	t.Parallel()
	// cand builds a candidate; all are priceable unless said otherwise.
	cand := func(fits, threshold bool) downgradeCandidate {
		return downgradeCandidate{priceable: true, fits: fits, thresholdMet: threshold}
	}
	unpriceable := downgradeCandidate{priceable: false}

	cases := []struct {
		name        string
		cands       []downgradeCandidate
		primaryFits bool
		wantIdx     int
		wantTrigger string
	}{
		{
			name:        "no trigger while primary fits and no threshold met",
			cands:       []downgradeCandidate{cand(true, false), cand(true, false)},
			primaryFits: true,
			wantIdx:     -1,
		},
		{
			name:        "threshold picks the deepest met fitting tier",
			cands:       []downgradeCandidate{cand(true, true), cand(true, true)},
			primaryFits: true, // proactive: downgrade even though primary fits
			wantIdx:     1,
			wantTrigger: store.DowngradeTriggerThreshold,
		},
		{
			name:        "threshold prefix: only the shallow tier is met",
			cands:       []downgradeCandidate{cand(true, true), cand(true, false)},
			primaryFits: true,
			wantIdx:     0,
			wantTrigger: store.DowngradeTriggerThreshold,
		},
		{
			name:        "projection routes to the least-aggressive fitting tier",
			cands:       []downgradeCandidate{cand(false, false), cand(true, false)},
			primaryFits: false,
			wantIdx:     1,
			wantTrigger: store.DowngradeTriggerProjection,
		},
		{
			name:        "projection prefers the most expensive tier that fits",
			cands:       []downgradeCandidate{cand(true, false), cand(true, false)},
			primaryFits: false,
			wantIdx:     0,
			wantTrigger: store.DowngradeTriggerProjection,
		},
		{
			name:        "rescue: threshold tier does not fit, drop to a fitting tier",
			cands:       []downgradeCandidate{cand(false, true), cand(true, true)},
			primaryFits: true,
			wantIdx:     1,
			wantTrigger: store.DowngradeTriggerThreshold,
		},
		{
			name:        "exhausted: nothing fits, no downgrade",
			cands:       []downgradeCandidate{cand(false, false), cand(false, true)},
			primaryFits: false,
			wantIdx:     -1,
		},
		{
			name:        "unpriceable deepest tier is skipped for a fitting one",
			cands:       []downgradeCandidate{cand(true, true), unpriceable},
			primaryFits: true,
			wantIdx:     0,
			wantTrigger: store.DowngradeTriggerThreshold,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			idx, trigger := chooseDowngrade(tc.cands, tc.primaryFits)
			if idx != tc.wantIdx {
				t.Errorf("idx = %d, want %d", idx, tc.wantIdx)
			}
			if idx >= 0 && trigger != tc.wantTrigger {
				t.Errorf("trigger = %q, want %q", trigger, tc.wantTrigger)
			}
		})
	}
}
