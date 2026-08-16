package contextmgr

import "testing"

func TestHeadroom(t *testing.T) {
	t.Parallel()
	cases := []struct {
		window int
		want   int
	}{
		{0, 0},
		{-5, 0},
		{100, 64},       // 5% = 5, floored to 64
		{1024, 64},      // 5% = 52 (ceil), floored to 64
		{2000, 100},     // 5% = 100
		{200000, 10000}, // 5% = 10000
		{10, 10},        // floor exceeds window → clamped to window
	}
	for _, tc := range cases {
		if got := Headroom(tc.window); got != tc.want {
			t.Errorf("Headroom(%d) = %d, want %d", tc.window, got, tc.want)
		}
	}
}

func TestDefaultBudget(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		window     int
		maxTokens  int
		wantBudget int
		wantOK     bool
	}{
		{"ample room", 200000, 1024, 200000 - 1024 - 10000, true},
		{"small window", 1024, 256, 1024 - 256 - 64, true},
		{"max_tokens eats window", 1024, 1000, 0, false},
		{"max_tokens plus headroom exactly window", 1024, 1024 - 64, 0, false},
		{"zero window", 0, 128, 0, false},
		{"negative max_tokens treated as zero", 2000, -5, 2000 - 100, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b, ok := DefaultBudget(tc.window, tc.maxTokens)
			if ok != tc.wantOK || (ok && b != tc.wantBudget) {
				t.Errorf("DefaultBudget(%d,%d) = (%d,%v), want (%d,%v)", tc.window, tc.maxTokens, b, ok, tc.wantBudget, tc.wantOK)
			}
		})
	}
}

func TestEffectiveBudget(t *testing.T) {
	t.Parallel()
	ptr := func(n int) *int { return &n }
	cases := []struct {
		name       string
		explicit   *int
		def        int
		hasDefault bool
		wantBudget int
		wantSrc    BudgetSource
		wantOK     bool
	}{
		{"nothing", nil, 0, false, 0, BudgetSourceNone, false},
		{"window only", nil, 5000, true, 5000, BudgetSourceWindow, true},
		{"explicit only (no window)", ptr(3000), 0, false, 3000, BudgetSourceExplicit, true},
		{"explicit tighter than window", ptr(3000), 5000, true, 3000, BudgetSourceExplicit, true},
		{"explicit equal to window", ptr(5000), 5000, true, 5000, BudgetSourceExplicit, true},
		{"explicit looser than window capped", ptr(9000), 5000, true, 5000, BudgetSourceExplicitCapped, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b, src, ok := EffectiveBudget(tc.explicit, tc.def, tc.hasDefault)
			if ok != tc.wantOK || src != tc.wantSrc || (ok && b != tc.wantBudget) {
				t.Errorf("EffectiveBudget = (%d,%v,%v), want (%d,%v,%v)", b, src, ok, tc.wantBudget, tc.wantSrc, tc.wantOK)
			}
		})
	}
}
