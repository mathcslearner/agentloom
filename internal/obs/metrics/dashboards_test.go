package metrics_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/mathcslearner/agentloom/internal/obs/metrics"
)

// obsDir is the observability config tree, resolved from this package's
// directory (go test runs in the package dir).
const obsDir = "../../../deploy/observability"

// obsFiles are every dashboard and alert-rule artifact that references
// metric names. A new dashboard or rules file must be added here to stay
// under the audit.
var obsFiles = []string{
	"grafana/dashboards/engine.json",
	"grafana/dashboards/api.json",
	"prometheus-rules.yml",
	"prometheus-rules.test.yml",
}

// TestDashboardsAndRulesReferenceRegisteredMetrics is 7.5's anti-drift
// audit: every engine_* metric name mentioned anywhere in the provisioned
// dashboards or the alert rules (PromQL, synthetic test series, and prose
// descriptions alike) must be an instrument actually registered by
// NewWorkerMetrics/NewAPIMetrics/NewRegistry. Renaming or dropping a
// metric in instruments.go now fails this test instead of silently
// blanking a panel or muting an alert.
func TestDashboardsAndRulesReferenceRegisteredMetrics(t *testing.T) {
	t.Parallel()

	// Gather the registered families exactly as a deployable exposes them:
	// the build-info registry plus both instrument sets, every vec child
	// touched so Gather sees each family.
	reg := metrics.NewRegistry(metrics.ServiceWorker)
	exercise(metrics.NewWorkerMetrics(reg), metrics.NewAPIMetrics(reg))
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	registered := make(map[string]bool, len(families))
	for _, fam := range families {
		registered[fam.GetName()] = true
	}

	// Metric references: the engine_ namespace plus word characters.
	nameRE := regexp.MustCompile(`\bengine_[a-z0-9_]+`)

	for _, rel := range obsFiles {
		path := filepath.Join(obsDir, rel)
		raw, err := os.ReadFile(path) // #nosec G304 -- fixed repo-relative paths
		if err != nil {
			t.Fatalf("reading %s: %v", rel, err)
		}
		refs := nameRE.FindAllString(string(raw), -1)
		if len(refs) == 0 {
			t.Errorf("%s references no engine_* metrics — wrong file under audit?", rel)
		}
		seen := map[string]bool{}
		for _, ref := range refs {
			if seen[ref] {
				continue
			}
			seen[ref] = true
			if !registered[normalizeMetricRef(ref)] {
				t.Errorf("%s references %q, which is not a registered instrument (see instruments.go)", rel, ref)
			}
		}
	}
}

// normalizeMetricRef maps a referenced series name to its metric family:
// histogram references carry the _bucket/_count/_sum suffixes the family
// name does not.
func normalizeMetricRef(ref string) string {
	for _, suffix := range []string{"_bucket", "_count", "_sum"} {
		if base, ok := strings.CutSuffix(ref, suffix); ok {
			return base
		}
	}
	return ref
}
