package plugin_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/mathcslearner/agentloom/internal/plugin"
)

// valid returns a minimal valid manifest to mutate per case.
func valid() plugin.Manifest {
	return plugin.Manifest{
		Kind:    plugin.KindExecutor,
		Name:    "echo",
		Version: "1.0.0",
	}
}

func TestManifestValidate(t *testing.T) {
	t.Parallel()

	if err := valid().Validate(); err != nil {
		t.Fatalf("valid manifest rejected: %v", err)
	}
	withSchema := valid()
	withSchema.ConfigSchema = json.RawMessage(`{"type":"object"}`)
	if err := withSchema.Validate(); err != nil {
		t.Fatalf("manifest with object schema rejected: %v", err)
	}
	prerelease := valid()
	prerelease.Version = "0.1.0-stub"
	if err := prerelease.Validate(); err != nil {
		t.Fatalf("pre-release version rejected: %v", err)
	}

	cases := []struct {
		name    string
		mutate  func(*plugin.Manifest)
		wantSub string
	}{
		{"zero kind", func(m *plugin.Manifest) { m.Kind = "" }, "not a plugin kind"},
		{"unknown kind", func(m *plugin.Manifest) { m.Kind = "middleware" }, "not a plugin kind"},
		{"empty name", func(m *plugin.Manifest) { m.Name = "" }, "name is empty"},
		{"uppercase name", func(m *plugin.Manifest) { m.Name = "Echo" }, "does not match"},
		{"leading digit", func(m *plugin.Manifest) { m.Name = "1echo" }, "does not match"},
		{"hyphenated name", func(m *plugin.Manifest) { m.Name = "http-request" }, "does not match"},
		{"overlong name", func(m *plugin.Manifest) { m.Name = strings.Repeat("a", 65) }, "exceeds 64"},
		{"empty version", func(m *plugin.Manifest) { m.Version = "" }, "not a semver"},
		{"two-part version", func(m *plugin.Manifest) { m.Version = "1.0" }, "not a semver"},
		{"v-prefixed version", func(m *plugin.Manifest) { m.Version = "v1.0.0" }, "not a semver"},
		{"schema not JSON", func(m *plugin.Manifest) { m.ConfigSchema = json.RawMessage(`{`) }, "not a JSON object"},
		{"schema not an object", func(m *plugin.Manifest) { m.ConfigSchema = json.RawMessage(`true`) }, "not a JSON object"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := valid()
			tc.mutate(&m)
			err := m.Validate()
			if err == nil {
				t.Fatalf("manifest %+v validated — want error containing %q", m, tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error %q does not mention %q", err, tc.wantSub)
			}
		})
	}
}

func TestRegistryRegisterRejections(t *testing.T) {
	t.Parallel()

	r := plugin.NewRegistry()
	if err := r.Register(valid(), struct{}{}); err != nil {
		t.Fatalf("first registration failed: %v", err)
	}

	err := r.Register(valid(), struct{}{})
	var dup *plugin.DuplicateError
	if !errors.As(err, &dup) || !errors.Is(err, plugin.ErrDuplicate) {
		t.Fatalf("duplicate registration returned %v — want *DuplicateError unwrapping ErrDuplicate", err)
	}
	if dup.Kind != plugin.KindExecutor || dup.Name != "echo" {
		t.Fatalf("DuplicateError identifies %s %q — want executor \"echo\"", dup.Kind, dup.Name)
	}

	bad := valid()
	bad.Version = "not-a-version"
	err = r.Register(bad, struct{}{})
	var inv *plugin.InvalidManifestError
	if !errors.As(err, &inv) || !errors.Is(err, plugin.ErrInvalidManifest) {
		t.Fatalf("invalid manifest returned %v — want *InvalidManifestError unwrapping ErrInvalidManifest", err)
	}

	err = r.Register(plugin.Manifest{Kind: plugin.KindTool, Name: "echo", Version: "1.0.0"}, nil)
	if !errors.As(err, &inv) {
		t.Fatalf("nil impl returned %v — want *InvalidManifestError", err)
	}
}

func TestRegistryCrossKindNamesAndLookup(t *testing.T) {
	t.Parallel()

	r := plugin.NewRegistry()
	execImpl, toolImpl := "exec-impl", "tool-impl"
	if err := r.Register(valid(), execImpl); err != nil {
		t.Fatalf("executor registration: %v", err)
	}
	// The same name under a different kind is a distinct identity, not a
	// duplicate (ADR-009: names are unique per kind).
	toolManifest := plugin.Manifest{Kind: plugin.KindTool, Name: "echo", Version: "2.0.0"}
	if err := r.Register(toolManifest, toolImpl); err != nil {
		t.Fatalf("same name under another kind rejected: %v", err)
	}

	impl, m, ok := r.Lookup(plugin.KindTool, "echo")
	if !ok || impl != toolImpl || m.Version != "2.0.0" {
		t.Fatalf("Lookup(tool, echo) = %v, %+v, %v — want the tool registration", impl, m, ok)
	}
	if _, _, ok := r.Lookup(plugin.KindRetriever, "echo"); ok {
		t.Fatal("Lookup on an unregistered kind reported ok")
	}
	if _, _, ok := r.Lookup(plugin.KindExecutor, "missing"); ok {
		t.Fatal("Lookup on an unregistered name reported ok")
	}
}

func TestRegistryManifestsSorted(t *testing.T) {
	t.Parallel()

	r := plugin.NewRegistry()
	// Registered deliberately out of listing order.
	for _, m := range []plugin.Manifest{
		{Kind: plugin.KindTool, Name: "json_transform", Version: "1.0.0"},
		{Kind: plugin.KindExecutor, Name: "sleep", Version: "1.0.0"},
		{Kind: plugin.KindExecutor, Name: "echo", Version: "1.0.0"},
		{Kind: plugin.KindModelProvider, Name: "anthropic", Version: "1.0.0"},
	} {
		if err := r.Register(m, struct{}{}); err != nil {
			t.Fatalf("registering %s %s: %v", m.Kind, m.Name, err)
		}
	}
	var got []string
	for _, m := range r.Manifests() {
		got = append(got, string(m.Kind)+"/"+m.Name)
	}
	want := []string{"executor/echo", "executor/sleep", "tool/json_transform", "model_provider/anthropic"}
	if len(got) != len(want) {
		t.Fatalf("Manifests() returned %v — want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Manifests() order %v — want %v", got, want)
		}
	}

	byKind := r.ManifestsByKind(plugin.KindExecutor)
	if len(byKind) != 2 || byKind[0].Name != "echo" || byKind[1].Name != "sleep" {
		t.Fatalf("ManifestsByKind(executor) = %+v — want echo, sleep", byKind)
	}
	if got := r.ManifestsByKind(plugin.KindValidator); len(got) != 0 {
		t.Fatalf("ManifestsByKind on an empty kind = %+v — want empty", got)
	}
}

func TestKinds(t *testing.T) {
	t.Parallel()

	ks := plugin.Kinds()
	if len(ks) != 5 {
		t.Fatalf("Kinds() has %d entries — want 5", len(ks))
	}
	for _, k := range ks {
		if !k.Valid() {
			t.Errorf("listed kind %q reports invalid", k)
		}
	}
	if plugin.Kind("executor ").Valid() || plugin.Kind("").Valid() {
		t.Error("malformed kinds report valid")
	}
}
