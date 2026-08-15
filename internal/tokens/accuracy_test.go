package tokens

import (
	"math"
	"testing"
)

// accuracyTolerance is the ±5% the ROADMAP demands vs recorded real counts.
const accuracyTolerance = 0.05

// TestAccuracyAgainstFixtures asserts the selected counter is within ±5% of
// each fixture's ground-truth input-token count.
//
//   - OpenAI fixtures carry a "reference" count from tiktoken (OpenAI's real
//     tokenizer) plus documented chat framing — genuine ground truth, since
//     the counter uses that same tokenizer; these run offline.
//   - Anthropic fixtures carry a "recorded" count from the free count_tokens
//     API. Until the gated recorder fills them (they ship "pending"), the
//     Anthropic assertion is skipped with a notice rather than passing
//     vacuously against a self-derived number.
//
// The aggregate error across all non-pending fixtures is also asserted ≤ 5%.
func TestAccuracyAgainstFixtures(t *testing.T) {
	reg := NewRegistry(nil)
	for _, provider := range []string{"openai", "anthropic"} {
		t.Run(provider, func(t *testing.T) {
			fixtures := loadFixtures(t, provider)
			var scored, pending int
			var sumWant, sumErr float64
			for _, f := range fixtures {
				if f.Source == "pending" || f.RecordedInputTokens == 0 {
					pending++
					continue
				}
				scored++
				c, _ := reg.Select(f.Provider, f.Model)
				got := c.CountRequest(f.request())
				want := f.RecordedInputTokens
				relErr := math.Abs(float64(got-want)) / float64(want)
				if relErr > accuracyTolerance {
					t.Errorf("%s/%s: count=%d recorded=%d relErr=%.1f%% > %.0f%%",
						provider, f.Name, got, want, relErr*100, accuracyTolerance*100)
				}
				sumWant += float64(want)
				sumErr += math.Abs(float64(got - want))
			}
			if scored > 0 {
				if agg := sumErr / sumWant; agg > accuracyTolerance {
					t.Errorf("%s: aggregate relErr %.1f%% > %.0f%%", provider, agg*100, accuracyTolerance*100)
				}
				t.Logf("%s: %d fixtures scored within ±5%% (%d pending live recording)", provider, scored, pending)
			} else {
				t.Logf("%s: no recorded fixtures yet (%d pending); run the gated recorder to populate — see TestRecordTokenCorpus", provider, pending)
			}
		})
	}
}

// TestAnthropicCalibrationFactorMatchesCorpus guards the pinned calibration
// factor against drift from the recorded Anthropic corpus: the factor implied
// by the corpus (Σ recorded / Σ o200k-base) must be within 0.5% of the
// constant. It skips when the corpus is still pending (no recorded counts).
func TestAnthropicCalibrationFactorMatchesCorpus(t *testing.T) {
	enc, err := getEncoding(encO200kBase)
	if err != nil {
		t.Fatalf("getEncoding: %v", err)
	}
	var sumBase, sumRecorded float64
	for _, f := range loadFixtures(t, "anthropic") {
		if f.Source == "pending" || f.RecordedInputTokens == 0 {
			continue
		}
		// Base framing mirrors the counter's countRequest walk, but counting
		// with the raw o200k BPE (no calibration) — so the ratio isolates the
		// calibration factor.
		base := countRequest(f.request(), anthropicFraming, func(s string) int { return len(enc.EncodeOrdinary(s)) })
		sumBase += float64(base)
		sumRecorded += float64(f.RecordedInputTokens)
	}
	if sumBase == 0 {
		t.Skip("anthropic corpus still pending live recording; run TestRecordTokenCorpus -record with a key")
	}
	implied := sumRecorded / sumBase
	if drift := math.Abs(implied-AnthropicCalibrationFactor) / AnthropicCalibrationFactor; drift > 0.005 {
		t.Errorf("calibration factor drift: corpus implies %.4f, constant is %.4f (%.2f%% > 0.5%%); re-derive and update AnthropicCalibrationFactor",
			implied, AnthropicCalibrationFactor, drift*100)
	}
}
