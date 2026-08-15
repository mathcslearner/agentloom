package tokens

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/mathcslearner/agentloom/internal/llm"
)

// fixture is one recorded/reference token-count corpus entry: a unified
// request plus the ground-truth input-token count it should produce for the
// named provider/model. OpenAI fixtures carry a "reference" count derived from
// tiktoken (OpenAI's real tokenizer) plus documented chat framing; Anthropic
// fixtures carry a "recorded" count from the free count_tokens API, or
// "pending" (0) until the gated recorder fills them.
type fixture struct {
	Name                string       `json:"name"`
	Provider            string       `json:"provider"`
	Model               string       `json:"model"`
	RecordedInputTokens int          `json:"recorded_input_tokens"`
	Source              string       `json:"source"`
	Note                string       `json:"note,omitempty"`
	System              string       `json:"system,omitempty"`
	Messages            []fixtureMsg `json:"messages"`
}

type fixtureMsg struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

// request builds the unified llm.ChatRequest the fixture describes.
func (f fixture) request() llm.ChatRequest {
	req := llm.ChatRequest{Model: f.Model, System: f.System, MaxTokens: 1024}
	for _, m := range f.Messages {
		req.Messages = append(req.Messages, llm.Message{
			Role:   llm.Role(m.Role),
			Blocks: []llm.Block{llm.TextBlock(m.Text)},
		})
	}
	return req
}

// loadFixtures reads and decodes every *.json fixture under
// testdata/<provider>, sorted by filename for deterministic iteration.
func loadFixtures(t *testing.T, provider string) []fixture {
	t.Helper()
	dir := filepath.Join("testdata", provider)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read fixtures dir %s: %v", dir, err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".json" {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	var out []fixture
	for _, name := range names {
		b, err := os.ReadFile(filepath.Join(dir, name)) // #nosec G304 -- committed fixture path, test-only
		if err != nil {
			t.Fatalf("read fixture %s: %v", name, err)
		}
		var f fixture
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&f); err != nil {
			t.Fatalf("decode fixture %s: %v", name, err)
		}
		out = append(out, f)
	}
	return out
}
