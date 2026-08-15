package tokens

import (
	"fmt"

	tiktoken "github.com/pkoukk/tiktoken-go"

	"github.com/mathcslearner/agentloom/internal/llm"
)

// openAIVersion bumps when the OpenAI framing constants or encoding-selection
// change, invalidating stored counts produced by an older version.
const openAIVersion = 1

// openAICounter counts OpenAI requests with the model's exact BPE (tiktoken).
// For text this is the reference tokenizer OpenAI itself uses; only the chat
// framing (openAIFraming) is an approximation, and a small one.
type openAICounter struct {
	encoding string
	enc      *tiktoken.Tiktoken
}

// newOpenAICounter builds the counter for an OpenAI model, selecting the BPE
// encoding by model prefix. It fails only if the (embedded) encoding cannot be
// loaded — a programming error, surfaced so Select can fall back rather than
// panic.
func newOpenAICounter(model string) (*openAICounter, error) {
	name := openAIEncodingFor(model)
	enc, err := getEncoding(name)
	if err != nil {
		return nil, fmt.Errorf("tokens: openai counter for model %q: %w", model, err)
	}
	return &openAICounter{encoding: name, enc: enc}, nil
}

func (c *openAICounter) ID() string {
	return fmt.Sprintf("openai/%s@%d", c.encoding, openAIVersion)
}

func (c *openAICounter) Count(text string) int { return bpeCount(c.enc, text) }

func (c *openAICounter) CountRequest(req llm.ChatRequest) int {
	return countRequest(req, openAIFraming, c.Count)
}
