package tokens

import (
	"fmt"
	"math"

	tiktoken "github.com/pkoukk/tiktoken-go"

	"github.com/mathcslearner/agentloom/internal/llm"
)

// anthropicVersion bumps when the Anthropic framing constants, the base
// encoding, or the calibration derivation change.
const anthropicVersion = 1

// AnthropicCalibrationFactor scales the o200k_base BPE count into an estimate
// of Claude's token count, which Anthropic does not publish a tokenizer for.
// It is derived once, offline, by least-squares over a recorded count_tokens
// corpus (the gated TestRecordTokenCorpus regenerates the fixtures and this
// constant is re-derived from them; TestAnthropicCalibrationFactorMatchesCorpus
// guards against >0.5% drift). The default seed reflects Claude counting
// modestly more tokens than o200k on typical mixed content; the ±5% headroom
// in ADR-014's default context budget is sized to absorb the residual slop.
//
// This is a SEED value pending a live recording pass; see docs/progress.md.
const AnthropicCalibrationFactor = 1.15

// anthropicFraming mirrors OpenAI's chat accounting (per-message + reply
// priming). Claude's real framing is not published; the calibration factor
// absorbs the aggregate difference, so the framing here only needs to be the
// same shape the factor was derived against.
var anthropicFraming = framing{perMessage: 3, perTool: 8, reply: 3}

// anthropicCounter estimates Claude token counts: it counts with the o200k
// BPE (the closest public reference) and multiplies by AnthropicCalibrationFactor.
type anthropicCounter struct {
	enc    *tiktoken.Tiktoken
	factor float64
}

func newAnthropicCounter() (*anthropicCounter, error) {
	enc, err := getEncoding(encO200kBase)
	if err != nil {
		return nil, fmt.Errorf("tokens: anthropic counter: %w", err)
	}
	return &anthropicCounter{enc: enc, factor: AnthropicCalibrationFactor}, nil
}

func (c *anthropicCounter) ID() string {
	return fmt.Sprintf("anthropic/estimate@%d;factor=%g", anthropicVersion, c.factor)
}

// Count returns the calibrated estimate: the o200k count scaled by the factor,
// rounded to the nearest whole token (never below 1 for non-empty text).
func (c *anthropicCounter) Count(text string) int {
	if text == "" {
		return 0
	}
	base := bpeCount(c.enc, text)
	est := int(math.Round(float64(base) * c.factor))
	if est < 1 {
		est = 1
	}
	return est
}

func (c *anthropicCounter) CountRequest(req llm.ChatRequest) int {
	return countRequest(req, anthropicFraming, c.Count)
}
