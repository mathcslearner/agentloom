package tokens

import (
	"fmt"
	"strings"
	"sync"

	tiktoken "github.com/pkoukk/tiktoken-go"
	tiktokenloader "github.com/pkoukk/tiktoken-go-loader"
)

// The two BPE encodings the OpenAI and Anthropic counters use. o200k_base is
// the current OpenAI families' encoding (gpt-4o/4.1, gpt-5, the o-series) and
// the closest public reference for the Anthropic estimate; cl100k_base is the
// legacy gpt-4/gpt-3.5 encoding.
const (
	encO200kBase  = "o200k_base"
	encCL100kBase = "cl100k_base"
)

// offlineLoaderOnce installs the offline, go:embed'd BPE-rank loader exactly
// once per process. Without it tiktoken downloads rank tables from the network
// on first use; ADR-014 requires counting to be offline, so the embedded
// tables (github.com/pkoukk/tiktoken-go-loader) are the only source. The test
// suite proves no network call is made via an http.DefaultTransport tripwire.
var offlineLoaderOnce sync.Once

func installOfflineLoader() {
	offlineLoaderOnce.Do(func() {
		tiktoken.SetBpeLoader(tiktokenloader.NewOfflineLoader())
	})
}

// encodingCache memoizes the (expensive) BPE construction per encoding name so
// each encoding is built once per process and shared across counters.
var (
	encodingCacheMu sync.Mutex
	encodingCache   = map[string]*tiktoken.Tiktoken{}
)

// getEncoding returns the memoized tiktoken encoding for name, building it (via
// the offline loader) on first use. The offline rank tables are embedded, so
// the only error path is an unknown encoding name — a programming error, since
// callers pass the two constants above.
func getEncoding(name string) (*tiktoken.Tiktoken, error) {
	encodingCacheMu.Lock()
	defer encodingCacheMu.Unlock()
	if enc, ok := encodingCache[name]; ok {
		return enc, nil
	}
	installOfflineLoader()
	enc, err := tiktoken.GetEncoding(name)
	if err != nil {
		return nil, fmt.Errorf("tokens: loading BPE encoding %q: %w", name, err)
	}
	encodingCache[name] = enc
	return enc, nil
}

// bpeCount counts tokens in text with the given encoding using EncodeOrdinary
// (no special-token handling — content is plain data, never control tokens).
func bpeCount(enc *tiktoken.Tiktoken, text string) int {
	if text == "" {
		return 0
	}
	return len(enc.EncodeOrdinary(text))
}

// openAIEncodingFor selects the BPE encoding for an OpenAI model by prefix.
// The current families (gpt-4o, gpt-4.1, gpt-4.5, gpt-5, and the o1/o3/o4
// reasoning series) use o200k_base; the legacy gpt-4 and gpt-3.5 families use
// cl100k_base. An unrecognized OpenAI model defaults to o200k_base (the
// current encoding), so a newly-released model counts sensibly until this
// table learns it. tiktoken's own MODEL_TO_ENCODING map is not used because it
// predates gpt-5 and the o-series.
func openAIEncodingFor(model string) string {
	m := strings.ToLower(model)
	// Legacy cl100k families first: "gpt-4" would otherwise be shadowed by a
	// broad "gpt-" rule, and "gpt-4o"/"gpt-4.1" must NOT match legacy gpt-4.
	switch {
	case strings.HasPrefix(m, "gpt-4o"),
		strings.HasPrefix(m, "gpt-4.1"),
		strings.HasPrefix(m, "gpt-4.5"),
		strings.HasPrefix(m, "gpt-5"),
		strings.HasPrefix(m, "chatgpt-4o"),
		strings.HasPrefix(m, "o1"),
		strings.HasPrefix(m, "o3"),
		strings.HasPrefix(m, "o4"):
		return encO200kBase
	case strings.HasPrefix(m, "gpt-4"),
		strings.HasPrefix(m, "gpt-3.5"):
		return encCL100kBase
	default:
		return encO200kBase
	}
}
