package retrieval_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mathcslearner/agentloom/internal/plugin"
	"github.com/mathcslearner/agentloom/internal/retrieval"
)

// fakeRetriever is a minimal Retriever with a settable manifest, for
// registry-rule coverage.
type fakeRetriever struct {
	manifest plugin.Manifest
}

func (f fakeRetriever) Manifest() plugin.Manifest { return f.manifest }

func (f fakeRetriever) Ingest(context.Context, []retrieval.Doc) error { return nil }

func (f fakeRetriever) Query(context.Context, string, int) ([]retrieval.ScoredDoc, error) {
	return nil, nil
}

func retrieverManifest(name string) plugin.Manifest {
	return plugin.Manifest{Kind: plugin.KindRetriever, Name: name, Version: "1.0.0"}
}

func TestRegistryRegisterAndGet(t *testing.T) {
	t.Parallel()
	reg, err := retrieval.NewRegistry(fakeRetriever{manifest: retrieverManifest("pg_fulltext")})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if _, err := reg.Get("pg_fulltext"); err != nil {
		t.Errorf("Get(pg_fulltext) = %v, want the registered retriever", err)
	}
	if names := reg.Names(); len(names) != 1 || names[0] != "pg_fulltext" {
		t.Errorf("Names() = %v, want [pg_fulltext]", names)
	}
	if ms := reg.Manifests(); len(ms) != 1 || ms[0].Kind != plugin.KindRetriever {
		t.Errorf("Manifests() = %+v, want one retriever manifest", ms)
	}
}

func TestRegistryUnknownRetriever(t *testing.T) {
	t.Parallel()
	reg, _ := retrieval.NewRegistry()
	_, err := reg.Get("missing")
	if !errors.Is(err, retrieval.ErrUnknownRetriever) {
		t.Fatalf("Get(missing) = %v, want ErrUnknownRetriever", err)
	}
	var ure *retrieval.UnknownRetrieverError
	if !errors.As(err, &ure) || ure.Name != "missing" {
		t.Errorf("err = %v, want *UnknownRetrieverError{Name: missing}", err)
	}
}

func TestRegistryRejectsBadRegistrations(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		reg     retrieval.Retriever
		wantSub string
	}{
		{"nil retriever", nil, "nil retriever"},
		{"wrong kind", fakeRetriever{manifest: plugin.Manifest{Kind: plugin.KindTool, Name: "x", Version: "1.0.0"}}, "must register as"},
		{"invalid manifest", fakeRetriever{manifest: plugin.Manifest{Kind: plugin.KindRetriever, Name: "X", Version: "1.0.0"}}, "registering retriever"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := retrieval.NewRegistry(tc.reg)
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("NewRegistry error = %v, want substring %q", err, tc.wantSub)
			}
		})
	}
}

func TestRegistryRejectsDuplicate(t *testing.T) {
	t.Parallel()
	_, err := retrieval.NewRegistry(
		fakeRetriever{manifest: retrieverManifest("dup")},
		fakeRetriever{manifest: retrieverManifest("dup")},
	)
	if err == nil || !strings.Contains(err.Error(), "registered twice") {
		t.Errorf("NewRegistry error = %v, want a duplicate rejection", err)
	}
}
