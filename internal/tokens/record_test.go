package tokens

import (
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mathcslearner/agentloom/internal/llm"
)

// tokensRecord, when set, makes TestRecordTokenCorpus rewrite the testdata
// fixtures with real recorded input-token counts.
var tokensRecord = flag.Bool("record", false, "rewrite tokens testdata fixtures with real recorded counts")

// TestRecordTokenCorpus populates the fixture corpus with real input-token
// counts from the providers, and reports the calibration factor the recorded
// Anthropic corpus implies. It is gated: it runs only with LIVE_LLM_TESTS=1,
// a provider key in the environment, and -record. It is the tool that turns
// the "pending" Anthropic fixtures into "recorded" ones so
// TestAccuracyAgainstFixtures asserts real ±5% accuracy, and re-derives
// AnthropicCalibrationFactor.
//
//	LIVE_LLM_TESTS=1 AGENTLOOM_ANTHROPIC_API_KEY=... AGENTLOOM_OPENAI_API_KEY=... \
//	    go test ./internal/tokens -run TestRecordTokenCorpus -record -v
//
// Ground truth is the provider's own Usage.InputTokens on a real (tiny) Chat
// call — exactly what the provider billed for the prompt.
func TestRecordTokenCorpus(t *testing.T) {
	if os.Getenv("LIVE_LLM_TESTS") != "1" {
		t.Skip("live recorder disabled; set LIVE_LLM_TESTS=1 to enable")
	}
	if !*tokensRecord {
		t.Skip("recorder is read-only without -record; pass -record to rewrite fixtures")
	}

	providers := map[string]llm.Provider{}
	if key := firstEnv("AGENTLOOM_ANTHROPIC_API_KEY", "ANTHROPIC_API_KEY"); key != "" {
		p, err := llm.NewAnthropic(llm.AnthropicConfig{APIKey: key})
		if err != nil {
			t.Fatalf("NewAnthropic: %v", err)
		}
		providers["anthropic"] = p
	}
	if key := firstEnv("AGENTLOOM_OPENAI_API_KEY", "OPENAI_API_KEY"); key != "" {
		p, err := llm.NewOpenAI(llm.OpenAIConfig{APIKey: key})
		if err != nil {
			t.Fatalf("NewOpenAI: %v", err)
		}
		providers["openai"] = p
	}
	if len(providers) == 0 {
		t.Skip("no provider keys in environment; set AGENTLOOM_ANTHROPIC_API_KEY and/or AGENTLOOM_OPENAI_API_KEY")
	}

	enc, err := getEncoding(encO200kBase)
	if err != nil {
		t.Fatalf("getEncoding: %v", err)
	}

	for providerName, provider := range providers {
		var sumBase, sumRecorded float64
		dir := filepath.Join("testdata", providerName)
		fixtures := loadFixtures(t, providerName)
		for _, f := range fixtures {
			req := f.request()
			req.MaxTokens = 16 // small completion; we only want InputTokens
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			resp, err := provider.Chat(ctx, req)
			cancel()
			if err != nil {
				t.Logf("%s/%s: Chat failed (%v); leaving fixture unchanged", providerName, f.Name, err)
				continue
			}
			f.RecordedInputTokens = int(resp.Usage.InputTokens)
			f.Source = "recorded"
			writeFixture(t, dir, f)
			base := countRequest(req, anthropicFraming, func(s string) int { return len(enc.EncodeOrdinary(s)) })
			sumBase += float64(base)
			sumRecorded += float64(f.RecordedInputTokens)
			t.Logf("%s/%s: recorded %d input tokens (o200k base %d)", providerName, f.Name, f.RecordedInputTokens, base)
		}
		if providerName == "anthropic" && sumBase > 0 {
			t.Logf("anthropic corpus implies AnthropicCalibrationFactor = %.4f (currently %.4f); update the constant if it drifts >0.5%%",
				sumRecorded/sumBase, AnthropicCalibrationFactor)
		}
	}
}

func firstEnv(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

func writeFixture(t *testing.T, dir string, f fixture) {
	t.Helper()
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		t.Fatalf("marshal fixture %s: %v", f.Name, err)
	}
	b = append(b, '\n')
	path := filepath.Join(dir, f.Name+".json")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatalf("write fixture %s: %v", path, err)
	}
}
